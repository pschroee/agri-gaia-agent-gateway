// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/config"
	"agw/internal/store"
)

// The image review of a training check (R4) is tested without a database, a container or a platform: the
// service gets a catalog that points at a fake model upstream, and the checks hand in what a measurement
// would leave behind (the images and their predictions).

// reviewImageNumber finds the places a request names, so that the fake model can answer about exactly the
// images it was shown.
var reviewImageNumber = regexp.MustCompile(`image (\d+):`)

// reviewUpstream is a fake provider of the chat's model: it records every request and answers what the
// test tells it to.
type reviewUpstream struct {
	srv *httptest.Server

	mu     sync.Mutex
	calls  int
	bodies []string
	reply  func(call int, body string) string
}

func newReviewUpstream(t *testing.T) *reviewUpstream {
	t.Helper()
	u := &reviewUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.calls++
		call := u.calls
		u.bodies = append(u.bodies, string(body))
		reply := u.reply
		u.mu.Unlock()
		answer := reviewVerdicts(string(body), "model_wrong")
		if reply != nil {
			answer = reply(call, string(body))
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%s}}],"usage":{"prompt_tokens":500,"completion_tokens":40}}`,
			strconv.Quote(answer))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *reviewUpstream) callsMade() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *reviewUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.bodies...)
}

// reviewVerdicts answers one verdict per place the request names.
func reviewVerdicts(body, verdict string) string {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	if len(req.Messages) > 1 {
		_ = json.Unmarshal(req.Messages[1].Content, &parts)
	}
	images := []string{}
	for _, p := range parts {
		if p.Type == "text" {
			for _, m := range reviewImageNumber.FindAllStringSubmatch(p.Text, -1) {
				images = append(images, m[1])
			}
		}
	}
	entries := make([]string, 0, len(images))
	for _, id := range images {
		entries = append(entries, fmt.Sprintf(`{"image":%s,"verdict":%q,"remark":"test"}`, id, verdict))
	}
	return `{"images":[` + strings.Join(entries, ",") + `]}`
}

// reviewService builds the check service of the tests with the given catalog and the default rule.
func reviewService(t *testing.T, cat *config.Catalog, aux *[]store.AuxCall) *Service {
	t.Helper()
	pattern, err := VisionPattern("")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Cat: cat, VisionPattern: pattern}
	if aux != nil {
		svc.Aux = func(_ context.Context, call store.AuxCall) { *aux = append(*aux, call) }
	}
	return svc
}

// A chat that names no model runs with the catalog's default; the image review resolves the identifier
// the same way the capability overview does (config.Catalog.ChatModel, issue #163).
func TestChatModelResolvesCatalogDefault(t *testing.T) {
	cat := &config.Catalog{Default: "ds/flash", Providers: []config.Provider{{ID: "ds", Models: []config.Model{
		{ID: "flash", Name: "Flash", Input: []string{"text", "image"}},
		{ID: "text", Input: []string{"text"}}}}}}
	if got := (&Service{Cat: cat, ChatModel: func(context.Context, string) string { return "" }}).chatModel(context.Background(), "c1"); got != "ds/flash" {
		t.Errorf("without a model: %q, want the catalog default ds/flash", got)
	}
	if got := (&Service{Cat: cat, ChatModel: func(context.Context, string) string { return "ds/text" }}).chatModel(context.Background(), "c1"); got != "ds/text" {
		t.Errorf("with a model: %q, want the chat's own ds/text", got)
	}
	// Without a catalog nothing is resolved, and the review reports why it did not look at anything.
	if got := (&Service{}).chatModel(context.Background(), "c1"); got != "" {
		t.Errorf("without a catalog: %q, want empty", got)
	}
}

// reviewCatalog points at the fake upstream: a cloud model that reads images, a text-only model and a
// self-hosted model that reads images.
func reviewCatalog(upstreamURL string) *config.Catalog {
	return &config.Catalog{Providers: []config.Provider{
		{ID: "ds", Upstream: upstreamURL, API: "openai-completions", Models: []config.Model{
			{ID: "flash", Name: "Flash", Input: []string{"text", "image"},
				Pricing: &config.Pricing{Input: 0.14, Output: 0.28, Currency: "USD"}},
			{ID: "text", Name: "Text", Input: []string{"text"}},
		}},
		{ID: "own", Upstream: upstreamURL, API: "openai-completions", Models: []config.Model{
			{ID: "vision", Name: "Vision", Input: []string{"text", "image"}, SelfHosted: true},
		}},
	}}
}

// reviewPNG is a small valid picture of one colour; the review decodes and re-encodes it.
func reviewPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// reviewSet builds n images of two classes; wrong[i] says that the model answers the other class.
func reviewSet(t *testing.T, n int, wrong func(i int) bool) ([]checkImage, map[string]Prediction) {
	return reviewSetFrom(t, n, 20, wrong)
}

// reviewSetFrom builds the same set with another colour offset, so that two sets do not share an image.
func reviewSetFrom(t *testing.T, n, base int, wrong func(i int) bool) ([]checkImage, map[string]Prediction) {
	t.Helper()
	measured := make([]checkImage, 0, n)
	byID := make(map[string]Prediction, n)
	for i := 0; i < n; i++ {
		class, top := "0", 0
		if i%2 == 1 {
			class, top = "1", 1
		}
		if wrong(i) {
			top = 1 - top
		}
		data := reviewPNG(t, color.RGBA{R: uint8(base + i), G: 100, B: 200, A: 255})
		sha := Sha256Hex(data)
		measured = append(measured, checkImage{Item: Item{SHA: sha, Class: class}, Data: data, Ext: ".png"})
		byID[sha] = Prediction{ID: sha, OK: true, Top: top, Probs: []float64{0.1, 0.9}}
	}
	return measured, byID
}

// reviewInput is the judgement input of an interim check with a clear class order.
func reviewInput(name string) JudgeInput {
	return JudgeInput{
		Kind: KindTrainingModel, Final: false,
		EvalDataset:      DatasetRef{ID: 41, Name: name},
		ValidationImages: 12, HoldoutImages: 24,
		Outputs: 2, Mapping: map[int]string{0: "0", 1: "1"}, MappingOK: true,
	}
}

// A cloud model may look at a dataset whose name matches the pattern, not at another one (R4.2, #111).
func TestReviewRuleCloud(t *testing.T) {
	up := newReviewUpstream(t)
	var aux []store.AuxCall
	svc := reviewService(t, reviewCatalog(up.srv.URL), &aux)
	measured, byID := reviewSet(t, 4, func(i int) bool { return i < 3 })
	in := reviewInput("mnist-mini")

	got := svc.reviewMisclassified(context.Background(), "", "c1", "ds/flash", time.Now(), in, measured, byID)
	if !got.ImageReview.Done || got.ImageReview.Model != "ds/flash" {
		t.Fatalf("image review: %+v", got.ImageReview)
	}
	if got.ImageReview.Of != 3 || got.ImageReview.Checked != 3 || got.ImageReview.Flagged != 0 {
		t.Errorf("counts: %+v", got.ImageReview)
	}
	if up.callsMade() != 1 {
		t.Errorf("3 images took %d calls, expected 1", up.callsMade())
	}
	if len(aux) != 1 || aux[0].Purpose != "image_review" || aux[0].Model != "ds/flash" {
		t.Errorf("the review call was not billed as an auxiliary call: %+v", aux)
	}
	body := up.requests()[0]
	if !strings.Contains(body, `the label is \"0\"`) || !strings.Contains(body, `the model says \"1\"`) {
		t.Errorf("the prompt does not name label and prediction: %s", body)
	}
	// The default answer says model_wrong for every image: no label looks wrong.
	joined := strings.Join(got.Hints, " ")
	if !strings.Contains(joined, "3 like model errors") {
		t.Errorf("the hint does not count the model errors: %v", got.Hints)
	}
	if len(got.Suggestions) != 0 {
		t.Errorf("model errors suggest a label fix: %+v", got.Suggestions)
	}

	// The same cloud model may not see a private dataset.
	up2 := newReviewUpstream(t)
	svc2 := reviewService(t, reviewCatalog(up2.srv.URL), nil)
	got = svc2.reviewMisclassified(context.Background(), "", "c1", "ds/flash", time.Now(), reviewInput("schweine-2026"), measured, byID)
	if got.ImageReview.Done || got.ImageReview.Reason != reasonCloudDataset {
		t.Fatalf("a private dataset went to the cloud model: %+v", got.ImageReview)
	}
	if up2.callsMade() != 0 {
		t.Errorf("the model was called although the rule forbids it")
	}
}

