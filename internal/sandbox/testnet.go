package sandbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Test networks get fixed addresses from 10.231.64.0/18, never Docker's default ranges
// 172.17–172.31. Reason (2026-10-05, twice): a leftover test network in 172.25.0.0/16
// shadowed the VPN address of the Agri-Gaia API (172.25.198.41) inside the Docker VM; names resolved,
// connections went nowhere. The slots themselves live in 10.231.128.0/17, the stack's fixed networks
// in 10.231.18–22.0/24; the range here overlaps with none of them.

// TestSubnet picks a random /28 from 10.231.64.0/18 (64 × 16 = 1024 networks).
func TestSubnet() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	n := (int(b[0])<<8 | int(b[1])) % 1024
	return fmt.Sprintf("10.231.%d.%d/28", 64+n/16, (n%16)*16)
}

// CreateTestNetwork creates a bridge network for a test with a subnet from TestSubnet and
// tries another one on overlap.
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
