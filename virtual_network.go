package main

import (
	// "bufio"
	"errors"
	"fmt"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"github.com/coreos/go-iptables/iptables"
	"github.com/docker/docker/pkg/mount"
	"net"
	"os/exec"
	"os"
	"runtime"
	"strconv"
	"time"
)

const (
	BridgeName = "handoff-bridge"
)

var (
	vethCount uint64 = 1
	freeIPs   []string
	bridgeAddr string
	netCidr string
)

func verifyBridgePresence(bridgeCidr string) error {
	if err := setupIPTables(bridgeCidr); err != nil {
		return err
	}

	handle, err := netlink.NewHandle()
	if err != nil {
		return err
	}

	// parse the CIDR block supplied by user
	addr, err := netlink.ParseAddr(bridgeCidr)
	if err != nil {
		return err
	}
	freeIPs, _ = hosts(bridgeCidr)

	netCidr = bridgeCidr

	link, err := handle.LinkByName(BridgeName)
	if err != nil {
		// we need to create the link
		linkAttrs := netlink.NewLinkAttrs()
		linkAttrs.Name = BridgeName

		link = &netlink.Bridge{LinkAttrs: linkAttrs}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
	}

	// at this point, the bridge ought to exist
	// we need to `up` it!
	netlink.LinkSetUp(link)

	// ...and add its address (ignore return code for now, since it's not idempotent)
	netlink.AddrAdd(link, addr)

	bridgeAddr = addr.IP.String()

	return nil
}

func execInNetNS(command string, args []string) error {
	newns, err := setupNetNs()
	if err != nil {
		return err
	}
	defer newns.Close()

	oldns, err := netns.Get()
	if err != nil {
		return err
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	saveLocation := fmt.Sprintf("./ns-%d", time.Now().Unix())
	if err = saveAndSwapNetNs(oldns, newns, saveLocation); err != nil {
		return err
	}

	// execute the command
	cmd := exec.Command(command, args...)
	// out, _ := cmd.StdoutPipe()
	// stderr, _ := cmd.StderrPipe()
	// cmd.StdinPipe()
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Start()	

	if err = restoreOldNs(saveLocation); err != nil {
		panic("unable to restore old network namespace")
	}

	return nil
}

func setupNetNs() (netns.NsHandle, error) {
	var handle netns.NsHandle

	bridge, err := netlink.LinkByName(BridgeName)
	if err != nil {
		return handle, err
	}

	// create a new veth interface
	countStr := strconv.FormatUint(vethCount, 10)
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: "hveth" + countStr,
			MTU:  1500},
		PeerName: "brveth" + countStr,
	}

	if len(freeIPs) == 0 {
		return handle, errors.New("No more IPs left in virtual network")
	}

	if err = netlink.LinkAdd(veth); err != nil {
		return handle, err
	}

	// attach the peer to the bridge
	peerIdx, err := netlink.VethPeerIndex(veth)
	if err != nil {
		return handle, err
	}

	peer, err := netlink.LinkByIndex(peerIdx)
	if err != nil {
		return handle, err
	}

	rbridge := netlink.Bridge{LinkAttrs: *(bridge.Attrs())}
	if netlink.LinkSetMaster(peer, &rbridge) != nil {
		return handle, err
	}

	// now we create a new network namespace
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	oldns, err := netns.Get()
	if err != nil {
		return handle, err
	}

	handle, err = netns.New()
	if err != nil {
		return handle, err
	}
	defer func() {
		// an insurance policy against an early return leaving us in the old netns
		err := netns.Set(oldns)
		if err != nil {
			panic("setupNetNs: error restoring old namespace")
		}

		oldns.Close()
	}()

	err = netns.Set(oldns)
	if err != nil {
		panic("setupNetNs: error restoring old namespace")
	}

	veth.PeerName = "" // prevents moving peer into namespace
	err = netlink.LinkSetNsFd(veth, int(handle))
	if err != nil {
		return handle, err
	}

	// set both ends of the veth up
	err = netlink.LinkSetUp(peer)
	if err != nil {
		return handle, err
	}

	// enter the netns
	if err = netns.Set(handle); err != nil {
		return handle, err
	}

	// rename vethn to eth0
	if err = netlink.LinkSetName(veth, "eth0"); err != nil {
		return handle, err
	}

	// and set it up
	if err = netlink.LinkSetUp(veth); err != nil {
		return handle, err
	}

	// and set up lo
	setupLoopback()

	// and assign it an IP
	mutex.Lock()
	vethAddr := freeIPs[0]
	freeIPs = freeIPs[1:]
	vethCount += 1
	mutex.Unlock()

	addr, err := netlink.ParseAddr(vethAddr + "/32")
	if err != nil {
		return handle, err
	}
	netlink.AddrAdd(veth, addr)

	bridgeRoute, err := makeBridgeNetRoute(veth.Attrs().Index)
	if err != nil {
		return handle, err
	}

	if err = netlink.RouteAdd(&bridgeRoute); err != nil {
		return handle, err
	}

	// add the default route
	defaultRoute, err := makeDefaultRoute(veth.Attrs().Index)
	if err != nil {
		return handle, err
	}

	if err = netlink.RouteAdd(&defaultRoute); err != nil {
		return handle, err
	}

	return handle, nil
}