// A self-hosted model may see every dataset (R4.2, #111).
func TestReviewSelfHosted(t *testing.T) {
	up := newReviewUpstream(t)
	svc := reviewService(t, reviewCatalog(up.srv.URL), nil)
	measured, byID := reviewSet(t, 2, func(i int) bool { return true })
	got := svc.reviewMisclassified(context.Background(), "", "c1", "own/vision", time.Now(),
		reviewInput("schweine-2026"), measured, byID)
	if !got.ImageReview.Done || up.callsMade() != 1 {
		t.Fatalf("a self-hosted model did not look at the images: %+v, calls %d", got.ImageReview, up.callsMade())
	}
}

// A model that reads text only never looks at the images, and the report carries the reason of R0.9.
func TestReviewWithoutVision(t *testing.T) {
	up := newReviewUpstream(t)
	svc := reviewService(t, reviewCatalog(up.srv.URL), nil)
	measured, byID := reviewSet(t, 2, func(i int) bool { return true })
	got := svc.reviewMisclassified(context.Background(), "", "c1", "ds/text", time.Now(),
		reviewInput("mnist-mini"), measured, byID)
	if got.ImageReview.Done || got.ImageReview.Reason != reasonTextOnly {
		t.Fatalf("image review: %+v", got.ImageReview)
	}
	if up.callsMade() != 0 {
		t.Errorf("a text-only model was asked about images")
	}
}

