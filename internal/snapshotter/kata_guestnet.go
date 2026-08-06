package snapshotter

import (
	"fmt"
)

const (
	guestIface   = "eth0"
	guestMask    = "16"
	guestGateway = "10.88.0.1"
	guestSubnet  = "10.88.0.0/16"
	guestBridge  = "sf-br0"
	gatewayMAC   = "02:00:00:5f:00:fe"
)

// guestNet is the per-instance address plan for restored Kata/gVisor children.
type guestNet struct {
	Iface      string
	IP         string
	Mask       string
	Gateway    string
	Subnet     string
	MAC        string
	GatewayMAC string
}

// guestNetForSlot assigns a unique guest IP/MAC from SlotID.
// Slot 0 → 10.88.0.2, slot 1 → 10.88.0.3, … up to slot 252 → 10.88.0.254.
// Gateway 10.88.0.1 is reserved for the host/bridge side (sf-br0).
func guestNetForSlot(slotID int32) (guestNet, error) {
	if slotID < 0 || slotID > 252 {
		return guestNet{}, fmt.Errorf("slotID %d out of range for guest IP (want 0..252)", slotID)
	}
	host := byte(2 + slotID)
	return guestNet{
		Iface:      guestIface,
		IP:         fmt.Sprintf("10.88.0.%d", host),
		Mask:       guestMask,
		Gateway:    guestGateway,
		Subnet:     guestSubnet,
		MAC:        fmt.Sprintf("02:00:00:5f:00:%02x", host),
		GatewayMAC: gatewayMAC,
	}, nil
}