func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func hosts(cidr string) ([]string, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}

	var ips []string
	for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
		ips = append(ips, ip.String())
	}

	return ips[1 : len(ips)-1], nil
}

func makeDefaultRoute(linkIndex int) (netlink.Route, error) {
	route := netlink.Route{}

	_, dst, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		return route, err
	}

	ip := net.ParseIP(bridgeAddr)
	if ip == nil {
		return route, errors.New("cannot parse bridge IP")
	}

	route.Dst = dst
	route.LinkIndex = linkIndex
	route.Gw = ip

	return route, nil
}

func makeBridgeNetRoute(link int) (netlink.Route, error) {
	route := netlink.Route{}

	_, dst, err := net.ParseCIDR(netCidr)
	if err != nil {
		return route, err
	}

	route.Dst = dst
	route.LinkIndex = link
	route.Scope = netlink.SCOPE_LINK

	return route, nil
}

func setupLoopback() error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}

	addr, err := netlink.ParseAddr("127.0.0.1/8")
	if err != nil {
		return err
	}

	if err = netlink.AddrAdd(lo, addr); err != nil {
		return err
	}

	return netlink.LinkSetUp(lo)
}

func setupIPTables(bridgeCidr string) error {
	table, err := iptables.New()
	if err != nil {
		return err
	}

	if err = table.AppendUnique("nat", "POSTROUTING", "-s", bridgeCidr, "-j", "MASQUERADE"); err != nil {
		return err
	}

	if err = table.ChangePolicy("filter", "FORWARD", "ACCEPT"); err != nil {
		return err
	}

	return nil
}

func saveAndSwapNetNs(oldns, newns netns.NsHandle, saveLocation string) error {
	pid := os.Getpid()
	nsloc := fmt.Sprintf("/proc/%d/ns/net", pid)

	// create the file
	tempfile, err := os.Create(saveLocation)
	if err != nil {
		return err
	}
	tempfile.Close()

	if err = mount.Mount(nsloc, saveLocation, "", "bind"); err != nil {
		fmt.Println(err)
		return err
	}
	
	oldns.Close()
	netns.Set(newns)

	return nil
}

func restoreOldNs(saveLocation string) error {
	ns, err := netns.GetFromPath(saveLocation)
	if err != nil {
		return err
	}

	if err = netns.Set(ns); err != nil {
		return err
	}

	if err = mount.Unmount(saveLocation); err != nil {
		return err
	}

	if err = os.Remove(saveLocation); err != nil {
		return err
	}

	return nil
}
