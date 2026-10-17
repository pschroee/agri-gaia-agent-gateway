// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package imagereview sends pictures to the model of a chat for a second, uncertain view (V3d, R4). It is
// the one client the gateway asks a provider about images with: the pre-annotation and the result check
// both go through it, each with its own fixed prompt. The pictures go straight from the gateway to the
// provider of the model, not through the sandbox's proxy: the call is not part of the agent's work, and
// the caller writes it down as an auxiliary call of the chat.
//
// The client carries no provider-specific extras. What the model sees and what comes back out of it is
// the caller's fixed prompt: what stands in a picture is data, never an instruction.
package imagereview

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agw/internal/config"
)

const (
	// maxTokens bounds one answer of the model. It is generous on purpose: a model that thinks before it
	// answers spends tokens before the JSON comes (V3d, to measure live).
	maxTokens = 2048
	// timeout bounds one model call.
	timeout = 180 * time.Second
)

// ErrUnsupported is the error of a provider whose API kind the client cannot speak to.
var ErrUnsupported = errors.New("API kind not supported for the image review")

// Image is one picture the client sends: the number the answer refers to, the picture as JPEG and the
// line of the prompt that explains it.
type Image struct {
	// ID is the number the answer refers to: the frame of a task (V3d) or the place of a validation
	// image of a check (R4). The caller reads the answer against the IDs of the call.
	ID   int
	JPEG []byte
	// Line is the line of the prompt for this image; the client sends all lines first, then the pictures
	// in the same order, so that an answer can be read against the request even when the model answers in
	// another order.
	Line string
}

// Answer is what came back from one model call, including what it cost. The content is the model's raw
// answer: only the caller's fixed JSON reader decides what of it is read.
type Answer struct {
	Content string
	// Model is the id of the chat's model ("provider/model"), as the catalog knows it.
	Model    string
	Status   int
	Usage    config.Usage
	Cost     float64
	Peak     bool
	Started  time.Time
	Duration time.Duration
}

// Client asks the chat's model about images. Like the title call it goes from the gateway straight to the
// provider of the model, not through the sandbox's proxy.
type Client struct {
	cat  *config.Catalog
	http *http.Client
}

// NewClient builds the client of the chat's model.
func NewClient(cat *config.Catalog) *Client {
	return &Client{cat: cat, http: &http.Client{Timeout: timeout}}
}

// Ask asks the model with the given fixed system prompt about the images of this call and answers the raw
// content. It sends every picture it is given; the caller chooses the batch (V3d: four, R4: four).
func (c *Client) Ask(ctx context.Context, chatModel, system string, images []Image) (Answer, error) {
	res := Answer{Model: chatModel, Started: time.Now()}
	if c.cat == nil {
		return res, errors.New("no model catalog is set up")
	}
	if len(images) == 0 {
		return res, errors.New("no image to look at")
	}
	prov, model, ok := c.cat.Lookup(chatModel)
	if !ok {
		return res, fmt.Errorf("model %q not in the catalog", chatModel)
	}
	if prov.API != "openai-completions" {
		return res, fmt.Errorf("%w: %s", ErrUnsupported, prov.API)
	}
	lines := make([]string, 0, len(images))
	parts := make([]any, 0, len(images)+1)
	for _, img := range images {
		lines = append(lines, img.Line)
	}
	parts = append(parts, map[string]any{"type": "text", "text": strings.Join(lines, "\n")})
	for _, img := range images {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{
			"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(img.JPEG)}})
	}
	req := map[string]any{
		"model":       model.ID,
		"max_tokens":  maxTokens,
		"temperature": 0,
		"stream":      false,
		"messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": parts},
		},
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(prov.Upstream, "/")+"/chat/completions",
		bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if key := prov.APIKey(); key != "" {
		hr.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.http.Do(hr)
	res.Duration = time.Since(res.Started)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return res, err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens         int64 `json:"prompt_tokens"`
			CompletionTokens     int64 `json:"completion_tokens"`
			PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
			PromptTokensDetails  *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return res, fmt.Errorf("the answer of the model is not readable (HTTP %d)", resp.StatusCode)
	}
	// Tokens as at the proxy: input without cache hits.
	u := out.Usage
	cached := u.PromptCacheHitTokens
	if cached == 0 && u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	res.Usage = config.Usage{Input: u.PromptTokens - cached, Output: u.CompletionTokens, CacheRead: cached}
	if cost, peak, ok := c.cat.Cost(chatModel, res.Usage, res.Started); ok {
		res.Cost, res.Peak = cost, peak
	}
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("the model answers HTTP %d", resp.StatusCode)
	}
	if len(out.Choices) == 0 {
		return res, errors.New("the model answered without content")
	}
	res.Content = out.Choices[0].Message.Content
	return res, nil
}
