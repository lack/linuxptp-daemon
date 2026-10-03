package ublox

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/golang/glog"
)

const (
	// GNSSDeviceSysfsTemplate is the sysfs path template for finding GNSS
	// devices attached to a network interface.
	GNSSDeviceSysfsTemplate = "/sys/class/net/%s/device/gnss"

	// ttyClassSysfsPath contains the sysfs class entries for tty devices.
	ttyClassSysfsPath = "/sys/class/tty"
)

var (
	// netClassSysfsPath contains the sysfs class entries for network devices.
	netClassSysfsPath = "/sys/class/net"

	// pciSlotsSysfsPath contains firmware-reported PCI hotplug slot mappings.
	pciSlotsSysfsPath = "/sys/bus/pci/slots"

	// ReadDir is the function used to read sysfs directories. Replace in tests
	// to mock filesystem access.
	ReadDir = os.ReadDir

	// permanentMACAddress returns the permanent hardware address reported by
	// ethtool. Replace in tests to avoid requiring a physical NIC.
	getPermanentMACAddress = readPermanentMACAddress
)

// GNSSDeviceFromInterface resolves the GNSS TTY device path for a given
// network interface by reading the sysfs directory
// /sys/class/net/<iface>/device/gnss/.
func GNSSDeviceFromInterface(iface string) (string, error) {
	glog.Infof("Looking for GNSS device associated with iface %s", iface)
	gnssDir := fmt.Sprintf(GNSSDeviceSysfsTemplate, iface)
	entries, err := ReadDir(gnssDir)
	if err != nil {
		return "", fmt.Errorf("no GNSS device found for interface %s: %w", iface, err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("GNSS sysfs directory %s is empty", gnssDir)
	}
	if len(entries) > 1 {
		// Sort for deterministic selection when multiple devices exist
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name() < entries[j].Name()
		})
		glog.Warningf("multiple GNSS devices found for %s, using %s", iface, entries[0].Name())
	}
	result := fmt.Sprintf("/dev/%s", entries[0].Name())
	glog.Infof("Detected GNSS device %s", result)
	return result, nil
}

// GNSSDeviceFromEthernetDevice resolves a GNSS device attached to an Ethernet
// device. Every supplied selector is applied as an AND criterion. A name-only
// selector uses a direct interface lookup; selectors that identify hardware
// are resolved against the interface's sysfs device information.
func GNSSDeviceFromEthernetDevice(name, pciAddress, permanentMAC, slot string) (string, error) {
	if name == "" && pciAddress == "" && permanentMAC == "" && slot == "" {
		return "", fmt.Errorf("EthernetDevice has no selection criteria")
	}

	if pciAddress != "" {
		var err error
		pciAddress, err = normalizePCIAddress(pciAddress)
		if err != nil {
			return "", err
		}
	}
	if permanentMAC != "" {
		address, err := net.ParseMAC(strings.TrimSpace(permanentMAC))
		if err != nil || len(address) != 6 {
			return "", fmt.Errorf("invalid permanent MAC address %q", permanentMAC)
		}
		permanentMAC = strings.ToLower(address.String())
	}
	if slot != "" {
		normalized, err := normalizePCIPosition(slot)
		if err != nil {
			return "", fmt.Errorf("invalid PCI slot ID %q: %w", slot, err)
		}
		slot = normalized
	}

	if name != "" && pciAddress == "" && permanentMAC == "" && slot == "" {
		return GNSSDeviceFromInterface(name)
	}

	entries, err := ReadDir(netClassSysfsPath)
	if err != nil {
		return "", fmt.Errorf("cannot enumerate Ethernet devices: %w", err)
	}
	interfaces := make([]string, 0, len(entries))
	for _, entry := range entries {
		if ethernetInterfaceMatches(entry.Name(), name, pciAddress, permanentMAC, slot) {
			interfaces = append(interfaces, entry.Name())
		}
	}
	return gnssDeviceFromInterfaces(interfaces, "EthernetDevice")
}

func ethernetInterfaceMatches(iface, name, pciAddress, permanentMAC, slot string) bool {
	if name != "" && iface != name {
		return false
	}
	if pciAddress == "" && permanentMAC == "" && slot == "" {
		return true
	}

	devicePath, err := filepath.EvalSymlinks(filepath.Join(netClassSysfsPath, iface, "device"))
	if err != nil {
		return false
	}
	if pciAddress != "" && !strings.EqualFold(filepath.Base(devicePath), pciAddress) {
		return false
	}
	if permanentMAC != "" {
		actual, err := getPermanentMACAddress(iface)
		if err != nil || !strings.EqualFold(actual, permanentMAC) {
			return false
		}
	}
	if slot != "" {
		actualSlot, err := pciSlotID(devicePath)
		if err != nil || actualSlot != slot {
			return false
		}
	}
	return true
}

