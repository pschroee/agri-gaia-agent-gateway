// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"testing"

	"agw/internal/config"
)

// pi falls back to its built-in models when it cannot load the models.json the gateway wrote, and the RPC does
// not report the error. A model of the catalogue that pi does not list (and that is not built in) is the sign.
func TestMissingFromRegistry(t *testing.T) {
	cat := &config.Catalog{Providers: []config.Provider{
		{ID: "ds", Models: []config.Model{{ID: "flash", PiBuiltin: true}}},
		{ID: "farm", Models: []config.Model{{ID: "Org/M"}}},
	}}
	reg := []config.RegistryModel{{Provider: "ds", ID: "flash"}}
	if got := MissingFromRegistry(cat, reg); len(got) != 1 || got[0] != "farm/Org/M" {
		t.Fatalf("missing: %v", got)
	}
	reg = append(reg, config.RegistryModel{Provider: "farm", ID: "Org/M"})
	if got := MissingFromRegistry(cat, reg); len(got) != 0 {
		t.Fatalf("all listed: %v", got)
	}
}
