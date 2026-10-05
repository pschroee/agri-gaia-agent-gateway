package sandbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Netze der Tests bekommen feste Adressen aus 10.231.64.0/18, nie Dockers Standardbereiche
// 172.17–172.31. Grund (05.10.2026, zweimal): Ein liegengebliebenes Testnetz in 172.25.0.0/16
// verdeckte in der Docker-VM die VPN-Adresse der Agri-Gaia-API (172.25.198.41); Namen lösten auf,
// Verbindungen liefen ins Leere. Die Plätze selbst liegen in 10.231.128.0/17, die festen Netze des
// Stacks in 10.231.18–22.0/24; der Bereich hier überschneidet sich mit keinem davon.

// TestSubnet wählt zufällig ein /28 aus 10.231.64.0/18 (64 × 16 = 1024 Netze).
func TestSubnet() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	n := (int(b[0])<<8 | int(b[1])) % 1024
	return fmt.Sprintf("10.231.%d.%d/28", 64+n/16, (n%16)*16)
}

// CreateTestNetwork legt ein Bridge-Netz für einen Test mit einem Subnetz aus TestSubnet an und
// versucht bei einer Überschneidung ein anderes.
func CreateTestNetwork(ctx context.Context, cli *client.Client, name string, internal bool) error {
	var err error
	for i := 0; i < 20; i++ {
		_, err = cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
			Driver: "bridge", Internal: internal,
			Labels: map[string]string{LabelManaged: "test"},
			IPAM:   &network.IPAM{Config: []network.IPAMConfig{{Subnet: mustPrefix(TestSubnet())}}},
		})
		if err == nil || (!strings.Contains(err.Error(), "overlap") && !strings.Contains(err.Error(), "Pool")) {
			return err
		}
	}
	return err
}
