package snapshotter

import "testing"

func TestGatewayMACIsLocallyAdministered(t *testing.T) {
	t.Parallel()
	// Must stay in sync with configureGuestNetwork's permanent ARP pin and
	// ensureSharedBridge's bridge MAC assignment.
	if gatewayMAC != "02:00:00:5f:00:fe" {
		t.Fatalf("gatewayMAC = %q", gatewayMAC)
	}
}
