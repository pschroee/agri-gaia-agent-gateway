// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"agw/internal/assess"
	"agw/internal/config"
	"agw/internal/store"
)

// The image review is tested without a database and without a platform: the service gets a fake CVAT, a
// fake model upstream and the catalog that points at it. Everything else the review works from is what a
// verification hands in (V3d).

// reviewJobID is the job the tests read their frames from.
const reviewJobID = 7

// frameInLine finds the frames the lines of a request name, so that a test's fake model can answer about
// exactly the images it was shown.
var frameInLine = regexp.MustCompile(`frame (\d+):`)

// reviewTest is the service of the image review with its two fakes.
type reviewTest struct {
	svc     *Service
	cvat    *fakeCVAT
	pattern *regexp.Regexp
	// reply builds the answer of one model call; nil answers one ok verdict per frame of that call.
	reply func(call int, body string) string

	mu     sync.Mutex
	calls  int
	bodies []string
	aux    []store.AuxCall
}

// newReviewTest builds the service of the tests: a model upstream that answers like the fake, and a CVAT
// that serves whatever pictures a test puts into it.
func newReviewTest(t *testing.T) *reviewTest {
	t.Helper()
	rt := &reviewTest{cvat: &fakeCVAT{}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rt.mu.Lock()
		rt.calls++
		call := rt.calls
		rt.bodies = append(rt.bodies, string(body))
		reply := rt.reply
		rt.mu.Unlock()
		answer := okVerdicts(string(body))
		if reply != nil {
			answer = reply(call, string(body))
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%s}}],"usage":{"prompt_tokens":1000,"completion_tokens":120}}`,
			strconv.Quote(answer))
	}))
	t.Cleanup(upstream.Close)
	cat := &config.Catalog{Providers: []config.Provider{{
		ID: "ds", Upstream: upstream.URL, API: "openai-completions",
		Models: []config.Model{
			{ID: "flash", Name: "Flash", Input: []string{"text", "image"},
				Pricing: &config.Pricing{Input: 0.14, Output: 0.28, Currency: "USD"}},
			{ID: "text", Name: "Text", Input: []string{"text"}},
			{ID: "own", Name: "Own", Input: []string{"text", "image"}, SelfHosted: true},
		},
	}}}
	pattern, err := assess.VisionPattern("")
	if err != nil {
		t.Fatal(err)
	}
	cvatSrv := httptest.NewServer(rt.cvat)
	t.Cleanup(cvatSrv.Close)
	c, err := NewCVAT(cvatSrv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt.pattern = pattern
	rt.svc = &Service{C: c, Cat: cat, VisionPattern: pattern,
		Aux: func(_ context.Context, call store.AuxCall) { rt.aux = append(rt.aux, call) }}
	return rt
}

// modelCalls are the request bodies the model upstream saw.
func (rt *reviewTest) modelCalls() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.bodies...)
}

// auxCalls are the auxiliary calls the review was billed as.
func (rt *reviewTest) auxCalls() []store.AuxCall {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]store.AuxCall(nil), rt.aux...)
}

// review runs the image review of one task: the chat, the model, the dataset, the task, the session and
// the jobs are the same for every test, only what is annotated is what a test hands in.
func (rt *reviewTest) review(t *testing.T, chatModel, dataset string, in reviewInput) reviewOutcome {
	t.Helper()
	in.ChatID, in.ChatModel, in.DatasetName = "c1", chatModel, dataset
	in.TaskID = 9
	in.Session = session{key: "k", csrf: "c", sessionID: "s"}
	in.Jobs = []Job{{ID: reviewJobID, StartFrame: 0, StopFrame: 999}}
	if in.Classes == nil {
		in.Classes = []string{"0"}
	}
	if in.Holdout == nil {
		in.Holdout = map[int]bool{}
	}
	return rt.svc.imageReview(context.Background(), in)
}

// addFrames gives the fake a picture for every frame and answers the automatic annotation of a
// classification: every frame carries the class "0".
func (rt *reviewTest) addFrames(t *testing.T, frames ...int) map[int][]AutoObject {
	t.Helper()
	auto := make(map[int][]AutoObject, len(frames))
	for _, frame := range frames {
		raw := picture(t, frame)
		rt.cvat.setFrame("job", reviewJobID, frame, raw)
		auto[frame] = []AutoObject{{Class: "0", Type: "tag"}}
	}
	return auto
}

// picture is a small JPEG a test serves as one frame of a task; every second frame is a shade darker, so
// that a test can tell two pictures apart.
func picture(t *testing.T, frame int) []byte {
	t.Helper()
	colour := white
	if frame%2 == 1 {
		colour = color.RGBA{R: 180, G: 180, B: 180, A: 255}
	}
	img := flatPicture(64, 64, colour)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// okVerdicts answers one ok verdict per frame of a call, read out of the lines of the request.
func okVerdicts(body string) string {
	seen := map[int]bool{}
	var found []int
	for _, m := range frameInLine.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		found = append(found, n)
	}
	parts := make([]string, 0, len(found))
	for _, n := range found {
		parts = append(parts, fmt.Sprintf(`{"frame":%d,"ok":true}`, n))
	}
	return `{"images":[` + strings.Join(parts, ",") + `]}`
}

// judgedAnswer answers one complaint per frame of a call.
func judgedAnswer(code string) func(int, string) string {
	return func(_ int, body string) string {
		ok := okVerdicts(body)
		return strings.ReplaceAll(ok, `"ok":true`, fmt.Sprintf(`"ok":false,"problems":[%q]`, code))
	}
}

// addFramesIn adds the frames from..to to the fake and answers their automatic annotation.
func (rt *reviewTest) addFramesIn(t *testing.T, from, to int) map[int][]AutoObject {
	t.Helper()
	var frames []int
	for f := from; f <= to; f++ {
		frames = append(frames, f)
	}
	return rt.addFrames(t, frames...)
}

// requestText reads the text part and the image parts out of one model request of the review.
func requestText(t *testing.T, body string) (string, []string) {
	t.Helper()
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("the request of the review is not readable: %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[1].Role != "user" {
		t.Fatalf("the review did not send the system prompt and one user message: %+v", req.Messages)
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(req.Messages[1].Content, &parts); err != nil {
		t.Fatalf("the content of the review is not readable: %v", err)
	}
	var text string
	var images []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			text = p.Text
		case "image_url":
			images = append(images, p.ImageURL.URL)
		default:
			t.Fatalf("the review sent a part of type %q", p.Type)
		}
	}
	return text, images
}

// The review sends the pictures themselves, in the OpenAI form the providers read, at most ReviewBatch
// per call, and each shrunk to ReviewImageSide (V3d).
func TestReviewSendsImages(t *testing.T) {
	rt := newReviewTest(t)
	big, err := RenderFrame(flatPicture(1024, 600, white),
		[]Box{{Colour: "red", Points: []float64{100, 100, 300, 500}}}, ReviewImageSide)
	if err != nil {
		t.Fatal(err)
	}
	rv := NewReviewer(rt.svc.Cat)
	res, err := rv.Review(context.Background(), "ds/flash", []ReviewImage{{Frame: 3, JPEG: big,
		Line: `frame 3: the image is annotated with the class "0" (colour red)`}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Verdicts) != 1 || res.Verdicts[0].Frame != 3 || !res.Verdicts[0].OK {
		t.Fatalf("verdicts: %+v", res.Verdicts)
	}
	if res.Model != "ds/flash" || res.Status != http.StatusOK {
		t.Errorf("result: %+v", res)
	}
	body := rt.modelCalls()[0]
	text, images := requestText(t, body)
	if !strings.Contains(text, "frame 3:") {
		t.Errorf("the line of the image is not in the request: %q", text)
	}
	if len(images) != 1 {
		t.Fatalf("the call carried %d images, expected one", len(images))
	}
	const prefix = "data:image/jpeg;base64,"
	if !strings.HasPrefix(images[0], prefix) {
		t.Fatalf("the picture is not sent as a data URL: %.40s", images[0])
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(images[0], prefix))
	if err != nil {
		t.Fatalf("the picture is not base64: %v", err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the sent picture is not a JPEG: %v", err)
	}
	if cfg.Width != ReviewImageSide || cfg.Height != 300 {
		t.Errorf("the sent picture is %dx%d, expected %dx300", cfg.Width, cfg.Height, ReviewImageSide)
	}
	// Six images do not go out in one call: four do, the caller asks again for the rest.
	six := make([]ReviewImage, 0, 6)
	for f := 0; f < 6; f++ {
		six = append(six, ReviewImage{Frame: f, JPEG: big, Line: fmt.Sprintf("frame %d: nothing is annotated on this image", f)})
	}
	if _, err := rv.Review(context.Background(), "ds/flash", six); err != nil {
		t.Fatal(err)
	}
	_, images = requestText(t, rt.modelCalls()[1])
	if len(images) != ReviewBatch {
		t.Errorf("one call carried %d images, expected at most %d", len(images), ReviewBatch)
	}
}

// The review looks at the images V3b found suspicious first and then at a sample of the annotated ones —
// never at a held-out image (V3d, E22).
func TestReviewSampleNeverHoldout(t *testing.T) {
	auto := map[int][]AutoObject{}
	for frame := 0; frame < 60; frame++ {
		auto[frame] = []AutoObject{{Class: "0"}}
	}
	holdout := map[int]bool{1: true, 31: true, 59: true}
	suspicious := []SuspiciousImage{{Frame: 31}, {Frame: 59}, {Frame: 12}}
	frames := reviewFrames(auto, suspicious, holdout)
	if len(frames) > ReviewMaxImages {
		t.Errorf("the review looks at %d images, at most %d are allowed", len(frames), ReviewMaxImages)
	}
	for _, frame := range frames {
		if holdout[frame] {
			t.Errorf("the review looks at held-out frame %d", frame)
		}
	}
	if len(frames) == 0 || frames[0] != 12 {
		t.Errorf("the suspicious image is not looked at first: %v", frames)
	}
	if duplicates := len(frames) - len(unique(frames)); duplicates != 0 {
		t.Errorf("the review looks at an image twice: %v", frames)
	}
	// At most ReviewSuspicious suspicious images and at most the same number of the annotated ones. The
	// suspicious frames lie outside the annotated ones, so that both bounds show.
	many := make([]SuspiciousImage, 0, 60)
	for frame := 100; frame < 160; frame++ {
		many = append(many, SuspiciousImage{Frame: frame})
	}
	if got := reviewFrames(auto, many, nil); len(got) != ReviewMaxImages {
		t.Errorf("a task with 60 suspicious images gives %d images, expected %d", len(got), ReviewMaxImages)
	}
	if got := reviewFrames(auto, nil, nil); len(got) != ReviewSample {
		t.Errorf("a task with 60 annotated images gives %d images, expected %d", len(got), ReviewSample)
	}
	// Every image of the task held out: there is nothing to look at.
	if got := reviewFrames(map[int][]AutoObject{0: {{Class: "0"}}}, nil, map[int]bool{0: true}); len(got) != 0 {
		t.Errorf("the review looks at held-out images: %v", got)
	}
}

// unique counts the frames of a list once each.
func unique(frames []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(frames))
	for _, f := range frames {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// Only the fixed JSON is read out of the model's answer: a fence around it, foreign fields, problem codes
// the gateway does not know and a remark longer than 200 characters (V3d).
func TestReviewParsesStrictJSON(t *testing.T) {
	frames := []int{5, 6}
	long := strings.Repeat("x", reviewRemarkRunes+50)
	answer := "Here is what I saw:\n```json\n{\"note\":\"extra\",\"images\":[" +
		`{"frame":5,"ok":false,"problems":["bad_box","nonsense","bad_box"],"remark":"` + long + `","confidence":0.9},` +
		`{"frame":6,"ok":true,"problems":["wrong_class"]},` +
		`{"frame":99,"ok":false,"problems":["bad_box"]},` +
		`{"frame":5,"ok":true,"remark":"a second answer for the same frame"}` +
		"]}\n```"
	got, ok := readReviewVerdicts(answer, frames)
	if !ok {
		t.Fatal("the answer of the model was not read")
	}
	if len(got) != 2 {
		t.Fatalf("verdicts: %+v", got)
	}
	if got[0].Frame != 5 || got[0].OK || strings.Join(got[0].Problems, ",") != "bad_box" {
		t.Errorf("the first verdict: %+v", got[0])
	}
	if r := []rune(got[0].Remark); len(r) != reviewRemarkRunes+1 || !strings.HasSuffix(got[0].Remark, "…") {
		t.Errorf("the remark was not cut to %d characters: %d", reviewRemarkRunes, len(r))
	}
	if !got[1].OK || len(got[1].Problems) != 0 {
		t.Errorf("an ok verdict carries a problem: %+v", got[1])
	}
	// A complaint without a code the gateway knows is no verdict at all, and a frame of another call is
	// not read.
	if v, ok := readReviewVerdicts(`{"images":[{"frame":5,"ok":false,"problems":["looks_wrong"]}]}`, frames); !ok || len(v) != 0 {
		t.Errorf("a complaint without a known code counted as a verdict: %+v", v)
	}
	if v, ok := readReviewVerdicts(`{"images":[{"frame":99,"ok":false,"problems":["bad_box"]}]}`, frames); !ok || len(v) != 0 {
		t.Errorf("a verdict about a frame of another call was read: %+v", v)
	}
	// An answer that is not the fixed JSON is not read at all.
	if _, ok := readReviewVerdicts("I cannot see any image here.", frames); ok {
		t.Error("a sentence without JSON was read as an answer")
	}
	if _, ok := readReviewVerdicts(`{"verdicts":[]}`, frames); ok {
		t.Error("JSON without the images field was read as an answer")
	}
}

