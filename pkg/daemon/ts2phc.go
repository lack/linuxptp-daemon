package daemon

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/golang/glog"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/config"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/parser"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/parser/constants"
)

var ts2phcOffsetUnmeasured = math.NaN()

// pollCycle groups the 'adding tstamp' lines for one PPS receipt by the
// seconds part of the log timestamp. Offsets consume individual timestamps;
// timeouts (or the next cycle) report the remaining managed clocks.
type pollCycle struct {
	// group is the nonfractional part of the ts2phc event timestamp (not the PHC timestamp).
	group int
	// tstamps is the list of collected PHC timestamps, collected one per log line
	tstamps []tstamp
	// counted represents whether, in the case of failure, this poll cycle has been counted to avoid double-counting if we receive multiple related timeout events
	counted bool
}

// tstamp represents indivdiual collected tstamp poll messages, which correspond to ts2phc observing a PPS event.
type tstamp struct {
	// tstamp is the actual PHC timestamp retrieved by ts2phc on the PPS event
	tstamp float64
	// device is the PHC device
	device string
	// iface is the associated leading interface
	iface string
	// role is the role of this PHC
	role parser.TS2PHCRole
	// handled means the sample was reported or intentionally skipped as a non-sink.
	// Reselecting a sink must not reopen an old skipped sample.
	handled bool
}

// Each ts2phc process owns its poll state and configured PHC roles.
type ts2phcPollState struct {
	// pollCycleLock is a mutex around the currentPollCycle
	pollCycleLock sync.RWMutex
	// currentPollCycle represents the current poll cycle
	currentPollCycle *pollCycle
	// This counts the consecutive number of poll cycles that have timed out by receiving one or more timout events
	pollTimeoutCount int
	// roles explicitly tracks each configured PHC. All start as sinks; runtime
	// selection logs change them to sinks, sources, or unmanaged devices.
	roles map[string]parser.TS2PHCRole
}

// pollGroupFromTimestamp pulls the nonfractional part of the given timestamp out to group related events together in a single 1s cycle
func pollGroupFromTimestamp(timestamp string) (int, error) {
	wholeSeconds, _, _ := strings.Cut(timestamp, ".")
	return strconv.Atoi(wholeSeconds)
}

// phcToIface retrieves the iface related to the given PHC device
func (p *ptpProcess) phcToIface(device string) (string, error) {
	iface := p.ifaces.GetPhcID2IFace(device)
	if iface == "" || iface == device {
		return "", fmt.Errorf("no configured interface associated with %s", device)
	}
	return iface, nil
}

// processTS2PhcEvent is the main entrypoint for all ts2phc events
// That is, all parser.PTPEvents with parser.PTPEvent.Ts2Phc set
// Any invalid events are silently ignored
func (p *ptpProcess) processTS2PhcEvent(ptpEvent *parser.PTPEvent) {
	event := ptpEvent.Ts2Phc
	if event == nil {
		return
	}

	var metrics []*parser.Metrics
	var err error
	switch {
	case event.Role != "":
		p.handlePHCSelection(event.PHCDevice, event.Role)
	case event.PollSuccessful:
		p.handleSuccessfulPollFor(ptpEvent.Iface)
	case event.PollTimeout:
		metrics, err = p.handlePollTimeout(event)
	default:
		metrics, err = p.handleAddedTstamp(event)
	}
	if err != nil {
		glog.Errorf("Failed to process ts2phc poll event for %s: %v", p.configName, err)
		return
	}
	for _, metric := range metrics {
		p.offset = metric.Offset
		processParsedMetrics(p, metric)
	}
}

// handlePHCSelection updates configured PHC roles without dropping timestamps.
// A role change applies to pending samples as well as future poll cycles.
func (p *ptpProcess) handlePHCSelection(device string, role parser.TS2PHCRole) {
	if role != parser.TS2PHCSink && role != parser.TS2PHCSource && role != parser.TS2PHCUnmanaged {
		return
	}
	s := &p.ts2phcPoll
	s.pollCycleLock.Lock()
	defer s.pollCycleLock.Unlock()
	if _, known := s.roles[device]; !known {
		glog.Infof("Ignoring unconfigured PTP device %s", device)
		return
	}
	s.roles[device] = role

	// Synchronize any tstamps already in the current poll cycle with this role change
	if s.currentPollCycle != nil {
		cycle := s.currentPollCycle
		for i := range cycle.tstamps {
			ts := &cycle.tstamps[i]
			if ts.device == device {
				ts.role = role
				if role != parser.TS2PHCSink {
					ts.handled = true
				}
			}
		}
	}
}

