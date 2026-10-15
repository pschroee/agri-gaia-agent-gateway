// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
)

// The capabilities of this package (G1.11): vision, internet, compute, bindings. A feature adds its own
// in its own file: notebook (own code C-E3), edge (edge D-E2), pre-annotation (V1).

func init() {
	registerCapability("vision", capabilityVision, setupVision)
	registerCapability("internet", capabilityInternet)
	registerCapability("compute", capabilityCompute)
	registerCapability("bindings", capabilityBindings, setupBindings)
}

// chatModelID names the model a chat works with; empty when the chat is unknown or has none.
func (m *Manager) chatModelID(ctx context.Context, chatID string) string {
	if c, err := m.st.GetChat(ctx, chatID); err == nil && c.Model != "" {
		return c.Model
	}
	if m.cat != nil {
		return m.cat.Default
	}
	return ""
}

// visionModel describes a catalog model for the overview; ok is false when the catalog does not know it.
func (m *Manager) visionModel(id string) (name string, readsImages, ok bool) {
	if m.cat == nil || id == "" {
		return "", false, false
	}
	_, model, found := m.cat.Lookup(id)
	if !found {
		return "", false, false
	}
	name = model.Name
	if name == "" {
		name = model.ID
	}
	return name, model.ReadsImages(), true
}

// capabilityVision: can the current chat model read images (G1.12)? Bildeingabe ist optional: without
// it everything else keeps working, the work with images then has to go through the user's words.
func capabilityVision(ctx context.Context, m *Manager, chatID string) Capability {
	c := Capability{
		Without: "work from the user's description of the image, or ask them to describe what matters",
	}
	id := m.chatModelID(ctx, chatID)
	if id == "" {
		c.Detail = "this chat has no model; whether images work is unknown"
		return c
	}
	name, readsImages, known := m.visionModel(id)
	if !known {
		c.Detail = "the model " + id + " is not in the catalog; whether it reads images is unknown"
		return c
	}
	c.Available = readsImages
	if readsImages {
		c.Detail = "the current model (" + name + ") reads images"
	} else {
		c.Detail = "the current model (" + name + ") reads text only"
	}
	return c
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

// capabilityInternet: is internet switched on in this chat right now? Off is the default, and the agent
// can request it for a task (the user decides).
func capabilityInternet(ctx context.Context, m *Manager, chatID string) Capability {
	c := Capability{
		Available: m.InternetOn(ctx, chatID),
		Without:   "work with the files in the workspace and with the platform; ask the user for sources you cannot reach",
	}
	if c.Available {
		c.Detail = "internet is on in this chat"
	} else {
		c.Detail = "internet is off in this chat; you can request it for a task and the user decides"
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
	c.Detail = "this chat uses the binding " + variant
	return c
}

// setupBindings: the binding of the slot the note is for; it never changes during a chat.
func setupBindings(s Setup) Capability {
	if s.Variant == "" {
		return Capability{Detail: "unknown"}
	}
	return Capability{Available: true, Detail: "the chat works through " + s.Variant}
}