// An answer that is not the fixed JSON leaves every image unchecked, and the call is billed all the same:
// there is no second try (V3d).
func TestReviewInvalidJSONCountsUnchecked(t *testing.T) {
	rt := newReviewTest(t)
	rt.reply = func(int, string) string { return "I am sorry, I cannot judge these pictures." }
	auto := rt.addFramesIn(t, 0, 11)
	rev := rt.review(t, "ds/flash", "mnist-mini", reviewInput{Auto: auto})
	if rev.ImageReview.Done || rev.ImageReview.Checked != 0 || rev.Flagged != 0 {
		t.Fatalf("an unreadable answer counted as a review: %+v", rev.ImageReview)
	}
	if rev.ImageReview.Reason != "the model's answer about the images was not the fixed JSON" {
		t.Errorf("reason: %q", rev.ImageReview.Reason)
	}
	if len(rt.modelCalls()) != 3 {
		t.Errorf("12 images took %d calls, expected %d", len(rt.modelCalls()), 3)
	}
	calls := rt.auxCalls()
	if len(calls) != 3 {
		t.Fatalf("12 images were billed as %d calls, expected 3", len(calls))
	}
	for _, call := range calls {
		if call.Error == "" {
			t.Error("a call whose answer was not readable was written down without its error")
		}
	}
}

