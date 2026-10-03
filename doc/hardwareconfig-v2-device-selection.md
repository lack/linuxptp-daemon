# HardwareConfig v2 device selection

This document describes how GNSS device detection works and how HardwareConfig v2
selectors identify Ethernet, serial, and USB devices.

## GNSS matcher

`GNSSConfig.Match` currently supports exactly one of:

- `ttyDevice`
- `serialDevice`
- `ethernetDevice`
- `usbDevice`

Hardware-oriented Ethernet and USB lookup must resolve to exactly one usable
GNSS tty; no match or multiple matching tty devices is an error. `ttyDevice` is
returned as supplied. A name-only Ethernet lookup directly reads that
interface's `device/gnss/` directory; if that directory contains multiple GNSS
entries, the current implementation logs a warning and chooses the
lexicographically first entry. This is a legacy exception to strict uniqueness.

### Direct tty selection

When `ttyDevice` is specified, it is returned as provided. This is the most
explicit option, but paths such as `/dev/ttyACM0` can change after reboot or
hotplug events.

### Ethernet-device selection

`ethernetDevice` selects the Ethernet interface associated with the GNSS receiver.
It supports any Linux interface name, a PCI function address, a permanent MAC
address, and a firmware-reported PCI slot ID. For example, a Westport Channel
(E810) setup can select a NIC by its interface name:

```yaml
match:
  ethernetDevice:
    name: ens2f0
```

The name may be any current Linux interface name, including names such as
`eno8703`, `enp2s0`, and `ens2f0`. For hardware-oriented selection, use
`pciAddress`, `permanentMACAddress`, or `slot`:

```yaml
match:
  ethernetDevice:
    pciAddress: "0000:86:00.0"
    permanentMACAddress: "00:11:22:aa:bb:cc"
    slot: "2"
```

The fields mean:

- `pciAddress` is a PCI bus:device.function address (BDF), for example
  `0000:86:00.0`. A short address such as `86:00.0` is normalized to the full
  domain form. It identifies a PCI function, not a chassis slot.
- `permanentMACAddress` is the permanent hardware MAC address, in colon-separated
  form such as `00:11:22:aa:bb:cc`. The daemon reads it using `ethtool -P`;
  it does not use the possibly overridden current MAC address.
- `slot` is the decimal firmware-reported PCI slot ID used in systemd slot-based
  interface names. In `ens2f0`, the slot component is `2`; it is not the PCI
  bus number. Multiple PCI functions in one physical slot can share the slot ID.

Every supplied selector is combined with the others using AND semantics. A
name-only selector takes the direct `/sys/class/net/<name>/device/gnss/` lookup
path; when a name is combined with hardware selectors, those selectors are
checked against that named interface as well. Vendor and device IDs are not
Ethernet selectors because identical NICs can share them.

For hardware-oriented selector lookup, the daemon enumerates `/sys/class/net`,
resolves each interface's `device` symlink, and checks all requested criteria:
PCI address, permanent MAC, and/or slot. For `slot`, it first checks the
firmware `_SUN` value exposed through `firmware_node/sun`, then falls back to
PCI slot address mappings under `/sys/bus/pci/slots`. The matching interface's
`device/gnss/` directory is used to resolve the GNSS device node. A selector
with no matching GNSS device, or one
that resolves to multiple GNSS device nodes, returns an error rather than
silently choosing one.

### ACPI serial-device selection

`serialDevice` identifies an onboard serial controller by platform hardware
identity instead of by the dynamically assigned Linux tty name. The selector
currently supports ACPI identity:

```yaml
serialDevice:
  acpi:
    hid: INTC10EE
```

The detector enumerates `/sys/class/tty/*`, resolves each tty's `device`
symlink, and walks its parent directories looking for an ACPI device directory
named `<hid>:<uid>`. If `uid` is omitted, any instance of the HID may match.
Here `uid` means the Linux ACPI device instance suffix in the sysfs directory
name; it is not the value of the ACPI `_UID` attribute. The result is
`/dev/<tty-name>`, and zero or multiple matching ttys are errors.

For the HPE EL140, HPE documents the GNSS-connected controller as:

```text
ACPI device: INTC10EE:00
Linux driver: dw-apb-uart
Typical tty: /dev/ttyS2
```

The Linux name `ttyS2` is not used as the selector because serial numbering can
change when other UARTs are enumerated. The ACPI HID and UID identify the
controller; the HPE EL140 hardware profile supplies the platform-specific
knowledge that this controller is wired to the GNSS module. The driver name is
not required for matching because it is an implementation detail of the Linux
kernel.

The HPE EL140 profile therefore uses:

```yaml
match:
  serialDevice:
    acpi:
      hid: INTC10EE
```