// Only an interim check reviews validation images; a final check never shows its hold-out images, and the
// review takes at most ReviewMaxMisclassified misclassified images (R4.1).
func TestReviewOnlyValidation(t *testing.T) {
	up := newReviewUpstream(t)
	svc := reviewService(t, reviewCatalog(up.srv.URL), nil)
	measured, byID := reviewSet(t, 20, func(i int) bool { return true })
	in := reviewInput("mnist-mini")

	// A final check returns before any call.
	got := svc.reviewMisclassified(context.Background(), "", "c1", "ds/flash", time.Now(), in.withFinal(true), measured, byID)
	if got.ImageReview.Done || got.ImageReview.Reason != imageReviewReason(true) {
		t.Fatalf("a final check reviewed images: %+v", got.ImageReview)
	}
	if up.callsMade() != 0 {
		t.Fatalf("a final check called the model")
	}

	// An interim check looks at the misclassified images it was given, 12 of them at most.
	got = svc.reviewMisclassified(context.Background(), "", "c1", "ds/flash", time.Now(), in, measured, byID)
	if !got.ImageReview.Done || got.ImageReview.Of != ReviewMaxMisclassified {
		t.Fatalf("the review did not take %d images: %+v", ReviewMaxMisclassified, got.ImageReview)
	}
	if got.ImageReview.Checked != ReviewMaxMisclassified {
		t.Errorf("checked %d of %d", got.ImageReview.Checked, got.ImageReview.Of)
	}
	if up.callsMade() != 3 {
		t.Errorf("12 images took %d calls, expected 3", up.callsMade())
	}
}

// withFinal is the same input with the other kind of check.
func (in JudgeInput) withFinal(final bool) JudgeInput {
	in.Final = final
	return in
}

// The review only adds a hint and a suggestion; it never makes the verdict better (R4.1).
func TestReviewNeverImproves(t *testing.T) {
	up := newReviewUpstream(t)
	svc := reviewService(t, reviewCatalog(up.srv.URL), nil)
	// Six of twelve validation images are wrong: 0.50 against a majority baseline of 0.50.
	measured, byID := reviewSet(t, 12, func(i int) bool { return i%2 == 0 })
	in := reviewInput("mnist-mini")
	sample, samplePreds := reviewSetFrom(t, 12, 100, func(int) bool { return false })
	for sha, p := range samplePreds {
		byID[sha] = p
	}
	seen := map[string]Item{}
	for _, s := range sample {
		seen[s.SHA] = s.Item
	}
	out := Output{Predictions: make([]Prediction, 0, len(sample)+len(measured))}
	out.Model = ModelInfo{Outputs: []TensorInfo{{Shape: []int{1, 2}}}}
	for _, p := range byID {
		out.Predictions = append(out.Predictions, p)
	}

	// The same run without a review is the control.
	control := svc.judgeRun(context.Background(), "c1", "", "", time.Now(), in, out, sample, measured, seen, 0)
	if control.Verdict != Questionable {
		t.Fatalf("the control check is %s, not questionable: %v", control.Verdict, control.Reasons)
	}
	up.reply = func(_ int, body string) string { return reviewVerdicts(body, "label_wrong") }
	reviewed := svc.judgeRun(context.Background(), "c1", "", "ds/flash", time.Now().Add(-time.Second),
		in, out, sample, measured, seen, 0)
	if reviewed.Verdict != control.Verdict || strings.Join(reviewed.Reasons, "|") != strings.Join(control.Reasons, "|") {
		t.Fatalf("the image review changed the verdict: %s %v against %s %v",
			reviewed.Verdict, reviewed.Reasons, control.Verdict, control.Reasons)
	}
	if !reviewed.ImageReview.Done || reviewed.ImageReview.Flagged != reviewed.ImageReview.Checked {
		t.Fatalf("image review: %+v", reviewed.ImageReview)
	}
	if len(reviewed.Hints) != len(control.Hints)+1 {
		t.Errorf("the review added %d hints, expected one: %v", len(reviewed.Hints)-len(control.Hints), reviewed.Hints)
	}
	if !strings.Contains(strings.Join(reviewed.Hints, " "), "look mislabelled") {
		t.Errorf("the hint lacks the labels: %v", reviewed.Hints)
	}
	if ids := suggestionIDs(reviewed); !containsString(ids, "fix_labels") {
		t.Errorf("no fix_labels suggestion: %v", ids)
	}
	reviewed.Finish()
	if reviewed.Header != "Result: questionable (validation images, interim, image review with ds/flash)" {
		t.Errorf("header: %q", reviewed.Header)
	}

	// A usable result stays usable although the review finds mislabelled images.
	usable, usablePreds := reviewSet(t, 12, func(int) bool { return false })
	for sha, p := range samplePreds {
		byID[sha] = p
	}
	for sha, p := range usablePreds {
		byID[sha] = p
	}
	out.Predictions = out.Predictions[:0]
	for _, p := range byID {
		out.Predictions = append(out.Predictions, p)
	}
	rep := svc.judgeRun(context.Background(), "c1", "", "ds/flash", time.Now(), in, out, sample, usable, seen, 0)
	if rep.Verdict != Usable {
		t.Fatalf("the review made a usable result %s: %v", rep.Verdict, rep.Reasons)
	}
}