// handleAddedTstamp adds a new PHC timestamp to this cycle
// It will create a new cycle if the group is different than the current cycle (or if there is no cycle)
// Normally when closing down a prior cycle, all managed interfaces will have
// been handled by a successful poll, so any remainders should generate a
// FREERUN metric (as ts2phc has not adjusted the PHC offset)
func (p *ptpProcess) handleAddedTstamp(data *parser.Ts2PhcEvent) ([]*parser.Metrics, error) {
	group, err := pollGroupFromTimestamp(data.Timestamp)
	if err != nil {
		return nil, err
	}
	if data.PHCTstamp == nil || data.PHCDevice == "" {
		return nil, fmt.Errorf("ts2phc timestamp event is missing its PHC or timestamp")
	}

	ts := tstamp{
		tstamp: *data.PHCTstamp,
		device: data.PHCDevice,
	}

	s := &p.ts2phcPoll
	s.pollCycleLock.Lock()
	defer s.pollCycleLock.Unlock()
	iface, err := p.phcToIface(data.PHCDevice)
	if err != nil {
		glog.V(4).Infof("Ignoring unmanaged ts2phc clock: %v", err)
		ts.role = parser.TS2PHCUnmanaged
	} else {
		ts.iface = iface
		ts.role = s.roles[ts.device]
	}
	ts.handled = ts.role != parser.TS2PHCSink

	var metrics []*parser.Metrics
	if s.currentPollCycle != nil {
		if s.currentPollCycle.group == group {
			// Group match: We are adding another timestamp to the same group
			s.currentPollCycle.tstamps = append(s.currentPollCycle.tstamps, ts)
			return nil, nil
		}
		// Group mismatch: This is the first event of the next poll cycle: Close the prior poll cycle
		metrics = s.currentPollCycle.unhandledMetrics()
		metrics = append(metrics, s.currentPollCycle.missingMetrics(p.ifaces, s.roles)...)
	}
	s.currentPollCycle = &pollCycle{
		group:   group,
		tstamps: []tstamp{ts},
	}
	return metrics, nil
}

// handleSuccessfulPollFor marks the given interface's timestamp as handled, as
// ts2phc has successfully processed and adjusted for any measured offset.
func (p *ptpProcess) handleSuccessfulPollFor(iface string) {
	iface = p.ifaces.GetPhcID2IFace(iface)
	s := &p.ts2phcPoll
	s.pollCycleLock.Lock()
	defer s.pollCycleLock.Unlock()
	s.pollTimeoutCount = 0
	if s.currentPollCycle == nil {
		// No current poll cycle: we can ignore this result and will pick up the next one
		return
	}
	for i := range s.currentPollCycle.tstamps {
		ts := &s.currentPollCycle.tstamps[i]
		if ts.iface == iface {
			ts.handled = true
			return
		}
	}
	glog.V(4).Infof("Current ts2phc poll did not record iface %s", iface)
}

// handlePollTimeout sends FREERUN metrics for all currently-received tstamp
// values, since a timeout prevents procesing of any PHC offsets and the PHCs
// are not being adjusted.
func (p *ptpProcess) handlePollTimeout(data *parser.Ts2PhcEvent) ([]*parser.Metrics, error) {
	group, err := pollGroupFromTimestamp(data.Timestamp)
	if err != nil {
		return nil, err
	}
	s := &p.ts2phcPoll
	s.pollCycleLock.Lock()
	defer s.pollCycleLock.Unlock()
	if s.currentPollCycle == nil {
		// Not catastrophic - We will begin checking on the next cycle.
		glog.Warning("Ignoring unexpected ts2phc poll timeout (no poll was started)")
		return nil, nil
	}

	metrics := s.currentPollCycle.unhandledMetrics()
	if !s.currentPollCycle.counted {
		s.pollTimeoutCount++
		s.currentPollCycle.counted = true
	}
	if s.currentPollCycle.group != group {
		// A timeout after a seconds boundary still applies to the old group,
		// but the old cycle must not survive into another PPS receipt.
		s.currentPollCycle = nil
	}
	return metrics, nil
}

func (c *pollCycle) appendTimestamp(ts tstamp) {
	c.tstamps = append(c.tstamps, ts)
}

