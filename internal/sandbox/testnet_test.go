package sandbox

import (
	"net/netip"
	"testing"
)

// Jedes gewählte Subnetz ist gültig und liegt in 10.231.64.0/18, also nie in Dockers 172.x.
func TestTestSubnetRange(t *testing.T) {
	pool := netip.MustParsePrefix("10.231.64.0/18")
	for range 5000 {
		s := TestSubnet()
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Bits() != 28 || !pool.Contains(p.Addr()) || p.Masked() != p {
			t.Fatalf("ungültig: %s (%v)", s, err)
		}
	}
}