// The image review can only make the verdict worse (V3d): a model that complains about the images turns a
// usable result into a questionable one, and one that complains about nothing leaves it as it is.
func TestReviewOnlyWorsens(t *testing.T) {
	rt := newReviewTest(t)
	rt.reply = judgedAnswer("wrong_class")
	auto := rt.addFramesIn(t, 0, 11)
	rev := rt.review(t, "ds/flash", "mnist-mini", reviewInput{Auto: auto})
	if !rev.ImageReview.Done || rev.ImageReview.Checked != 12 || rev.Flagged != 12 {
		t.Fatalf("the review did not look at twelve images: %+v (%d flagged)", rev.ImageReview, rev.Flagged)
	}
	if len(rev.Suspicious) != 12 {
		t.Fatalf("the flagged images are not in the report: %+v", rev.Suspicious)
	}
	if rev.Suspicious[0].Reason != "image_review:wrong_class" {
		t.Errorf("the reason of a flagged image: %q", rev.Suspicious[0].Reason)
	}
	plain := verdictTask(20, 20)
	if got := Verify(plain).Report.Verdict; got != assess.Usable {
		t.Fatalf("the figures alone are %q, expected usable", got)
	}
	clean := plain
	clean.ImageReview = assess.ImageReview{Done: true, Model: "ds/flash", Checked: 12}
	if got := Verify(clean).Report.Verdict; got != assess.Usable {
		t.Errorf("a review that complains about nothing changed the verdict to %q", got)
	}
	worse := plain
	worse.ImageReview, worse.Flagged = rev.ImageReview, rev.Flagged
	res := Verify(worse)
	if res.Report.Verdict != assess.Questionable {
		t.Fatalf("the image review did not make the verdict worse: %q (%v)", res.Report.Verdict, res.Report.Reasons)
	}
	if res.Report.ImageReview.FlaggedShare != 1 {
		t.Errorf("the report does not carry the share the review complained about: %+v", res.Report.ImageReview)
	}
	if !containsReason(res.Report.Reasons, "of the images the image review looked at were flagged") {
		t.Errorf("the reason does not name the image review: %v", res.Report.Reasons)
	}
	// The flagged images are named under the suspicious ones, with the reason of the review; an image V3b
	// already named keeps both reasons in one line.
	own := []SuspiciousImage{{Frame: 5, Reason: "the box is almost empty"}}
	suspicious := mergeSuspicious(own, rev.Suspicious)
	if len(suspicious) != 12 {
		t.Errorf("the flagged images are not in the report: %+v", suspicious)
	}
	merged, found := SuspiciousImage{}, false
	for _, s := range suspicious {
		if s.Frame == 5 {
			merged, found = s, true
		}
	}
	if !found || !strings.Contains(merged.Reason, "the box is almost empty") ||
		!strings.Contains(merged.Reason, "image_review:wrong_class") {
		t.Errorf("an image V3b named and the review flagged: %+v", merged)
	}
	// A share of a fifth is the bound: below it the review does not change the verdict.
	ok := plain
	ok.ImageReview = assess.ImageReview{Done: true, Model: "ds/flash", Checked: 20}
	ok.Flagged = 4
	if got := Verify(ok).Report.Verdict; got != assess.Usable {
		t.Errorf("a share of 0.20 turned the verdict to %q", got)
	}
	ok.Flagged = 5
	if got := Verify(ok).Report.Verdict; got != assess.Questionable {
		t.Errorf("a share above 0.20 left the verdict at %q", got)
	}
}

