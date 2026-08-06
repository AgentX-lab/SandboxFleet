package snapshotter

import "testing"

func TestGuestNetForSlotUnique(t *testing.T) {
	t.Parallel()
	a, err := guestNetForSlot(0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := guestNetForSlot(1)
	if err != nil {
		t.Fatal(err)
	}
	if a.IP != "10.88.0.2" || b.IP != "10.88.0.3" {
		t.Fatalf("ips = %q %q", a.IP, b.IP)
	}
	if a.MAC == b.MAC {
		t.Fatalf("macs collided: %q", a.MAC)
	}
	if a.Gateway != "10.88.0.1" {
		t.Fatalf("gateway = %q", a.Gateway)
	}
}

func TestGuestNetForSlotRejectsOutOfRange(t *testing.T) {
	t.Parallel()
	if _, err := guestNetForSlot(-1); err == nil {
		t.Fatal("expected error for slot -1")
	}
	if _, err := guestNetForSlot(253); err == nil {
		t.Fatal("expected error for slot 253")
	}
}
