// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"testing"

	"agw/internal/config"
)

// The image review rule of #111 (P14): a self hosted model may see every dataset, a cloud model only
// the public ones, and a text model none.
func TestImageReviewAllowed(t *testing.T) {
	pattern, err := VisionPattern("")
	if err != nil {
		t.Fatal(err)
	}
	vision := config.Model{ID: "local/vision", Input: []string{"text", "image"}}
	cloud := config.Model{ID: "cloud/vision", Input: []string{"text", "image"}}
	text := config.Model{ID: "cloud/text", Input: []string{"text"}}
	selfHosted := config.Model{ID: "local/vision", Input: []string{"text", "image"}, SelfHosted: true}

	if ok, why := ImageReviewAllowed(selfHosted, "schweine-2026", pattern); !ok {
		t.Errorf("self hosted on a private dataset: %v", why)
	}
	if ok, why := ImageReviewAllowed(vision, "schweine-2026", pattern); ok || why == "" {
		t.Error("a cloud model may see images of a private dataset")
	}
	if ok, _ := ImageReviewAllowed(cloud, "mnist-mini", pattern); !ok {
		t.Error("a cloud model may not see a public test set")
	}
	if ok, _ := ImageReviewAllowed(cloud, "test-set", pattern); !ok {
		t.Error("a cloud model may not see a public test set")
	}
	if ok, why := ImageReviewAllowed(text, "mnist-mini", pattern); ok || why != "the current model reads text only" {
		t.Errorf("a text model: %v %q", ok, why)
	}
	if ok, why := ImageReviewAllowed(text, "schweine-2026", pattern); ok || why != "the current model reads text only" {
		t.Errorf("a text model on a private dataset: %v %q", ok, why)
	}
	// The reason of the refusal names the variable, so that the user can act on it.
	if _, why := ImageReviewAllowed(cloud, "schweine-2026", pattern); why != "images of this dataset may not go to the cloud model (AGW_VISION_DATASET_PATTERN)" {
		t.Errorf("reason: %q", why)
	}
	// A pattern of the instance replaces the default, and a broken one is a configuration error.
	own, err := VisionPattern(`^public-`)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := ImageReviewAllowed(cloud, "public-cows", own); !ok {
		t.Error("the configured pattern does not apply")
	}
	if ok, _ := ImageReviewAllowed(cloud, "mnist-mini", own); ok {
		t.Error("the default pattern still applies next to the configured one")
	}
	if _, err := VisionPattern(`(`); err == nil {
		t.Error("a broken pattern is accepted")
	}
	// Without a pattern (unset) only a self hosted model may see images.
	if ok, why := ImageReviewAllowed(cloud, "mnist-mini", nil); ok || why == "" {
		t.Error("without a pattern a cloud model may see images")
	}
	if ok, _ := ImageReviewAllowed(selfHosted, "mnist-mini", nil); !ok {
		t.Error("a self hosted model needs no pattern")
	}
}