// Every model call of the review is an auxiliary call of the chat, with model, tokens and cost (V3d).
func TestReviewBilledAsAuxCall(t *testing.T) {
	rt := newReviewTest(t)
	auto := rt.addFramesIn(t, 0, 11)
	rev := rt.review(t, "ds/flash", "mnist-mini", reviewInput{Auto: auto})
	if !rev.ImageReview.Done {
		t.Fatalf("the review did not run: %+v", rev.ImageReview)
	}
	calls := rt.auxCalls()
	if len(calls) != 3 {
		t.Fatalf("12 images were billed as %d calls, expected 3", len(calls))
	}
	for _, call := range calls {
		if call.ChatID != "c1" || call.Purpose != reviewAuxPurpose || call.Model != "ds/flash" {
			t.Errorf("the call was not billed for this chat and this model: %+v", call)
		}
		if call.Status != http.StatusOK || call.Input != 1000 || call.Output != 120 {
			t.Errorf("the tokens of the call are not what the model answered: %+v", call)
		}
		// 1000 input tokens at 0.14 USD per million and 120 output at 0.28: about 0.00017 USD.
		if call.Cost < 0.0001 || call.Cost > 0.0002 {
			t.Errorf("the call was priced at %v USD, expected around 0.00017", call.Cost)
		}
	}
}

