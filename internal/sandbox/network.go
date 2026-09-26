package sandbox

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Network is the platform bridge sandboxes attach to. Isolation rules:
//
//   - bridge ports are isolated: sandboxes cannot reach each other
//   - sandboxes cannot open connections to the host (only reply to the proxy)
//   - link-local (cloud metadata, 169.254.0.0/16) is unreachable
//   - egress to the internet is NATed through the host
type Network struct {
	bridge string
	subnet *net.IPNet
	gw     net.IP
	dns    []string

	mu   sync.Mutex
	used map[string]bool // allocated IPs
	next int             // round-robin cursor, so freed addresses aren't reused at once
}

// NewNetwork creates (or adopts) the bridge and installs its firewall rules.
// cidr is the sandbox subnet, e.g. "10.88.0.0/16"; the first address is the
// gateway on the bridge.
func NewNetwork(bridge, cidr string) (*Network, error) {
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("sandbox subnet: %w", err)
	}
	if subnet.IP.To4() == nil {
		return nil, fmt.Errorf("sandbox subnet must be IPv4")
	}
	n := &Network{bridge: bridge, subnet: subnet, gw: nthIP(subnet, 1), dns: hostDNS(), used: map[string]bool{}}

	br, err := netlink.LinkByName(bridge)
	if err != nil {
		attrs := netlink.NewLinkAttrs()
		attrs.Name = bridge
		if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: attrs}); err != nil {
			return nil, fmt.Errorf("create bridge %s: %w", bridge, err)
		}
		if br, err = netlink.LinkByName(bridge); err != nil {
			return nil, err
		}
	}
	ones, _ := subnet.Mask.Size()
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: n.gw, Mask: net.CIDRMask(ones, 32)}}
	if err := netlink.AddrReplace(br, addr); err != nil {
		return nil, fmt.Errorf("bridge address: %w", err)
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return nil, err
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return nil, fmt.Errorf("enable ip_forward: %w", err)
	}
	if err := n.installFirewall(); err != nil {
		return nil, err
	}
	return n, nil
}

// Gateway is the host's address on the bridge.
func (n *Network) Gateway() net.IP { return n.gw }

