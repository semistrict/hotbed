//go:build linux

package vm

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// NetAlloc describes a TAP device and static IPv4 configuration assigned to a VM.
type NetAlloc struct {
	TAPName   string `json:"tapName"`
	GuestIP   string `json:"guestIP"`
	GatewayIP string `json:"gatewayIP"`
	Netmask   string `json:"netmask"`
	GuestMAC  string `json:"guestMAC"`
}

// KernelIPArg returns the Linux CONFIG_IP_PNP kernel command-line parameter
// that configures eth0 inside the guest at boot without DHCP.
func (n NetAlloc) KernelIPArg() string {
	if n.GuestIP == "" {
		return ""
	}
	return fmt.Sprintf("ip=%s::%s:%s::eth0:off", n.GuestIP, n.GatewayIP, n.Netmask)
}

// NetManager manages a single host bridge (e.g. hb-vm0) with NAT masquerading
// and per-VM TAP devices attached to it.
type NetManager struct {
	bridgeName string
	bridgePref netip.Prefix
	vmUser     string

	mu       sync.Mutex
	ready    bool
	usedIPs  map[netip.Addr]string // IP -> vmID
	nextHost uint16
}

// NewNetManager creates a NetManager for bridgeName and CIDR (e.g. 10.199.0.1/16).
func NewNetManager(bridgeName, bridgeCIDR, vmUser string) (*NetManager, error) {
	pref, err := netip.ParsePrefix(bridgeCIDR)
	if err != nil {
		return nil, fmt.Errorf("vm net: parse bridge CIDR %q: %w", bridgeCIDR, err)
	}
	if !pref.Addr().Is4() {
		return nil, fmt.Errorf("vm net: bridge CIDR %q must be IPv4", bridgeCIDR)
	}
	return &NetManager{
		bridgeName: bridgeName,
		bridgePref: pref,
		vmUser:     vmUser,
		usedIPs:    map[netip.Addr]string{pref.Addr(): "_bridge"},
		nextHost:   2,
	}, nil
}

// ReserveIP records an already-allocated IP (from persisted VM metadata on
// worker restart) so newly launched VMs never collide with surviving VMs.
func (m *NetManager) ReserveIP(vmID, ipStr string) {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.usedIPs[addr] = vmID
	m.mu.Unlock()
}

// EnsureBridge idempotently creates the host bridge, enables IPv4 forwarding,
// and installs nftables NAT masquerading for outbound VM internet access.
func (m *NetManager) EnsureBridge(ctx context.Context) error {
	m.mu.Lock()
	if m.ready {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	// Create bridge if absent.
	if err := runCmd(ctx, "ip", "link", "show", "dev", m.bridgeName); err != nil {
		if err := runCmd(ctx, "ip", "link", "add", "name", m.bridgeName, "type", "bridge"); err != nil {
			return fmt.Errorf("create bridge %s: %w", m.bridgeName, err)
		}
	}
	// Assign gateway address idempotently.
	_ = runCmd(ctx, "ip", "addr", "replace", m.bridgePref.String(), "dev", m.bridgeName)
	if err := runCmd(ctx, "ip", "link", "set", m.bridgeName, "up"); err != nil {
		return fmt.Errorf("bring up bridge %s: %w", m.bridgeName, err)
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable ipv4 forwarding: %w", err)
	}
	if err := m.ensureNAT(ctx); err != nil {
		return err
	}

	m.mu.Lock()
	m.ready = true
	m.mu.Unlock()
	return nil
}

func (m *NetManager) ensureNAT(ctx context.Context) error {
	subnet := m.bridgePref.Masked().String()
	if _, err := exec.LookPath("nft"); err == nil {
		// Idempotent atomic nftables table for hotbed VMs.
		script := fmt.Sprintf(`table inet hotbed_vm
delete table inet hotbed_vm
table inet hotbed_vm {
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname "%s" accept
		oifname "%s" ct state established,related accept
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %s oifname != "%s" masquerade
	}
}
`, m.bridgeName, m.bridgeName, subnet, m.bridgeName)
		cmd := exec.CommandContext(ctx, "nft", "-f", "-")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			// First run when table does not exist yet: apply without delete.
			createScript := fmt.Sprintf(`table inet hotbed_vm {
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname "%s" accept
		oifname "%s" ct state established,related accept
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %s oifname != "%s" masquerade
	}
}
`, m.bridgeName, m.bridgeName, subnet, m.bridgeName)
			cmd2 := exec.CommandContext(ctx, "nft", "-f", "-")
			cmd2.Stdin = strings.NewReader(createScript)
			if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
				return fmt.Errorf("configure nftables NAT for %s: %v (%s) / %v (%s)", m.bridgeName, err, strings.TrimSpace(string(out)), err2, strings.TrimSpace(string(out2)))
			}
		}
		return nil
	}
	return errors.New("nft binary not found on host; install nftables for Firecracker VM NAT networking")
}

