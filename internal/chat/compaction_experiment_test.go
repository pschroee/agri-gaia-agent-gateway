// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agw/internal/config"
	"agw/internal/store"
)

// Issue #281: a plan may turn pi's compaction on for its runs; the reserve is checked against the model.

func compactionCatalog(t *testing.T) *config.Catalog {
	t.Helper()
	cat, err := config.ParseCatalog([]byte(`{"default":"edge/qwen","providers":[{"id":"edge","models":[{"id":"qwen","context_window":262144,"max_tokens":12288}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestExperimentCompactionCheck(t *testing.T) {
	m := &Manager{cat: compactionCatalog(t)}
	for _, tc := range []struct {
		name    string
		c       *store.RunCompaction
		want    int64
		wantErr string
	}{
		{"none", nil, 0, ""},
		{"off", &store.RunCompaction{Mode: "off"}, 0, ""},
		{"empty mode", &store.RunCompaction{}, 0, ""},
		{"off with reserve", &store.RunCompaction{Mode: "off", ReserveTokens: 5}, 0, "needs mode auto"},
		{"unknown mode", &store.RunCompaction{Mode: "always", ReserveTokens: 26214}, 0, "mode must be off or auto"},
		{"auto without reserve", &store.RunCompaction{Mode: "auto"}, 0, "needs reserve_tokens"},
		{"auto 10 percent", &store.RunCompaction{Mode: "auto", ReserveTokens: 26214}, 26214, ""},
		{"auto at the floor (max_tokens + 4096)", &store.RunCompaction{Mode: "auto", ReserveTokens: 16384}, 16384, ""},
		{"auto below the floor", &store.RunCompaction{Mode: "auto", ReserveTokens: 16383}, 0, "compaction would come too late"},
		{"auto 5 percent is below the floor", &store.RunCompaction{Mode: "auto", ReserveTokens: 13107}, 0, "compaction would come too late"},
		{"auto above half the window", &store.RunCompaction{Mode: "auto", ReserveTokens: 140000}, 0, "more than half"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.experimentCompaction("edge/qwen", tc.c)
			if tc.wantErr == "" {
				if err != nil || got != tc.want {
					t.Fatalf("got %d, %v; want %d", got, err, tc.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want ErrInvalid containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// A plan with compaction auto gets automatic compaction in pi and slots with its own reserve; without the field
// the experiment stays without compaction (X2.5).
func TestCreateExperimentCompaction(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	sent := func(a *fakeAgent) (seen, last bool) {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, cmd := range a.cmds {
			if cmd["type"] == "set_auto_compaction" {
				seen, last = true, cmd["enabled"].(bool)
			}
		}
		return seen, last
	}
	newChat := func(c *store.RunCompaction) ChatView {
		t.Helper()
		prof := experimentProfile("none")
		prof.Compaction = c
		v, err := e.m.Create(context.Background(), NewChat{Title: "compaction", Experiment: prof,
			Delegation: json.RawMessage(experimentDelegation), Owner: "sub-1", OwnerName: experimentUser})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	plain := newChat(nil)
	if plain.AutoCompact || plain.Variant != "exp:cli" {
		t.Fatalf("without the field: auto %v, variant %q", plain.AutoCompact, plain.Variant)
	}
	if seen, last := sent(e.agent(len(e.agents) - 1)); !seen || last {
		t.Fatalf("set_auto_compaction without the field: %v %v", seen, last)
	}
	auto := newChat(&store.RunCompaction{Mode: "auto", ReserveTokens: 26214})
	if !auto.AutoCompact || auto.Variant != "exp:cli~cr26214" {
		t.Fatalf("with auto: auto %v, variant %q", auto.AutoCompact, auto.Variant)
	}
	if auto.Experiment == nil || !auto.Experiment.Compaction.Auto() || auto.Experiment.Compaction.ReserveTokens != 26214 {
		t.Fatalf("stored profile: %+v", auto.Experiment)
	}
	if seen, last := sent(e.agent(len(e.agents) - 1)); !seen || !last {
		t.Fatalf("set_auto_compaction with auto: %v %v", seen, last)
	}
	if _, err := e.m.Create(context.Background(), NewChat{Title: "bad", Experiment: func() *store.Experiment {
		p := experimentProfile("none")
		p.Compaction = &store.RunCompaction{Mode: "auto"}
		return p
	}(), Delegation: json.RawMessage(experimentDelegation), Owner: "sub-1", OwnerName: experimentUser}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("auto without reserve: %v", err)
	}
}