// Only the fixed JSON is read: a fence is dropped, an unknown verdict, a foreign place and a second answer
// for the same place are not, and the remark is cut (R4).
func TestReadClassificationVerdicts(t *testing.T) {
	long := strings.Repeat("x", reviewRemarkRunes+50)
	answer := "Here is my answer:\n```json\n{\"note\":\"extra\",\"images\":[" +
		`{"image":1,"verdict":"label_wrong","remark":"` + long + `","confidence":0.9},` +
		`{"image":2,"verdict":"MODEL_WRONG"},` +
		`{"image":3,"verdict":"looks_odd"},` +
		`{"image":4,"verdict":"unclear_image"},` +
		`{"image":1,"verdict":"model_wrong"}` +
		"]}\n```"
	got, ok := readClassificationVerdicts(answer, []int{1, 2, 4})
	if !ok {
		t.Fatal("the answer of the model was not read")
	}
	if len(got) != 3 {
		t.Fatalf("verdicts: %+v", got)
	}
	if got[0].ID != 1 || got[0].Verdict != "label_wrong" {
		t.Errorf("the first verdict: %+v", got[0])
	}
	if r := []rune(got[0].Remark); len(r) != reviewRemarkRunes+1 || !strings.HasSuffix(got[0].Remark, "…") {
		t.Errorf("the remark was not cut to %d characters: %d", reviewRemarkRunes, len(r))
	}
	if got[1].ID != 2 || got[1].Verdict != "model_wrong" {
		t.Errorf("the verdict is not lower-cased and read: %+v", got[1])
	}
	if _, ok := readClassificationVerdicts("I cannot see any image here.", []int{1}); ok {
		t.Error("a sentence without JSON was read as an answer")
	}
	if _, ok := readClassificationVerdicts(`{"verdicts":[]}`, []int{1}); ok {
		t.Error("JSON without the images field was read as an answer")
	}
}

// The check card reads the steps of a running check by these names (R4, L12).
func TestProgressStepNames(t *testing.T) {
	raw, err := json.Marshal(Progress{Set: SetValidation, Images: 12, ReviewModel: "ds/flash",
		Steps: []ProgressStep{{Name: "statistics", Done: true, Seconds: 2.5}, {Name: "image_review", K: 3, M: 12}}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{`"review_model":"ds/flash"`, `"name":"statistics"`, `"name":"image_review"`, `"k":3`, `"m":12`} {
		if !strings.Contains(text, want) {
			t.Errorf("the progress lacks %s: %s", want, text)
		}
	}
}

// A review whose answer is not the fixed JSON counts no image, and the call is still billed (R4).
func TestReviewUnreadableAnswer(t *testing.T) {
	up := newReviewUpstream(t)
	var aux []store.AuxCall
	svc := reviewService(t, reviewCatalog(up.srv.URL), &aux)
	up.reply = func(int, string) string { return "I am sorry, I cannot judge these pictures." }
	measured, byID := reviewSet(t, 6, func(i int) bool { return true })
	got := svc.reviewMisclassified(context.Background(), "", "c1", "ds/flash", time.Now(),
		reviewInput("mnist-mini"), measured, byID)
	if got.ImageReview.Done || got.ImageReview.Checked != 0 ||
		got.ImageReview.Reason != "the model's answer about the images was not the fixed JSON" {
		t.Fatalf("an unreadable answer counted as a review: %+v", got.ImageReview)
	}
	if len(aux) != 2 {
		t.Errorf("6 images were billed as %d calls, expected 2", len(aux))
	}
	for _, call := range aux {
		if call.Error == "" {
			t.Error("a call whose answer was not readable was written down without its error")
		}
	}
}