// AllocTAP allocates a free IPv4 address, creates a TAP device attached to the
// bridge, and returns its configuration.
func (m *NetManager) AllocTAP(ctx context.Context, vmID string) (NetAlloc, error) {
	if err := m.EnsureBridge(ctx); err != nil {
		return NetAlloc{}, err
	}
	ip, err := m.allocIP(vmID)
	if err != nil {
		return NetAlloc{}, err
	}
	tap := tapNameFor(vmID)
	// Clean any stale TAP with the same name.
	_ = runCmd(ctx, "ip", "link", "del", tap)

	args := []string{"tuntap", "add", "dev", tap, "mode", "tap"}
	if m.vmUser != "" {
		args = append(args, "user", m.vmUser)
	}
	if err := runCmd(ctx, "ip", args...); err != nil {
		m.releaseIP(ip)
		return NetAlloc{}, fmt.Errorf("create tap %s: %w", tap, err)
	}
	if err := runCmd(ctx, "ip", "link", "set", tap, "master", m.bridgeName); err != nil {
		_ = runCmd(ctx, "ip", "link", "del", tap)
		m.releaseIP(ip)
		return NetAlloc{}, fmt.Errorf("attach tap %s to %s: %w", tap, m.bridgeName, err)
	}
	if err := runCmd(ctx, "ip", "link", "set", tap, "up"); err != nil {
		_ = runCmd(ctx, "ip", "link", "del", tap)
		m.releaseIP(ip)
		return NetAlloc{}, fmt.Errorf("bring up tap %s: %w", tap, err)
	}
	return NetAlloc{
		TAPName:   tap,
		GuestIP:   ip.String(),
		GatewayIP: m.bridgePref.Addr().String(),
		Netmask:   prefixMaskV4(m.bridgePref.Bits()),
		GuestMAC:  macFor(vmID),
	}, nil
}

// ReleaseTAP removes the TAP device and frees its allocated IP.
func (m *NetManager) ReleaseTAP(ctx context.Context, alloc NetAlloc) {
	if alloc.TAPName != "" {
		_ = runCmd(ctx, "ip", "link", "del", alloc.TAPName)
	}
	if addr, err := netip.ParseAddr(alloc.GuestIP); err == nil {
		m.releaseIP(addr)
	}
}

func (m *NetManager) allocIP(vmID string) (netip.Addr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	base := m.bridgePref.Masked().Addr().As4()
	hostBits := 32 - m.bridgePref.Bits()
	if hostBits < 2 || hostBits > 16 {
		return netip.Addr{}, fmt.Errorf("unsupported bridge prefix length /%d", m.bridgePref.Bits())
	}
	maxHosts := uint32(1)<<hostBits - 2
	baseNum := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	for tries := uint32(0); tries < maxHosts; tries++ {
		off := uint32(m.nextHost)
		m.nextHost++
		if uint32(m.nextHost) > maxHosts {
			m.nextHost = 2
		}
		n := baseNum + off
		cand := netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		if _, used := m.usedIPs[cand]; !used {
			m.usedIPs[cand] = vmID
			return cand, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("no free IPv4 addresses left in %s", m.bridgePref)
}

func (m *NetManager) releaseIP(addr netip.Addr) {
	m.mu.Lock()
	delete(m.usedIPs, addr)
	m.mu.Unlock()
}

func tapNameFor(vmID string) string {
	h := sha256.Sum256([]byte(vmID))
	// IFNAMSIZ is 16 bytes (15 chars + NUL): "hbt-" + 10 hex chars = 14 chars.
	return fmt.Sprintf("hbt-%x", h[:5])
}

func macFor(vmID string) string {
	h := sha256.Sum256([]byte("mac:" + vmID))
	// Locally administered unicast MAC (02:68:62:... where 68 62 = "hb").
	return fmt.Sprintf("02:68:62:%02x:%02x:%02x", h[0], h[1], h[2])
}

func prefixMaskV4(bits int) string {
	var mask uint32
	if bits > 0 {
		mask = ^uint32(0) << (32 - bits)
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask))
}

func runCmd(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