func (n *Network) installFirewall() error {
	src, br := n.subnet.String(), n.bridge
	rules := []struct {
		table, chain string
		args         []string
	}{
		{"nat", "POSTROUTING", []string{"-s", src, "!", "-o", br, "-j", "MASQUERADE"}},
		// Our chains, jumped to first so they win over other rules (e.g. docker's).
		{"filter", "SERVD-FWD", []string{"-i", br, "-d", "169.254.0.0/16", "-j", "DROP"}},
		{"filter", "SERVD-FWD", []string{"-i", br, "-o", br, "-j", "DROP"}},
		{"filter", "SERVD-FWD", []string{"-i", br, "!", "-o", br, "-j", "ACCEPT"}},
		{"filter", "SERVD-FWD", []string{"-o", br, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},
		{"filter", "SERVD-IN", []string{"-i", br, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},
		{"filter", "SERVD-IN", []string{"-i", br, "-j", "DROP"}},
	}
	for _, c := range []string{"SERVD-FWD", "SERVD-IN"} {
		_ = iptables("-t", "filter", "-N", c) // exists already after a restart
		if err := iptables("-t", "filter", "-F", c); err != nil {
			return err
		}
	}
	for _, r := range rules {
		if iptables(append([]string{"-t", r.table, "-C", r.chain}, r.args...)...) == nil {
			continue
		}
		if err := iptables(append([]string{"-t", r.table, "-A", r.chain}, r.args...)...); err != nil {
			return err
		}
	}
	for chain, target := range map[string]string{"FORWARD": "SERVD-FWD", "INPUT": "SERVD-IN"} {
		if iptables("-t", "filter", "-C", chain, "-j", target) != nil {
			if err := iptables("-t", "filter", "-I", chain, "1", "-j", target); err != nil {
				return err
			}
		}
	}
	return nil
}

func iptables(args ...string) error {
	out, err := exec.Command("iptables", append([]string{"-w"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Attach wires the network namespace at nsPath onto the bridge as eth0,
// reusing want if it is a free address (so a restarted sandbox keeps its IP).
func (n *Network) Attach(id, nsPath string, want net.IP) (net.IP, error) {
	ip, err := n.allocate(want)
	if err != nil {
		return nil, err
	}
	n.flushNeighbor(ip) // a previous holder's MAC may still be cached
	if err := n.attach(id, nsPath, ip); err != nil {
		n.Detach(id, ip)
		return nil, err
	}
	return ip, nil
}

func (n *Network) attach(id, nsPath string, ip net.IP) error {
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return err
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()

	hostName := vethName(id)
	peerName := "p" + hostName[1:]
	_ = deleteLink(hostName) // leftover from a crash
	attrs := netlink.NewLinkAttrs()
	attrs.Name = hostName
	attrs.MTU = bridgeMTU(n.bridge)
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: attrs, PeerName: peerName}); err != nil {
		return fmt.Errorf("create veth: %w", err)
	}
	host, err := netlink.LinkByName(hostName)
	if err != nil {
		return err
	}
	peer, err := netlink.LinkByName(peerName)
	if err != nil {
		return err
	}
	br, err := netlink.LinkByName(n.bridge)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetMaster(host, br); err != nil {
		return err
	}
	if err := netlink.LinkSetIsolated(host, true); err != nil {
		return fmt.Errorf("isolate bridge port: %w", err)
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return err
	}
	if err := netlink.LinkSetNsFd(peer, int(ns)); err != nil {
		return err
	}

	// Inside the sandbox's namespace.
	if peer, err = h.LinkByName(peerName); err != nil {
		return err
	}
	if err := h.LinkSetName(peer, "eth0"); err != nil {
		return err
	}
	ones, _ := n.subnet.Mask.Size()
	if err := h.AddrAdd(peer, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(ones, 32)}}); err != nil {
		return err
	}
	if err := h.LinkSetUp(peer); err != nil {
		return err
	}
	if err := loopbackUp(h); err != nil {
		return err
	}
	return h.RouteAdd(&netlink.Route{LinkIndex: peer.Attrs().Index, Gw: n.gw})
}

// Reserve marks an address as taken (for sandboxes that survived a platform
// restart).
func (n *Network) Reserve(ip net.IP) {
	if ip == nil {
		return
	}
	n.mu.Lock()
	n.used[ip.String()] = true
	n.mu.Unlock()
}

// Detach removes a sandbox's veth pair and frees its address.
func (n *Network) Detach(id string, ip net.IP) {
	_ = deleteLink(vethName(id))
	if ip != nil {
		n.flushNeighbor(ip)
		n.mu.Lock()
		delete(n.used, ip.String())
		n.mu.Unlock()
	}
}

// flushNeighbor drops the host's ARP entry for ip on the bridge, so traffic
// to a reassigned address reaches its new owner immediately rather than the
// old MAC until the entry expires.
func (n *Network) flushNeighbor(ip net.IP) {
	br, err := netlink.LinkByName(n.bridge)
	if err != nil {
		return
	}
	neighs, err := netlink.NeighList(br.Attrs().Index, netlink.FAMILY_V4)
	if err != nil {
		return
	}
	for _, nb := range neighs {
		if nb.IP.Equal(ip) {
			nb := nb
			_ = netlink.NeighDel(&nb)
		}
	}
}

// LoopbackOnly brings up lo in a namespace that gets no other network.
func LoopbackOnly(nsPath string) error {
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return err
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	return loopbackUp(h)
}

func loopbackUp(h *netlink.Handle) error {
	lo, err := h.LinkByName("lo")
	if err != nil {
		return err
	}
	return h.LinkSetUp(lo)
}

func (n *Network) allocate(want net.IP) (net.IP, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if want != nil && n.subnet.Contains(want) && !want.Equal(n.gw) && !n.used[want.String()] {
		n.used[want.String()] = true
		return want, nil
	}
	ones, bits := n.subnet.Mask.Size()
	size := 1 << (bits - ones)
	for tries := 0; tries < size; tries++ {
		n.next++
		if n.next < 2 || n.next >= size-1 { // skip network, gateway, broadcast
			n.next = 2
		}
		ip := nthIP(n.subnet, n.next)
		if !n.used[ip.String()] {
			n.used[ip.String()] = true
			return ip, nil
		}
	}
	return nil, errors.New("sandbox network is full")
}

func nthIP(subnet *net.IPNet, i int) net.IP {
	base := binary.BigEndian.Uint32(subnet.IP.To4())
	out := make(net.IP, 4)
	binary.BigEndian.PutUint32(out, base+uint32(i))
	return out
}

// vethName derives a stable, ≤15-char interface name from a sandbox id.
func vethName(id string) string {
	h := sha256.Sum256([]byte(id))
	return "sv" + hex.EncodeToString(h[:6])
}

func deleteLink(name string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	return netlink.LinkDel(l)
}

func bridgeMTU(bridge string) int {
	if l, err := netlink.LinkByName(bridge); err == nil && l.Attrs().MTU > 0 {
		return l.Attrs().MTU
	}
	return 1500
}

// hostDNS returns the host's non-loopback resolvers (a loopback resolver such
// as systemd-resolved's 127.0.0.53 is unreachable from a sandbox), falling
// back to public ones.
func hostDNS() []string {
	var out []string
	if f, err := os.Open("/etc/resolv.conf"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && fields[0] == "nameserver" {
				if ip := net.ParseIP(fields[1]); ip != nil && !ip.IsLoopback() && ip.To4() != nil {
					out = append(out, fields[1])
				}
			}
		}
	}
	if len(out) == 0 {
		out = []string{"1.1.1.1", "8.8.8.8"}
	}
	return out
}