This is a hardware identity match, not a generic proof that every system with
`INTC10EE:00` has a GNSS receiver attached.

#### Live EL140 verification

The selector was checked on a live EL140 node. Its tty and platform-device
relationships were:

```text
ttyS0 -> /sys/devices/pnp0/00:03
ttyS1 -> /sys/devices/pnp0/00:02
ttyS2 -> /sys/devices/platform/INTC10EE:00
ttyS3 -> /sys/devices/platform/serial8250
```

The matching device reported:

```text
/sys/devices/platform/INTC10EE:00/driver -> /sys/bus/platform/drivers/dw-apb-uart
/sys/devices/platform/INTC10EE:00/modalias = acpi:INTC10EE:
/dev/ttyS2 exists
/dev/ttyACM0 is absent
```

The ACPI sysfs entry also reported `uid = 1` at
`/sys/bus/acpi/devices/INTC10EE:00/uid`. This is distinct from the `:00`
Linux device-instance suffix in the platform-device name. The matcher uses the
platform-device ancestry and therefore deliberately matches `hid: INTC10EE`
without requiring either UID value. It does not need to inspect the UART driver
name; `dw-apb-uart` is recorded as a diagnostic confirmation rather than a
selection criterion.

The live result confirms that the selector resolves `/dev/ttyS2` without relying
on the dynamically assigned tty number. The serial stream itself was not read
during verification.

### USB-device selection

The GNR-D GNSS receiver is selected by its USB vendor and product IDs:

```yaml
match:
  usbDevice:
    vendor: "1546"
    product: "01a9"
```

If multiple identical receivers may be connected, specify the optional USB
bus-port topology `path` as an additional selector:

```yaml
match:
  usbDevice:
    vendor: "1546"
    product: "01a9"
    path: "2-1.4"
```

The path is the bus-and-port chain shown in sysfs, such as the `2-1.4` device
node in `/sys/devices/.../usb2/2-1/2-1.4`. It identifies where the receiver is
connected, not the receiver itself, so it changes if the device is moved or the
USB topology changes. Vendor, product, and path are all required to match when
`path` is supplied.

The detector does not depend on the tty name or on the `ttyACM`/`ttyUSB` driver
name. It performs the following sysfs traversal:

1. Enumerate `/sys/class/tty/*`.
2. Ignore entries without a resolvable `device` symlink. This excludes virtual
   tty devices and stale class entries.
3. Resolve `/sys/class/tty/<tty>/device`.
4. Walk the resolved device's parent directories.
5. Identify USB device ancestors by their `subsystem` symlink and read
   `idVendor` and `idProduct`.
6. Compare the normalized hexadecimal IDs with the requested selector and, when
   `path` is specified, require the USB device node's bus-port path to match.
7. Return `/dev/<tty-name>` if exactly one tty matches.

On the GNR-D system the relevant topology is:

```text
/sys/class/tty/ttyACM0/device
  -> /sys/devices/.../usb1/1-4/1-4:1.0
  -> /sys/devices/.../usb1/1-4
```

The USB attributes are located at the `1-4` device node:

```text
/sys/devices/.../usb1/1-4/idVendor      = 1546
/sys/devices/.../usb1/1-4/idProduct     = 01a9
/sys/devices/.../usb1/1-4/manufacturer  = u-blox AG - www.u-blox.com
/sys/devices/.../usb1/1-4/product       = u-blox GNSS receiver
```

The result is `/dev/ttyACM0`. The device has multiple USB interfaces, but only
one currently produces a tty.
If multiple identical receivers are connected, the optional `path` narrows the
match to the receiver on that bus-port chain. If the selected receiver itself
exposes multiple matching tty nodes, resolution still fails with an ambiguity
error; the topology path does not select between interfaces of one USB device.

## SR-IOV comparison

The SR-IOV Network Operator's `SriovNetworkNodePolicy.spec.nicSelector` supports
these related criteria:

- `vendor`
- `deviceID`
- `pfNames`
- `rootDevices` (PCI addresses)
- `netFilter` for platform-specific network identity

Its documented guidance is to provide enough criteria to avoid unintended
matches. `rootDevices` must be combined with another identifying criterion,
and `pfNames` plus `rootDevices` must refer to the same device. A `netFilter`
can be used alone where the platform network identifier is unique.

HardwareConfig v2 currently uses a simpler model:

| SR-IOV concept | HardwareConfig v2 equivalent | Notes |
|---|---|---|
| `pfNames` | `ethernetDevice.name` | Direct Linux interface lookup |
| `rootDevices` | `ethernetDevice.pciAddress` | PCI function/BDF lookup |
| `vendor` / `deviceID` | None | Not sufficiently specific to identify an Ethernet device |
| `netFilter` | None | Platform-specific network identity is not currently needed |