func (c *pollCycle) tstampsWithRole(role parser.TS2PHCRole) []tstamp {
	var matches []tstamp
	for _, ts := range c.tstamps {
		if ts.role == role {
			matches = append(matches, ts)
		}
	}
	return matches
}

// maxOffset returns the signed other-minus-target offset furthest from zero,
// in nanoseconds, or NaN if no other device has a timestamp.
func maxOffset(ts *tstamp, tslist []tstamp) float64 {
	computedMetric := false
	var offset float64
	for _, other := range tslist {
		if ts.device == other.device {
			// Skip self
			continue
		}
		delta := other.tstamp - ts.tstamp
		if !computedMetric || math.Abs(delta) > math.Abs(offset) {
			offset = delta
		}
		computedMetric = true
	}
	if computedMetric {
		return offset * 1e9
	}
	return ts2phcOffsetUnmeasured
}

// computeTSOffset returns an offset in nanoseconds from timestamps in seconds.
// With a source it returns the signed source-minus-target offset; otherwise it
// returns the largest absolute peer offset, or NaN if no measurement is possible.
func computeTSOffset(ts *tstamp, cycle *pollCycle) float64 {
	if cycle == nil || ts == nil {
		return ts2phcOffsetUnmeasured
	}

	// Ideal case: We have a definitive source to measure offset against
	offset := maxOffset(ts, cycle.tstampsWithRole(parser.TS2PHCSource))
	if !math.IsNaN(offset) {
		return offset
	}

	// Fallback: Find the maximum offset from all other timestamps
	return math.Abs(maxOffset(ts, cycle.tstamps))
}

// unhandledMetrics creates a FREERUN metric for any tstamp in this cycle that has not already been handled
func (c *pollCycle) unhandledMetrics() []*parser.Metrics {
	var metrics []*parser.Metrics
	for i := range c.tstamps {
		ts := &c.tstamps[i]
		if ts.handled || ts.role != parser.TS2PHCSink {
			// Skip handled or non-sink PHCs
			continue
		}
		offset := computeTSOffset(ts, c)
		metrics = append(metrics, &parser.Metrics{
			From:       constants.TS2PHC,
			Iface:      ts.iface,
			ClockState: constants.ClockStateFreeRun,
			Source:     constants.Master,
			Offset:     offset,
			MaxOffset:  offset,
		})
		ts.handled = true
	}
	return metrics
}

// missingMetrics constructs a FREERUN metric for any managed interface that was not present in this poll cycle
func (c *pollCycle) missingMetrics(ifaces config.IFaces, roles map[string]parser.TS2PHCRole) []*parser.Metrics {
	var metrics []*parser.Metrics
	for _, iface := range ifaces {
		if roles[iface.PhcId] != parser.TS2PHCSink {
			// Skip non-sink PHCs
			continue
		}
		if slices.ContainsFunc(c.tstamps, func(ts tstamp) bool {
			return ts.iface == iface.Name
		}) {
			// Skip interfaces with recorded PHCs
			continue
		}
		// We have no tstamp measurement for this missing interface, so we cannot measure the offset.
		metrics = append(metrics, &parser.Metrics{
			From:       constants.TS2PHC,
			Iface:      iface.Name,
			ClockState: constants.ClockStateFreeRun,
			Source:     constants.Master,
			Offset:     ts2phcOffsetUnmeasured,
			MaxOffset:  ts2phcOffsetUnmeasured,
		})
	}
	return metrics
}

// resetPollCycle initializes configured PHCs as sinks and clears partial polls.
// Called after configuration rendering and on process restart.
func (p *ptpProcess) resetPollCycle() {
	s := &p.ts2phcPoll
	s.pollCycleLock.Lock()
	defer s.pollCycleLock.Unlock()
	s.currentPollCycle = nil
	s.pollTimeoutCount = 0
	s.roles = make(map[string]parser.TS2PHCRole, len(p.ifaces))
	for _, iface := range p.ifaces {
		if iface.PhcId != "" {
			s.roles[iface.PhcId] = parser.TS2PHCSink
		}
	}
}

// TS2phcHasTimeouts reports whether this process has observed a poll timeout since its last offset.
func (p *ptpProcess) TS2phcHasTimeouts() bool {
	s := &p.ts2phcPoll
	s.pollCycleLock.RLock()
	defer s.pollCycleLock.RUnlock()
	return s.pollTimeoutCount > 0
}
