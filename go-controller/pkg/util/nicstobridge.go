// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package util

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"
)

const (
	ubuntuDefaultFile = "/etc/default/openvswitch-switch"
	rhelDefaultFile   = "/etc/default/openvswitch"
)

func GetBridgeName(iface string) string {
	return fmt.Sprintf("br%s", iface)
}

// getBridgePortsInterfaces returns a mapping of bridge brName ports to its interfaces
func getBridgePortsInterfaces(brName string) (map[string][]string, error) {
	stdout, stderr, err := RunOVSVsctl("list-ports", brName)
	if err != nil {
		return nil, fmt.Errorf("failed to get list of ports on bridge %q:, stderr: %q, error: %v",
			brName, stderr, err)
	}

	portsToInterfaces := make(map[string][]string)
	for _, port := range strings.Split(stdout, "\n") {
		stdout, stderr, err = RunOVSVsctl("get", "Port", port, "Interfaces")
		if err != nil {
			return nil, fmt.Errorf("failed to get port %q on bridge %q:, stderr: %q, error: %v",
				port, brName, stderr, err)

		}
		// remove brackets on list of interfaces
		ifaces := strings.TrimPrefix(strings.TrimSuffix(stdout, "]"), "[")
		portsToInterfaces[port] = strings.Split(ifaces, ",")
	}
	return portsToInterfaces, nil
}

// GetNicName returns the physical NIC name, given an OVS bridge name
// configured by NicToBridge()
func GetNicName(brName string) (string, error) {
	// Check for system type port (required to be set if using NetworkManager)
	var stdout, stderr string
	portsToInterfaces, err := getBridgePortsInterfaces(brName)
	if err != nil {
		return "", err
	}

	systemPorts := make([]string, 0)
	for port, ifaces := range portsToInterfaces {
		for _, iface := range ifaces {
			stdout, stderr, err = RunOVSVsctl("get", "Interface", strings.TrimSpace(iface), "Type")
			if err != nil {
				return "", fmt.Errorf("failed to get Interface %q Type on bridge %q:, stderr: %q, error: %v",
					iface, brName, stderr, err)

			}
			// If system Type we know this is the OVS port is the NIC
			if stdout == "system" {
				systemPorts = append(systemPorts, port)
			}
		}
	}
	if len(systemPorts) == 1 {
		return systemPorts[0], nil
	} else if len(systemPorts) > 1 {
		klog.Infof("Found more than one system Type ports on the OVS bridge %s, so skipping "+
			"this method of determining the uplink port", brName)
	}

	// Check for bridge-uplink to indicate the NIC
	stdout, stderr, err = RunOVSVsctl(
		"br-get-external-id", brName, "bridge-uplink")
	if err != nil {
		return "", fmt.Errorf("failed to get the bridge-uplink for the bridge %q:, stderr: %q, error: %v",
			brName, stderr, err)
	}
	if stdout == "" && strings.HasPrefix(brName, "br") {
		// This would happen if the bridge was created before the bridge-uplink
		// changes got integrated. Assuming naming format of "br<nic name>".
		return brName[len("br"):], nil
	}
	return stdout, nil
}

func saveIPAddress(oldLink, newLink netlink.Link, addrs []netlink.Addr) error {
	for i := range addrs {
		addr := addrs[i]

		if addr.IP.IsGlobalUnicast() {
			// Remove from oldLink
			if err := netLinkOps.AddrDel(oldLink, &addr); err != nil {
				klog.Errorf("Remove addr from %q failed: %v", oldLink.Attrs().Name, err)
				return err
			}

			// Add to newLink
			addr.Label = newLink.Attrs().Name
			if err := netLinkOps.AddrAdd(newLink, &addr); err != nil {
				klog.Errorf("Add addr %q to newLink %q failed: %v", addr.String(), addr.Label, err)
				return err
			}
			klog.Infof("Successfully saved addr %q to newLink %q", addr.String(), addr.Label)
		}
	}

	return netLinkOps.LinkSetUp(newLink)
}

// delAddRoute removes 'route' from 'oldLink' and moves to 'newLink'
func delAddRoute(oldLink, newLink netlink.Link, route netlink.Route) error {
	// Remove route from old interface
	if err := netLinkOps.RouteDel(&route); err != nil && !strings.Contains(err.Error(), "no such process") {
		klog.Errorf("Remove route from %q failed: %v", oldLink.Attrs().Name, err)
		return err
	}

	// Add route to newLink
	route.LinkIndex = newLink.Attrs().Index
	if err := netLinkOps.RouteAdd(&route); err != nil && !os.IsExist(err) {
		klog.Errorf("Add route to newLink %q failed: %v", newLink.Attrs().Name, err)
		return err
	}

	klog.Infof("Successfully saved route %q", route.String())
	return nil
}