// The images of a dataset that does not match AGW_VISION_DATASET_PATTERN never go to a cloud model: no
// call and no picture, and the report says why (V3d, #111).
func TestReviewDatasetPattern(t *testing.T) {
	rt := newReviewTest(t)
	auto := rt.addFramesIn(t, 0, 11)
	rev := rt.review(t, "ds/flash", "schweine-2026", reviewInput{Auto: auto})
	if rev.ImageReview.Done || rev.ImageReview.Model != "" {
		t.Fatalf("a cloud model was named for a dataset it may not see: %+v", rev.ImageReview)
	}
	if rev.ImageReview.Reason != "images of this dataset may not go to the cloud model (AGW_VISION_DATASET_PATTERN)" {
		t.Errorf("reason: %q", rev.ImageReview.Reason)
	}
	if len(rt.modelCalls()) != 0 {
		t.Error("images of a dataset that does not match the pattern went to the model")
	}
	if len(rt.cvat.frameRequests()) != 0 {
		t.Error("the gateway fetched pictures it may not send anywhere")
	}
	// A name the pattern allows goes through.
	rev = rt.review(t, "ds/flash", "mnist-mini", reviewInput{Auto: auto})
	if !rev.ImageReview.Done || rev.ImageReview.Model != "ds/flash" {
		t.Fatalf("a public dataset was not reviewed: %+v", rev.ImageReview)
	}
	if len(rt.cvat.frameRequests()) != 12 {
		t.Errorf("the review read %d pictures, expected 12", len(rt.cvat.frameRequests()))
	}
}

