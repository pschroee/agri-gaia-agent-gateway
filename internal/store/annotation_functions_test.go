// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
)

// The register of the functions built from a model (V2): the classes survive the round trip, the
// listing belongs to the user (not to one chat) and the state is written by name.
func TestAnnotationFunctions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "a", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateChat(ctx, NewChat{Title: "b", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.AddAnnotationFunction(ctx, AnnotationFunction{
		Name: "agw-test-m5-ab12cd34", ChatID: c.ID, Username: "test", ModelID: 5,
		ModelSHA256: "ab12cd34ff", Kind: "classification", Labels: []string{"class_0", "class_1"},
		InputSize: 64, TemplateVersion: 1, PlatformTask: "/tasks/7"})
	if err != nil {
		t.Fatal(err)
	}
	if f.State != AnnotationFunctionDeploying || f.CreatedAt.IsZero() {
		t.Fatalf("new function: %+v", f)
	}
	got, err := s.AnnotationFunctionsOf(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != f.Name || got[0].PlatformTask != "/tasks/7" ||
		len(got[0].Labels) != 2 || got[0].Labels[1] != "class_1" || got[0].InputSize != 64 {
		t.Fatalf("read: %+v", got)
	}
	// The listing belongs to the user, not to the chat: a row of another chat of the same user is in
	// it, a row of another user is not.
	if _, err := s.AddAnnotationFunction(ctx, AnnotationFunction{
		Name: "agw-test-m7-00112233", ChatID: other.ID, Username: "test", ModelID: 7,
		ModelSHA256: "00112233ff", Kind: "detection", Labels: []string{"pig"}, InputSize: 640, TemplateVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAnnotationFunction(ctx, AnnotationFunction{
		Name: "agw-other-m5-ab12cd34", ChatID: c.ID, Username: "other", ModelID: 5,
		ModelSHA256: "ab12cd34ff", Kind: "classification", Labels: []string{"0"}, InputSize: 224, TemplateVersion: 1}); err != nil {
		t.Fatal(err)
	}
	got, err = s.AnnotationFunctionsOf(ctx, "test")
	if err != nil || len(got) != 2 {
		t.Fatalf("two functions: %+v %v", got, err)
	}
	if other, err := s.AnnotationFunctionsOf(ctx, "nobody"); err != nil || len(other) != 0 {
		t.Fatalf("a user without functions: %+v %v", other, err)
	}
	// The state is written by name, also for a row another chat built.
	if err := s.SetAnnotationFunctionState(ctx, "agw-test-m7-00112233", AnnotationFunctionReady); err != nil {
		t.Fatal(err)
	}
	got, _ = s.AnnotationFunctionsOf(ctx, "test")
	if got[1].State != AnnotationFunctionReady || got[0].State != AnnotationFunctionDeploying {
		t.Fatalf("states: %+v", got)
	}
	// Deleting a chat takes the rows built in it along; the function of the other chat stays.
	if _, err := s.DeleteChatAnonymised(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.AnnotationFunctionsOf(ctx, "test")
	if err != nil || len(got) != 1 || got[0].Name != "agw-test-m7-00112233" {
		t.Fatalf("after deleting a chat: %+v %v", got, err)
	}
}

// A row carries the columns of the build: the state defaults to deploying, the classes are a JSON
// list even when there are none.
func TestAnnotationFunctionDefaults(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "t", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.AddAnnotationFunction(ctx, AnnotationFunction{
		Name: "agw-test-m5-ab12cd34", ChatID: c.ID, Username: "test", ModelID: 5,
		ModelSHA256: "ab12cd34ff", Kind: "classification", InputSize: 224, TemplateVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if f.State != AnnotationFunctionDeploying || f.PlatformTask != "" {
		t.Fatalf("defaults: %+v", f)
	}
	got, err := s.AnnotationFunctionsOf(ctx, "test")
	if err != nil || len(got) != 1 || got[0].Labels == nil || len(got[0].Labels) != 0 {
		t.Fatalf("classes: %+v %v", got, err)
	}
}

// The same model gives the same name: building it again after a failure refreshes its row (state
// deploying, the new task, the chat of the new build) instead of failing on the primary key.
func TestAnnotationFunctionRebuild(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "a", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateChat(ctx, NewChat{Title: "b", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	build := func(chat, task string) AnnotationFunction {
		f, err := s.AddAnnotationFunction(ctx, AnnotationFunction{
			Name: "agw-test-m5-ab12cd34", ChatID: chat, Username: "test", ModelID: 5,
			ModelSHA256: "ab12cd34ff", Kind: "classification", Labels: []string{"0"}, InputSize: 224,
			TemplateVersion: 1, PlatformTask: task})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if f := build(c.ID, "7"); f.State != AnnotationFunctionDeploying || f.PlatformTask != "7" {
		t.Fatalf("first build: %+v", f)
	}
	if err := s.SetAnnotationFunctionState(ctx, "agw-test-m5-ab12cd34", AnnotationFunctionFailed); err != nil {
		t.Fatal(err)
	}
	if f := build(again.ID, "9"); f.State != AnnotationFunctionDeploying || f.PlatformTask != "9" || f.ChatID != again.ID {
		t.Fatalf("rebuild: %+v", f)
	}
	got, err := s.AnnotationFunctionsOf(ctx, "test")
	if err != nil || len(got) != 1 || got[0].PlatformTask != "9" || got[0].ChatID != again.ID || got[0].State != AnnotationFunctionDeploying {
		t.Fatalf("after the rebuild: %+v %v", got, err)
	}
}
