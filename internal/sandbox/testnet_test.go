package sandbox

import (
	"net/netip"
	"testing"
)

// Every chosen subnet is valid and lies in 10.231.64.0/18, so never in Docker's 172.x.
func TestTestSubnetRange(t *testing.T) {
	pool := netip.MustParsePrefix("10.231.64.0/18")
	for range 5000 {
		s := TestSubnet()
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Bits() != 28 || !pool.Contains(p.Addr()) || p.Masked() != p {
			t.Fatalf("invalid: %s (%v)", s, err)
		}
	}
}
