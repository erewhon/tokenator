package widget

import "testing"

func TestReserve(t *testing.T) {
	inv := New()
	inv.Add("bolt", 10)

	if err := inv.Reserve("bolt", 4); err != nil {
		t.Fatalf("reserve 4 of 10: %v", err)
	}
	if got := inv.Available("bolt"); got != 6 {
		t.Fatalf("available after reserving 4 = %d, want 6", got)
	}

	// Reserving exactly the remaining stock must succeed.
	if err := inv.Reserve("bolt", 6); err != nil {
		t.Fatalf("reserve remaining 6: %v", err)
	}
	if got := inv.Available("bolt"); got != 0 {
		t.Fatalf("available after reserving all = %d, want 0", got)
	}

	// Now truly out of stock.
	if err := inv.Reserve("bolt", 1); err == nil {
		t.Fatal("reserve from empty stock should fail")
	}
}

func TestReserveErrors(t *testing.T) {
	inv := New()
	inv.Add("nut", 5)
	if err := inv.Reserve("nut", 0); err == nil {
		t.Error("zero qty should fail")
	}
	if err := inv.Reserve("nut", -2); err == nil {
		t.Error("negative qty should fail")
	}
	if err := inv.Reserve("washer", 1); err == nil {
		t.Error("unknown sku should fail")
	}
	if err := inv.Reserve("nut", 6); err == nil {
		t.Error("over-stock reserve should fail")
	}
}

func TestRelease(t *testing.T) {
	inv := New()
	inv.Add("gear", 3)
	if err := inv.Reserve("gear", 3); err != nil {
		t.Fatalf("reserve all: %v", err)
	}
	if err := inv.Release("gear", 2); err != nil {
		t.Fatalf("release 2: %v", err)
	}
	if got := inv.Available("gear"); got != 2 {
		t.Fatalf("available after release = %d, want 2", got)
	}
	if err := inv.Release("gear", 5); err == nil {
		t.Error("releasing more than reserved should fail")
	}
}
