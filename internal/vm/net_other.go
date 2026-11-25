//go:build !linux

package vm

import (
	"context"
	"errors"
	"fmt"
)

// NetAlloc describes a TAP device and static IPv4 configuration assigned to a VM.
type NetAlloc struct {
	TAPName   string `json:"tapName"`
	GuestIP   string `json:"guestIP"`
	GatewayIP string `json:"gatewayIP"`
	Netmask   string `json:"netmask"`
	GuestMAC  string `json:"guestMAC"`
}

// KernelIPArg returns the Linux CONFIG_IP_PNP kernel command-line parameter.
func (n NetAlloc) KernelIPArg() string {
	if n.GuestIP == "" {
		return ""
	}
	return fmt.Sprintf("ip=%s::%s:%s::eth0:off", n.GuestIP, n.GatewayIP, n.Netmask)
}

// NetManager is a stub on non-Linux hosts.
type NetManager struct{}

func NewNetManager(string, string, string) (*NetManager, error) {
	return &NetManager{}, nil
}

func (m *NetManager) ReserveIP(string, string) {}

func (m *NetManager) EnsureBridge(context.Context) error {
	return errors.New("vm networking requires Linux")
}

func (m *NetManager) AllocTAP(context.Context, string) (NetAlloc, error) {
	return NetAlloc{}, errors.New("vm networking requires Linux")
}

func (m *NetManager) ReleaseTAP(context.Context, NetAlloc) {}

func (m *NetManager) ReleaseTAPByVMID(context.Context, string) {}

func (m *NetManager) SweepOrphanTAPs(context.Context, map[string]bool) int { return 0 }
