// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"strings"

	"agw/internal/config"
	"agw/internal/toolset"
)

// The capabilities of this package (G1.11): vision, internet, compute, bindings. A feature adds its own
// in its own file: notebook (own code C-E3), edge (edge D-E2), pre-annotation (V1).

func init() {
	registerCapability("vision", capabilityVision, setupVision)
	registerCapability("internet", capabilityInternet)
	registerCapability("compute", capabilityCompute)
	registerCapability("bindings", capabilityBindings, setupBindings)
}

// chatModelID names the model a chat works with; empty when the chat is unknown and the catalog names
// no default. A chat without a model runs with the catalog's default (Manager.run), so the overview
// resolves it the same way (config.Catalog.ChatModel).
func (m *Manager) chatModelID(ctx context.Context, chatID string) string {
	id := ""
	if c, err := m.st.GetChat(ctx, chatID); err == nil {
		id = c.Model
	}
	if m.cat == nil {
		return id
	}
	return m.cat.ChatModel(id)
}

// visionFor answers the vision entry from the catalog alone (G1.12). The checks and the pre-annotation
// ask the same catalog entry through assess.ReviewAllowed; both resolve the chat's model with
// config.Catalog.ChatModel, so "capabilities" and an image review cannot disagree about the same model
// (issue #163). modelID is already resolved; empty means the catalog names no model at all.
func visionFor(cat *config.Catalog, modelID string) Capability {
	c := Capability{
		Without: "work from the user's description of the image, or ask them to describe what matters",
	}
	if modelID == "" {
		c.Detail = "this chat has no model; whether images work is unknown"
		return c
	}
	if cat == nil {
		c.Detail = "the model " + modelID + " is not in the catalog; whether it reads images is unknown"
		return c
	}
	_, model, found := cat.Lookup(modelID)
	if !found {
		c.Detail = "the model " + modelID + " is not in the catalog; whether it reads images is unknown"
		return c
	}
	name := model.Name
	if name == "" {
		name = model.ID
	}
	c.Available = model.ReadsImages()
	if c.Available {
		c.Detail = "the current model (" + name + ") reads images"
	} else {
		c.Detail = "the current model (" + name + ") reads text only"
	}
	return c
}

// capabilityVision: can the current chat model read images (G1.12)? Bildeingabe ist optional: without
// it everything else keeps working, the work with images then has to go through the user's words.
func capabilityVision(ctx context.Context, m *Manager, chatID string) Capability {
	return visionFor(m.cat, m.chatModelID(ctx, chatID))
}

// setupVision: the part of vision that does not change during a chat, namely the model a slot starts
// with (the catalog's default). A feature that changes the model of a chat does not change this line.
func setupVision(s Setup) Capability {
	c := Capability{Without: "work from the user's description of the image"}
	if s.Catalog == nil || s.Catalog.Default == "" {
		c.Detail = "no model configured"
		return c
	}
	_, model, ok := s.Catalog.Lookup(s.Catalog.Default)
	if !ok {
		c.Detail = "the model " + s.Catalog.Default + " is not in the catalog"
		return c
	}
	name := model.Name
	if name == "" {
		name = model.ID
	}
	c.Available = model.ReadsImages()
	if c.Available {
		c.Detail = "the model " + name + " reads images"
	} else {
		c.Detail = "the model " + name + " reads text only"
	}
	return c
}

// capabilityInternet: does an internet grant cover this chat right now? Off is the default, and the agent
// can request a kind of access for a task (the user decides). With grants the detail names each one
// (kind, list, end and data left; issue #175).
func capabilityInternet(ctx context.Context, m *Manager, chatID string) Capability {
	c := Capability{
		Available: m.InternetOn(ctx, chatID),
		Without:   "work with the files in the workspace and with the platform; ask the user for sources you cannot reach",
	}
	if c.Available {
		c.Detail = "internet is on in this chat"
		var parts []string
		for _, g := range m.ActiveGrantViews(chatID) {
			left := max(g.MaxBytes-g.Bytes, 0) >> 20
			what := g.Kind
			if len(g.Domains) > 0 {
				what += " " + strings.Join(g.Domains, ", ")
			}
			if g.ExpiresAt != nil {
				what += " until " + g.ExpiresAt.UTC().Format("15:04")
			}
			parts = append(parts, fmt.Sprintf("%s, %d MB left (grant %d)", what, left, g.ID))
		}
		if len(parts) > 0 {
			c.Detail += ": " + strings.Join(parts, "; ")
		}
	} else {
		c.Detail = "internet is off in this chat; you can request a kind of access (search, domains, packages or full) for a task and the user decides"
	}
	return c
}

// capabilityCompute: how many compute calls (building or starting a training) this chat has left. The
// limit is fixed for the chat (AGW_COMPUTE_MAX_PER_CHAT).
func capabilityCompute(ctx context.Context, m *Manager, chatID string) Capability {
	max := m.computeMax()
	used, err := m.st.ComputeCalls(ctx, chatID)
	if err != nil {
		return Capability{Detail: "the used compute calls are not readable right now",
			Without: "prepare everything, but let the user start the training"}
	}
	left := max - used
	if left < 0 {
		left = 0
	}
	c := Capability{
		Available: left > 0,
		Detail:    fmt.Sprintf("%d of %d compute calls left in this chat (building or starting a training)", left, max),
		Without:   "prepare the training and let the user start it in the platform's UI",
	}
	return c
}

// capabilityBindings: how this chat reaches the platform. The binding is fixed when the chat is created
// (AGW_TOOLSETS, issue #29).
func capabilityBindings(ctx context.Context, m *Manager, chatID string) Capability {
	c := Capability{
		Available: true,
		Without:   "the tools of the other bindings do not exist in this chat; work with the tools of this one",
	}
	variant := ""
	if ch, err := m.st.GetChat(ctx, chatID); err == nil {
		variant = ch.Variant
	}
	if variant == "" {
		variant = m.opt.Toolsets.Key()
	}
	// X0.12: the overview names the binding, never the experiment profile or its protection level.
	if base, _ := toolset.SplitExperiment(variant); base != "" {
		variant = base
	}
	c.Detail = "this chat uses the binding " + variant
	return c
}

// setupBindings: the binding of the slot the note is for; it never changes during a chat.
func setupBindings(s Setup) Capability {
	if s.Variant == "" {
		return Capability{Detail: "unknown"}
	}
	variant := s.Variant
	if base, _ := toolset.SplitExperiment(variant); base != "" {
		variant = base
	}
	return Capability{Available: true, Detail: "the chat works through " + variant}
}
