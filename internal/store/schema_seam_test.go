// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"strings"
	"testing"
)

// A schema file a feature registered is applied when a schema is created (G1.6); the tables of this
// package come first and stay.
func TestRegisteredSchemaApplied(t *testing.T) {
	registerSchema(`CREATE TABLE IF NOT EXISTS seam_items (id text PRIMARY KEY, note text NOT NULL DEFAULT '')`)
	t.Cleanup(func() { registeredSchemas = registeredSchemas[:len(registeredSchemas)-1] })

	if !strings.Contains(SchemaSQL(), "seam_items") {
		t.Fatal("the registered schema is not part of SchemaSQL")
	}
	if strings.Index(SchemaSQL(), "seam_items") < strings.Index(SchemaSQL(), "CREATE TABLE IF NOT EXISTS chats") {
		t.Fatal("the registered schema must follow the built-in one")
	}
	s := open(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO seam_items (id) VALUES ('x')`); err != nil {
		t.Fatalf("registered table missing: %v", err)
	}
	// The built-in schema was applied as well.
	c, err := s.CreateChat(ctx, NewChat{Title: "seam", Model: "m", Variant: "cli"})
	if err != nil || c.ID == "" {
		t.Fatalf("chats table: %+v %v", c, err)
	}
}