func saveRoute(oldLink, newLink netlink.Link, routes []netlink.Route) error {
	for i := range routes {
		route := routes[i]

		// Handle routes for default gateway later.  This is a special case for
		// GCE where we have /32 IP addresses and we can't add the default
		// gateway before the route to the gateway.
		if IsNilOrAnyNetwork(route.Dst) && route.Gw != nil && route.LinkIndex > 0 {
			continue
		} else if route.Dst != nil && !route.Dst.IP.IsGlobalUnicast() {
			continue
		}

		err := delAddRoute(oldLink, newLink, route)
		if err != nil {
			return err
		}
	}

	// Now add the default gateway (if any) via this interface.
	for i := range routes {
		route := routes[i]
		if IsNilOrAnyNetwork(route.Dst) && route.Gw != nil && route.LinkIndex > 0 {
			// Remove route from 'oldLink' and move it to 'newLink'
			err := delAddRoute(oldLink, newLink, route)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func setupDefaultFile() {
	platform, err := runningPlatform()
	if err != nil {
		klog.Errorf("Failed to set OVS package default file (%v)", err)
		return
	}

	var defaultFile, text string
	if platform == ubuntu {
		defaultFile = ubuntuDefaultFile
		text = "OVS_CTL_OPTS=\"$OVS_CTL_OPTS --delete-transient-ports\""
	} else if platform == rhel {
		defaultFile = rhelDefaultFile
		text = "OPTIONS=--delete-transient-ports"
	} else {
		return
	}

	fileContents, err := os.ReadFile(defaultFile)
	if err != nil {
		klog.Warningf("Failed to parse file %s (%v)",
			defaultFile, err)
		return
	}

	ss := strings.Split(string(fileContents), "\n")
	for _, line := range ss {
		if strings.Contains(line, "--delete-transient-ports") {
			// Nothing to do
			return
		}
	}

	// The defaultFile does not contain '--delete-transient-ports' set.
	// We should set it.
	f, err := os.OpenFile(defaultFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		klog.Errorf("Failed to open %s to write (%v)", defaultFile, err)
		return
	}
	defer f.Close()

	if _, err = f.WriteString(text); err != nil {
		klog.Errorf("Failed to write to %s (%v)",
			defaultFile, err)
		return
	}
}

// NicToBridge creates a OVS bridge for the 'iface' and also moves the IP
// address and routes of 'iface' to OVS bridge.
// EnsureBridgeOwnsPortAddrs restores OVN-K's steady-state invariant that
// the OVS gateway bridge — not the host kernel slave port — carries the
// IP addresses and routes. Call this after detecting that an existing OVS
// bridge owns portName via `ovs-vsctl port-to-br`, before reading the
// bridge's IPs.
//
// The host netd (e.g. systemd-networkd applying netplan with the
// `vlans.<iface>.addresses` model) re-applies the configured IP to the
// kernel slave port on every boot. NicToBridge migrates that IP to the
// OVS bridge on first node-join, but OVS conf.db persists the bridge
// structure across reboots while the kernel-side migration has to be
// redone every boot. This function detects the bare-bridge state and
// re-runs the IP/route migration without recreating the bridge.
//
// No-op when the bridge already has any global-unicast IP (steady state)
// or when the port has no global-unicast IP to migrate (host netd not
// yet applied / DHCP pending). Unlike NicToBridge, this function does
// not create the bridge or attach the port; both must already exist
// (caller has verified via port-to-br).
//
// Bridge IPs are evaluated through IsGlobalUnicast(), which means
// link-local addresses OVN-K itself plumbs on the bridge (e.g. the
// 169.254/16 host masquerade) do not satisfy the "bridge already owns
// IPs" check — same filter used by resolveNextHopSelf, keeping
// repo-wide semantics consistent.
func EnsureBridgeOwnsPortAddrs(portName, bridgeName string) error {
	bridgeLink, err := netLinkOps.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("failed to look up bridge %q: %w", bridgeName, err)
	}
	bridgeAddrs, err := netLinkOps.AddrList(bridgeLink, syscall.AF_UNSPEC)
	if err != nil {
		return fmt.Errorf("failed to list addresses on bridge %q: %w", bridgeName, err)
	}
	for _, a := range bridgeAddrs {
		if a.IP.IsGlobalUnicast() {
			// steady state — bridge already owns at least one routable IP
			return nil
		}
	}

	portLink, err := netLinkOps.LinkByName(portName)
	if err != nil {
		return fmt.Errorf("failed to look up port %q: %w", portName, err)
	}
	portAddrs, err := netLinkOps.AddrList(portLink, syscall.AF_UNSPEC)
	if err != nil {
		return fmt.Errorf("failed to list addresses on port %q: %w", portName, err)
	}
	var portHasGlobalUnicast bool
	for _, a := range portAddrs {
		if a.IP.IsGlobalUnicast() {
			portHasGlobalUnicast = true
			break
		}
	}
	if !portHasGlobalUnicast {
		// Nothing to migrate. Stay silent on the return path so that the
		// downstream "no IPv4 address on interface <bridge>" error from
		// gateway init retains its more precise context; a warning here
		// leaves a breadcrumb that the helper ran without surprises.
		klog.Warningf("Bridge %q has no global-unicast IP and port %q has none to migrate; "+
			"skipping address re-migration. Downstream gateway init will surface the missing-IP error.",
			bridgeName, portName)
		return nil
	}

	portRoutes, err := netLinkOps.RouteList(portLink, syscall.AF_UNSPEC)
	if err != nil {
		return fmt.Errorf("failed to list routes on port %q: %w", portName, err)
	}

	klog.Infof("Bridge %q has no global-unicast IP; re-migrating addresses and routes from port %q "+
		"(typically triggered by host netd re-applying interface config across reboot)",
		bridgeName, portName)
	if err := saveIPAddress(portLink, bridgeLink, portAddrs); err != nil {
		return fmt.Errorf("failed to migrate IP addresses from port %q to bridge %q: %w", portName, bridgeName, err)
	}
	if err := saveRoute(portLink, bridgeLink, portRoutes); err != nil {
		return fmt.Errorf("failed to migrate routes from port %q to bridge %q: %w", portName, bridgeName, err)
	}
	return nil
}

func NicToBridge(iface string) (string, error) {
	ifaceLink, err := netLinkOps.LinkByName(iface)
	if err != nil {
		return "", err
	}

	bridge := GetBridgeName(iface)
	ovsArgs := []string{
		"--", "--may-exist", "add-br", bridge,
		"--", "br-set-external-id", bridge, "bridge-id", bridge,
		"--", "br-set-external-id", bridge, "bridge-uplink", iface,
		"--", "set", "bridge", bridge, "fail-mode=standalone",
		fmt.Sprintf("other_config:hwaddr=%s", ifaceLink.Attrs().HardwareAddr),
		"--", "--may-exist", "add-port", bridge, iface,
		"--", "set", "port", iface, "other-config:transient=true",
	}
	stdout, stderr, err := RunOVSVsctl(ovsArgs...)
	if err != nil {
		klog.Errorf("Failed to create OVS bridge, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return "", err
	}
	klog.Infof("Successfully created OVS bridge %q", bridge)

	setupDefaultFile()

	// Get ip addresses and routes before any real operations.
	family := syscall.AF_UNSPEC
	addrs, err := netLinkOps.AddrList(ifaceLink, family)
	if err != nil {
		return "", err
	}
	routes, err := netLinkOps.RouteList(ifaceLink, family)
	if err != nil {
		return "", err
	}

	bridgeLink, err := netLinkOps.LinkByName(bridge)
	if err != nil {
		return "", err
	}

	// save ip addresses to bridge.
	if err = saveIPAddress(ifaceLink, bridgeLink, addrs); err != nil {
		return "", err
	}

	// save routes to bridge.
	if err = saveRoute(ifaceLink, bridgeLink, routes); err != nil {
		return "", err
	}

	return bridge, nil
}

// BridgeToNic moves the IP address and routes of internal port of the bridge to
// underlying NIC interface and deletes the OVS bridge.
func BridgeToNic(bridge string) error {
	// Internal port is named same as the bridge
	bridgeLink, err := netLinkOps.LinkByName(bridge)
	if err != nil {
		return err
	}

	// Get ip addresses and routes before any real operations.
	family := syscall.AF_UNSPEC
	addrs, err := netLinkOps.AddrList(bridgeLink, family)
	if err != nil {
		return err
	}
	routes, err := netLinkOps.RouteList(bridgeLink, family)
	if err != nil {
		return err
	}

	nicName, err := GetNicName(bridge)
	if err != nil {
		return err
	}
	ifaceLink, err := netLinkOps.LinkByName(nicName)
	if err != nil {
		return err
	}

	// save ip addresses to iface.
	if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil {
		return err
	}

	// save routes to iface.
	if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil {
		return err
	}

	// for every bridge interface that is of type "patch", find the peer
	// interface and delete that interface from the integration bridge
	stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)
	if err != nil {
		klog.Errorf("Failed to get interfaces for OVS bridge: %q, "+
			"stderr: %q, error: %v", bridge, stderr, err)
		return err
	}
	ifacesList := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, iface := range ifacesList {
		stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "type")
		if err != nil {
			klog.Warningf("Failed to determine the type of interface: %q, "+
				"stderr: %q, error: %v", iface, stderr, err)
			continue
		} else if stdout != "patch" {
			continue
		}
		stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "options:peer")
		if err != nil {
			klog.Warningf("Failed to get the peer port for patch interface: %q, "+
				"stderr: %q, error: %v", iface, stderr, err)
			continue
		}
		// stdout has the peer interface, just delete it
		peer := strings.TrimSpace(stdout)
		_, stderr, err = RunOVSVsctl("--if-exists", "del-port", "br-int", peer)
		if err != nil {
			klog.Warningf("Failed to delete patch port %q on br-int, "+
				"stderr: %q, error: %v", peer, stderr, err)
		}
	}

	// Now delete the bridge
	stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)
	if err != nil {
		klog.Errorf("Failed to delete OVS bridge, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}
	klog.Infof("Successfully deleted OVS bridge %q", bridge)
	return nil
}