func readPermanentMACAddress(iface string) (string, error) {
	output, err := exec.Command("ethtool", "-P", iface).Output()
	if err != nil {
		return "", fmt.Errorf("cannot read permanent MAC address for %s: %w", iface, err)
	}
	const prefix = "Permanent address:"
	for _, line := range strings.Split(string(output), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			address, err := net.ParseMAC(strings.TrimSpace(value))
			if err != nil || len(address) != 6 {
				return "", fmt.Errorf("invalid permanent MAC address reported for %s", iface)
			}
			return strings.ToLower(address.String()), nil
		}
	}
	return "", fmt.Errorf("ethtool returned no permanent MAC address for %s", iface)
}

// pciSlotID returns the slot number used by systemd's PCI slot-based naming.
// Newer systems expose the ACPI _SUN value through firmware_node/sun. Older
// systems expose slot-to-device mappings below /sys/bus/pci/slots.
func pciSlotID(devicePath string) (string, error) {
	for path := devicePath; path != "." && path != string(filepath.Separator); path = filepath.Dir(path) {
		if value, err := os.ReadFile(filepath.Join(path, "firmware_node", "sun")); err == nil {
			if slot, err := normalizePCIPosition(string(value)); err == nil {
				return slot, nil
			}
		}

		pciAddress := filepath.Base(path)
		if !strings.Contains(pciAddress, ":") || !strings.Contains(pciAddress, ".") {
			continue
		}
		entries, err := ReadDir(pciSlotsSysfsPath)
		if err != nil {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			address, err := os.ReadFile(filepath.Join(pciSlotsSysfsPath, entry.Name(), "address"))
			if err != nil {
				continue
			}
			slotAddress := strings.TrimSpace(string(address))
			if strings.Count(slotAddress, ":") == 1 {
				slotAddress = "0000:" + slotAddress
			}
			if slotAddress != "" && strings.HasPrefix(pciAddress, slotAddress) {
				if _, err := normalizePCIPosition(entry.Name()); err == nil {
					return normalizePCIPosition(entry.Name())
				}
			}
		}
	}
	return "", fmt.Errorf("no firmware-reported PCI slot for %s", filepath.Base(devicePath))
}

// normalizePCIPosition canonicalizes a decimal PCI slot or function number.
func normalizePCIPosition(position string) (string, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(position), 10, 32)
	if err != nil {
		return "", fmt.Errorf("must be a non-negative decimal number")
	}
	return strconv.FormatUint(value, 10), nil
}

func gnssDeviceFromInterfaces(interfaces []string, selector string) (string, error) {
	sort.Strings(interfaces)
	var candidates []string
	for _, iface := range interfaces {
		device, err := GNSSDeviceFromInterface(iface)
		if err == nil {
			candidates = append(candidates, device)
		}
	}

	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no GNSS device found for %s", selector)
	case 1:
		return candidates[0], nil
	default:
		return "", fmt.Errorf("multiple GNSS devices found for %s: %s", selector, strings.Join(candidates, ", "))
	}
}

// GNSSDeviceFromACPIDevice resolves the tty exposed by an ACPI-enumerated
// serial controller. It walks each tty's sysfs ancestry and matches an ACPI
// device name such as INTC10EE:00, rather than relying on the dynamically
// assigned tty name (for example, ttyS2).
func GNSSDeviceFromACPIDevice(hid, uid string) (string, error) {
	hid = strings.TrimSpace(hid)
	uid = strings.TrimSpace(uid)
	if hid == "" {
		return "", fmt.Errorf("invalid ACPI hardware ID: must not be empty")
	}
	return findTTYFromACPIDevice(ttyClassSysfsPath, hid, uid)
}

func findTTYFromACPIDevice(ttyClassPath, hid, uid string) (string, error) {
	selector := hid
	if uid != "" {
		selector += ":" + uid
	}
	glog.Infof("Looking for GNSS tty exposed by ACPI serial device %s", selector)
	entries, err := ReadDir(ttyClassPath)
	if err != nil {
		return "", fmt.Errorf("cannot enumerate tty devices: %w", err)
	}

	var candidates []string
	for _, entry := range entries {
		devicePath := filepath.Join(ttyClassPath, entry.Name(), "device")
		resolvedDevicePath, err := filepath.EvalSymlinks(devicePath)
		if err != nil {
			// Virtual tty devices and stale class entries may not have a
			// resolvable device path.
			continue
		}
		if acpiDeviceMatches(resolvedDevicePath, hid, uid) {
			candidates = append(candidates, filepath.Join("/dev", entry.Name()))
		}
	}

	sort.Strings(candidates)
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no tty device found for ACPI serial device %s", selector)
	case 1:
		glog.Infof("Detected GNSS device %s", candidates[0])
		return candidates[0], nil
	default:
		return "", fmt.Errorf("multiple tty devices found for ACPI serial device %s: %s",
			selector, strings.Join(candidates, ", "))
	}
}

// acpiDeviceMatches walks from a tty's sysfs device to its parents, looking
// for an ACPI device directory named <HID>:<UID>. If uid is empty, any
// instance of the requested HID matches.
func acpiDeviceMatches(devicePath, hid, uid string) bool {
	for path := devicePath; path != "." && path != string(filepath.Separator); path = filepath.Dir(path) {
		name := filepath.Base(path)
		parts := strings.SplitN(name, ":", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], hid) {
			continue
		}
		if uid == "" || strings.EqualFold(parts[1], uid) {
			return true
		}
	}
	return false
}