// A model on the user's own hardware may look at every dataset, whatever its name (V3d, #111).
func TestReviewLocalProviderIgnoresPattern(t *testing.T) {
	rt := newReviewTest(t)
	auto := rt.addFramesIn(t, 0, 11)
	rev := rt.review(t, "ds/own", "schweine-2026", reviewInput{Auto: auto})
	if !rev.ImageReview.Done || rev.ImageReview.Model != "ds/own" {
		t.Fatalf("a self hosted model did not review the images: %+v", rev.ImageReview)
	}
	if rev.ImageReview.Checked != 12 {
		t.Errorf("the review checked %d images, expected 12", rev.ImageReview.Checked)
	}
	if len(rt.modelCalls()) != 3 {
		t.Errorf("12 images took %d calls, expected 3", len(rt.modelCalls()))
	}
}

// A model that reads text only is not asked at all: no call, no picture, and the stricter thresholds of
// V3b hold (V3d).
func TestNoVisionNoUpstreamCall(t *testing.T) {
	rt := newReviewTest(t)
	auto := rt.addFramesIn(t, 0, 11)
	rev := rt.review(t, "ds/text", "mnist-mini", reviewInput{Auto: auto})
	if rev.ImageReview.Done {
		t.Fatalf("a text model counted as an image review: %+v", rev.ImageReview)
	}
	if len(rt.modelCalls()) != 0 {
		t.Error("a model that reads text only was sent the pictures")
	}
	if len(rt.cvat.frameRequests()) != 0 {
		t.Error("the gateway fetched pictures for a model that cannot read them")
	}
	// A chat without a model, and a model the catalog does not know, are refused the same way.
	if rev := rt.review(t, "", "mnist-mini", reviewInput{Auto: auto}); rev.ImageReview.Done || rev.ImageReview.Reason == "" {
		t.Errorf("a chat without a model: %+v", rev.ImageReview)
	}
	if rev := rt.review(t, "ds/nope", "mnist-mini", reviewInput{Auto: auto}); rev.ImageReview.Done ||
		!strings.Contains(rev.ImageReview.Reason, "not in the catalog") {
		t.Errorf("a model outside the catalog: %+v", rev.ImageReview)
	}
}

// Without an image review the same figures judge by the stricter thresholds of V3b; with one they judge by
// the milder set (V3d).
func TestNoVisionStricterVerdict(t *testing.T) {
	// Twelve held-out images: too few for a verdict without a second view, enough with one.
	short := verdictTask(12, 12)
	if got := Verify(short).Report.Verdict; got != assess.NotMeasured {
		t.Fatalf("twelve images without a review are %q, expected not_measured", got)
	}
	reviewed := short
	reviewed.ImageReview = assess.ImageReview{Done: true, Model: "ds/flash", Checked: 12}
	if got := Verify(reviewed).Report.Verdict; got != assess.Usable {
		t.Errorf("twelve checked images with a review are %q, expected usable: %v", got, Verify(reviewed).Report.Reasons)
	}
	// A figure of 0.84 is below the 0.85 without a review and above the 0.80 with one.
	weak := verdictTask(25, 21)
	if got := Verify(weak).Report.Verdict; got != assess.Questionable {
		t.Fatalf("a figure of 0.84 without a review is %q, expected questionable", got)
	}
	weak.ImageReview = assess.ImageReview{Done: true, Model: "ds/flash", Checked: 25}
	if got := Verify(weak).Report.Verdict; got != assess.Usable {
		t.Errorf("a figure of 0.84 with a review is %q, expected usable: %v", got, Verify(weak).Report.Reasons)
	}
}

