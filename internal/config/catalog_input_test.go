// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package config

import "testing"

// The catalog carries what a model accepts (G1.12): "image" in input means the model reads images;
// without the field a model counts as text only, so that nothing is promised that nobody checked.
func TestCatalogInputField(t *testing.T) {
	cat, err := ParseCatalog([]byte(`{"default":"p/bild","providers":[{"id":"p","models":[
		{"id":"bild","input":["text","image"]},
		{"id":"text","input":["text"]},
		{"id":"leer"},
		{"id":"komisch","input":["audio"]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"p/bild": true, "p/text": false, "p/leer": false, "p/komisch": false} {
		_, m, ok := cat.Lookup(id)
		if !ok {
			t.Fatalf("%s missing", id)
		}
		if got := m.ReadsImages(); got != want {
			t.Errorf("%s: ReadsImages = %v, want %v (input %v)", id, got, want, m.Input)
		}
	}
	// The field is optional: an older catalog without it still parses.
	if _, err := ParseCatalog([]byte(`{"default":"p/m","providers":[{"id":"p","models":[{"id":"m"}]}]}`)); err != nil {
		t.Fatalf("catalog without the field: %v", err)
	}
}

// A chat without a model works with the catalog's default, so every reader of the image capability has
// to resolve the identifier the same way (issue #163).
func TestCatalogChatModel(t *testing.T) {
	cat, err := ParseCatalog([]byte(`{"default":"p/bild","providers":[{"id":"p","models":[
		{"id":"bild","input":["text","image"]},{"id":"text","input":["text"]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cat.ChatModel(""); got != "p/bild" {
		t.Errorf("without a model: %q, want the default p/bild", got)
	}
	if got := cat.ChatModel("p/text"); got != "p/text" {
		t.Errorf("with a model: %q, want the chat's own p/text", got)
	}
	if _, m, ok := cat.Lookup(cat.ChatModel("")); !ok || !m.ReadsImages() {
		t.Errorf("the default is not the image model: %v %v", m.Input, ok)
	}
}