The resolver applies every supplied Ethernet selector as an AND criterion. A
name-only selector is a direct interface lookup; with additional selectors, the
name identifies the candidate interface and the other fields further constrain
it. The resulting GNSS device node must be unique.

## Other HardwareConfig v2 Ethernet references

There are two additional places where HardwareConfig v2 represents Ethernet
hardware using interface names.

### 1. `Subsystem.DPLL.NetworkInterface`

```yaml
dpll:
  networkInterface: eno8703
```

This is the strongest candidate for a richer selector. It identifies the
interface used to derive or look up the DPLL clock ID and is used throughout the
daemon for:

- clock ID resolution;
- phase-adjustment and delay-compensation lookup;
- applying hardware defaults;
- resolving subsystem-level operations;
- fallback selection when an Ethernet port is present.

The current resolution precedence is roughly:

1. Explicit `dpll.networkInterface`.
2. A leading interface derived from a PTP port by following its PHC to the PCI
   device and then enumerating `/sys/bus/pci/devices/<pci>/net/`.
3. The first Ethernet port in `subsystem.ethernet[].ports`.
4. For some clock types, the first interface found in the PTP profile.

This existing PHC-to-PCI-to-network traversal is a useful precedent for adding
stable matching. A future API could add a `dpll.networkDevice` selector while
retaining a resolved `networkInterface` string internally. This would avoid
changing every downstream consumer at once.

**Assessment: high value, but broad implementation impact.** This field is the
best next candidate after GNSS matching because a renamed interface can cause
clock-ID and hardware-resolution failures.

### 2. `Subsystem.Ethernet[].Ports`

```yaml
ethernet:
  - ports:
      - eno8703
      - eno8803
```

This is a list rather than a single device. The first port is significant: it is
currently treated as the default interface when `dpll.networkInterface` is
omitted. The list is also used to associate multiple physical Ethernet ports
with one synchronization subsystem and to validate PTP receiver membership.

A richer selector could be useful for systems where predictable interface names
are unavailable, but it requires defining list semantics first:

- Does one selector resolve to one port or all ports on a PCI function?
- Does a PCI device with multiple ports expand to multiple interfaces?
- How is the default/first port selected?
- Can a selector match a PF while the list is intended to contain individual
  ports?
- How should explicit ordering be preserved?

**Assessment: potentially high value, but not a drop-in replacement.** This
should likely use a list of selectors or a separate `EthernetDeviceGroup` type,
not simply replace each string with one `EthernetDevice` object.

### 3. `SourceConfig.PTPTimeReceivers`

```yaml
sources:
  - sourceType: ptpTimeReceiver
    ptpTimeReceivers:
      - eno8703
```

These values identify ports used by the PTP behavior and are derived from the
PTP profile's `ptp4l` configuration in common cases. They are not purely
hardware selectors: they also correspond to runtime PTP interface sections and
must remain valid names for the generated/configured PTP process.

Replacing these strings with hardware selectors would require resolving the
selectors before generating the PTP configuration and carefully handling
multiple matches. It could be useful for a higher-level declarative API, but it
would blur the distinction between:

- identifying hardware; and
- naming a port in a PTP runtime configuration.

**Assessment: lower priority.** Keep these as resolved interface names for now.
If selector support is added later, resolve selectors during HardwareConfig
resolution and keep the runtime representation as interface names.

## Recommended direction

1. Keep the current `EthernetDevice` selector for GNSS.
2. Introduce a reusable internal Ethernet selector/resolver abstraction without
   immediately changing every API field.
3. Add a selector alongside `DPLL.NetworkInterface`, resolve it once to a
   concrete interface name, and keep downstream code name-based.
4. Address `Ethernet[].Ports` separately because it represents an ordered,
   potentially multi-port group.
5. Leave `PTPTimeReceivers` as concrete runtime interface names until selector
   expansion is integrated with PTP configuration generation.
6. For every selector, use AND semantics for supplied identity fields and fail
   on zero or multiple matches.

## References

- SR-IOV Network Operator node policy API: `SriovNetworkNodePolicy.spec.nicSelector`
  ([API reference](https://github.com/k8snetworkplumbingwg/sriov-network-operator/blob/master/doc/api/node-policies-api.md))
- OpenShift SR-IOV device selection documentation:
  [Configuring an SR-IOV network device](https://docs.redhat.com/en/documentation/openshift_container_platform/4.21/html/hardware_networks/configuring-sriov-device)
- HPE EL140 UART advisory:
  [a00157521en_us](https://support.hpe.com/hpesc/public/docDisplay?docId=a00157521en_us&docLocale=en_US)
- HardwareConfig v2 API: `../ptp-operator/api/v2alpha1/hardwareconfig_types.go`