// The reason a report gives for not reviewing is the one the rule of #111 words (assess.ImageReviewAllowed,
// R0.9) — the same entry the capabilities overview reports under "vision" (V3d).
func TestNoVisionReasonFromCapabilities(t *testing.T) {
	rt := newReviewTest(t)
	_, textOnly, found := rt.svc.Cat.Lookup("ds/text")
	if !found {
		t.Fatal("the catalog of the test does not know the text model")
	}
	_, want := assess.ImageReviewAllowed(textOnly, "mnist-mini", rt.pattern)
	if want == "" {
		t.Fatal("the rule gave no reason for a model that reads text only")
	}
	rev := rt.review(t, "ds/text", "mnist-mini", reviewInput{Auto: rt.addFrames(t, 0, 1)})
	if rev.ImageReview.Reason != want {
		t.Errorf("reason %q, expected the wording of the rule: %q", rev.ImageReview.Reason, want)
	}
	if !strings.Contains(rev.ImageReview.Reason, "reads text only") {
		t.Errorf("the reason does not name the capability: %q", rev.ImageReview.Reason)
	}
	_, cloud, _ := rt.svc.Cat.Lookup("ds/flash")
	_, wantCloud := assess.ImageReviewAllowed(cloud, "schweine-2026", rt.pattern)
	if wantCloud == "" || rev.ImageReview.Reason == wantCloud {
		t.Errorf("the two reasons are not told apart: %q", wantCloud)
	}
}

// The milder thresholds of V3b rest on at least ten checked images: below that the report says why and
// judges by the stricter set (V3d).
func TestReviewDoneNeedsTenChecked(t *testing.T) {
	rt := newReviewTest(t)
	few := rt.addFramesIn(t, 0, 7)
	rev := rt.review(t, "ds/flash", "mnist-mini", reviewInput{Auto: few})
	if rev.ImageReview.Done || rev.ImageReview.Checked != 8 {
		t.Fatalf("eight checked images: %+v", rev.ImageReview)
	}
	if rev.ImageReview.Model != "ds/flash" {
		t.Errorf("the report does not name the model that looked: %+v", rev.ImageReview)
	}
	if !strings.Contains(rev.ImageReview.Reason, "from 10 images on") {
		t.Errorf("reason: %q", rev.ImageReview.Reason)
	}
	short := verdictTask(12, 12)
	short.ImageReview = rev.ImageReview
	if got := Verify(short).Report.Verdict; got != assess.NotMeasured {
		t.Errorf("eight checked images carried the milder thresholds: %q", got)
	}
	enough := rt.addFramesIn(t, 10, 21)
	rev = rt.review(t, "ds/flash", "mnist-mini", reviewInput{Auto: enough})
	if !rev.ImageReview.Done || rev.ImageReview.Checked != 12 || rev.ImageReview.Reason != "" {
		t.Fatalf("twelve checked images: %+v", rev.ImageReview)
	}
	short.ImageReview = rev.ImageReview
	if got := Verify(short).Report.Verdict; got != assess.Usable {
		t.Errorf("twelve checked images did not carry the milder thresholds: %q (%v)", got, Verify(short).Report.Reasons)
	}
}

// verdictTask builds a verified task of n held-out images of one class, of which the function answered
// right ones right; the automatic annotations look like the manual ones, so that the figures alone decide
// the verdict.
func verdictTask(n, right int) VerifyInput {
	in := VerifyInput{Kind: KindClassification, Final: true, Classes: []string{"0"},
		Mapping: map[string]string{"0": "0"}, Threshold: 0.5, SourceID: "run-1",
		CalibrationImages: 10, ValidationImages: 5, HoldoutImages: n}
	manual := map[int][]ManualObject{}
	auto := map[int][]AutoObject{}
	for frame := 0; frame < n; frame++ {
		manual[frame] = []ManualObject{{Class: "0"}}
		answer := Answer{Label: "0", Confidence: 0.9}
		if frame >= right {
			// A class the mapping does not name: the function answered nothing on this image.
			answer = Answer{Label: "not_mapped", Confidence: 0.9}
		}
		in.Measured = append(in.Measured, Observation{Frame: frame, Answers: []Answer{answer}, Manual: manual[frame]})
	}
	for frame := n; frame < n+20; frame++ {
		auto[frame] = []AutoObject{{Class: "0", Type: "tag"}}
	}
	in.Objects = TaskObjects{Frames: n + 20, Manual: manual, Auto: auto}
	return in
}

// containsReason reports whether one of the reasons carries the wording.
func containsReason(reasons []string, wording string) bool {
	for _, r := range reasons {
		if strings.Contains(r, wording) {
			return true
		}
	}
	return false
}