// GNSSDeviceFromUSB resolves the tty device exposed by a USB device with the
// given hexadecimal vendor and product IDs, optionally constrained by its USB
// bus-port topology path. It enumerates tty class devices and walks their sysfs
// ancestry, rather than relying on an unstable tty name such as ttyACM0.
func GNSSDeviceFromUSB(vendor, product, topologyPath string) (string, error) {
	rawVendor, rawProduct := vendor, product
	vendor, err := normalizeUSBID(vendor)
	if err != nil {
		return "", fmt.Errorf("invalid USB vendor ID %q: %w", rawVendor, err)
	}
	product, err = normalizeUSBID(product)
	if err != nil {
		return "", fmt.Errorf("invalid USB product ID %q: %w", rawProduct, err)
	}
	topologyPath = strings.TrimSpace(topologyPath)
	if topologyPath != "" && !validUSBTopologyPath(topologyPath) {
		return "", fmt.Errorf("invalid USB topology path %q", topologyPath)
	}

	return findTTYFromUSBDevice(ttyClassSysfsPath, vendor, product, topologyPath)
}

func findTTYFromUSBDevice(ttyClassPath, vendor, product, topologyPath string) (string, error) {
	glog.Infof("Looking for GNSS tty exposed by USB device %s:%s at path %q", vendor, product, topologyPath)
	entries, err := ReadDir(ttyClassPath)
	if err != nil {
		return "", fmt.Errorf("cannot enumerate tty devices: %w", err)
	}

	var candidates []string
	for _, entry := range entries {
		devicePath := filepath.Join(ttyClassPath, entry.Name(), "device")
		resolvedDevicePath, err := filepath.EvalSymlinks(devicePath)
		if err != nil {
			// Virtual tty devices and stale class entries may not have a
			// resolvable device path.
			continue
		}
		if usbDeviceMatches(resolvedDevicePath, vendor, product, topologyPath) {
			candidates = append(candidates, filepath.Join("/dev", entry.Name()))
		}
	}

	sort.Strings(candidates)
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no tty device found for USB device %s:%s at path %q", vendor, product, topologyPath)
	case 1:
		glog.Infof("Detected GNSS device %s", candidates[0])
		return candidates[0], nil
	default:
		return "", fmt.Errorf("multiple tty devices found for USB device %s:%s at path %q: %s",
			vendor, product, topologyPath, strings.Join(candidates, ", "))
	}
}

// usbDeviceMatches walks from a tty's sysfs device to its parents, looking for
// a USB device node with matching IDs and, when provided, its bus-port path.
func usbDeviceMatches(devicePath, vendor, product, topologyPath string) bool {
	for sysfsPath := devicePath; sysfsPath != "." && sysfsPath != string(filepath.Separator); sysfsPath = filepath.Dir(sysfsPath) {
		subsystemPath, err := filepath.EvalSymlinks(filepath.Join(sysfsPath, "subsystem"))
		if err == nil && filepath.Base(subsystemPath) == "usb" {
			if topologyPath != "" && filepath.Base(sysfsPath) != topologyPath {
				continue
			}
			actualVendor, vendorErr := os.ReadFile(filepath.Join(sysfsPath, "idVendor"))
			actualProduct, productErr := os.ReadFile(filepath.Join(sysfsPath, "idProduct"))
			if vendorErr == nil && productErr == nil &&
				strings.EqualFold(strings.TrimSpace(string(actualVendor)), vendor) &&
				strings.EqualFold(strings.TrimSpace(string(actualProduct)), product) {
				return true
			}
		}
	}
	return false
}

func validUSBTopologyPath(path string) bool {
	parts := strings.Split(path, "-")
	if len(parts) != 2 || !decimalUSBPathComponent(parts[0]) {
		return false
	}
	for _, port := range strings.Split(parts[1], ".") {
		if !decimalUSBPathComponent(port) {
			return false
		}
	}
	return true
}

func decimalUSBPathComponent(value string) bool {
	if value == "" {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func normalizeUSBID(id string) (string, error) {
	return normalizePCIID(id)
}

func normalizePCIAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if filepath.Base(address) != address || address == "." || address == ".." {
		return "", fmt.Errorf("invalid PCI address %q", address)
	}
	if strings.Count(address, ":") == 1 {
		address = "0000:" + address
	}
	return address, nil
}

func normalizePCIID(id string) (string, error) {
	id = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(id), "0x"))
	if id == "" || len(id) > 4 {
		return "", fmt.Errorf("must be one to four hexadecimal digits")
	}
	value, err := strconv.ParseUint(id, 16, 16)
	if err != nil {
		return "", fmt.Errorf("must be hexadecimal: %w", err)
	}
	return fmt.Sprintf("%04x", value), nil
}
