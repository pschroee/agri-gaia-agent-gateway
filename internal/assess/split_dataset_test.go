// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"testing"

	"agw/internal/platform"
)

// The split is fixed and does not depend on the order the images arrive in: per class every tenth
// position below the share goes into the training dataset, the rest is kept back (R1.2).
func TestSelectTrainDeterministic(t *testing.T) {
	var items []Item
	for _, cls := range []string{"cow", "pig"} {
		for _, sha := range []string{"aa", "bb", "cc", "dd", "ee", "ff", "gg", "hh", "ii", "jj"} {
			items = append(items, Item{SHA: sha, Class: cls})
		}
	}
	train, kept := SelectTrain(items, 70)
	if len(train) != 14 || len(kept) != 6 {
		t.Fatalf("70 %% of ten per class: %d train, %d kept", len(train), len(kept))
	}
	// Two calls with the same images in another order give the same split: every class keeps its share,
	// because the rule sorts by class and name before it counts.
	shuffled := make([]Item, 0, len(items))
	for _, i := range []int{9, 19, 0, 11, 3, 14, 7, 2, 17, 5, 1, 12, 4, 16, 8, 10, 6, 15, 13, 18} {
		shuffled = append(shuffled, items[i])
	}
	t2, k2 := SelectTrain(shuffled, 70)
	if len(t2) != 14 || len(k2) != 6 {
		t.Fatalf("shuffled: %d train, %d kept", len(t2), len(k2))
	}
	// The first seven SHAs of each class are in the training set, the last three are kept back.
	for _, cls := range []string{"cow", "pig"} {
		for _, sha := range []string{"aa", "bb", "cc", "dd", "ee", "ff", "gg"} {
			if !containsItem(train, Item{SHA: sha, Class: cls}) {
				t.Errorf("%s-%s is not in the training set", cls, sha)
			}
		}
		for _, sha := range []string{"hh", "ii", "jj"} {
			if !containsItem(kept, Item{SHA: sha, Class: cls}) {
				t.Errorf("%s-%s is not kept back", cls, sha)
			}
		}
	}
	// Another share moves the border.
	train, kept = SelectTrain(items, 50)
	if len(train) != 10 || len(kept) != 10 {
		t.Fatalf("50 %%: %d train, %d kept", len(train), len(kept))
	}
	// The result is sorted: classes alphabetically, within a class by SHA.
	for i := 1; i < len(train); i++ {
		if train[i-1].Class > train[i].Class ||
			(train[i-1].Class == train[i].Class && train[i-1].SHA > train[i].SHA) {
			t.Fatalf("the training set is not sorted: %+v", train)
		}
	}
}

func containsItem(list []Item, want Item) bool {
	for _, it := range list {
		if it == want {
			return true
		}
	}
	return false
}

// A dataset with boxes or polygons is no classification dataset: the split refuses it (R1.2, P8).
func TestSelectTrainRejectsBoxes(t *testing.T) {
	images := []testImage{
		{Name: "a.png", Data: "a", Tags: []string{"cow"}},
		{Name: "b.png", Data: "b", Tags: []string{"cow"}, Shape: "box"},
	}
	items, stats, err := SplitOf(DatasetFiles{
		Images:     map[string][]byte{"a.png": []byte("a"), "b.png": []byte("b")},
		Annotation: testAnnotation(images),
	})
	if err == nil || err.Error() != MsgSplitNotClassification {
		t.Fatalf("boxes: %v", err)
	}
	if !strings.Contains(err.Error(), "boxes or polygons") {
		t.Errorf("the refusal does not name boxes and polygons: %v", err)
	}
	if stats.Shapes != 1 || len(items) != 0 {
		t.Errorf("a dataset with shapes gave items: %+v", stats)
	}
	// Images without a tag or with several are counted, not split.
	images = []testImage{
		{Name: "a.png", Data: "a", Tags: []string{"cow"}},
		{Name: "b.png", Data: "b"},
		{Name: "c.png", Data: "c", Tags: []string{"cow", "pig"}},
	}
	items, stats, err = SplitOf(DatasetFiles{
		Images:     map[string][]byte{"a.png": []byte("a"), "b.png": []byte("b"), "c.png": []byte("c")},
		Annotation: testAnnotation(images),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || stats.Tagged != 1 || stats.Skipped != 2 {
		t.Fatalf("counting: %d items, %+v", len(items), stats)
	}
}

// The filtered annotation keeps exactly the images the gateway picked, and everything else of the
// document — the <meta> block above all — stands as it was (R1.3).
func TestFilterCVATImages(t *testing.T) {
	images := []testImage{
		{Name: "a.png", Data: "a", Tags: []string{"cow"}},
		{Name: "b.png", Data: "b", Tags: []string{"pig"}},
		{Name: "c.png", Data: "c", Tags: []string{"cow"}},
	}
	original := testAnnotation(images)
	filtered, err := FilterCVATImages(original, map[string]bool{"a.png": true, "c.png": true})
	if err != nil {
		t.Fatal(err)
	}
	ann, err := ParseCVAT(filtered)
	if err != nil {
		t.Fatalf("the filtered annotation is not readable: %v", err)
	}
	names := map[string]bool{}
	for _, im := range ann.Images {
		names[im.Name] = true
	}
	if len(ann.Images) != 2 || names["b.png"] || !names["a.png"] || !names["c.png"] {
		t.Fatalf("filtered images: %+v", ann.Images)
	}
	// The meta block and the version stand unchanged.
	head := func(b []byte) string { return string(b[:strings.Index(string(b), "</meta>")+len("</meta>")]) }
	if head(original) != head(filtered) {
		t.Errorf("the meta block changed:\n%s\n%s", head(original), head(filtered))
	}
	if !strings.Contains(string(filtered), "<labels>") {
		t.Error("the labels of the annotation are lost")
	}
	// The tags of a kept image stay with it.
	if len(ann.Images[0].Tags) != 1 || ann.Images[0].Tags[0] != "cow" {
		t.Errorf("the tags of a kept image are lost: %+v", ann.Images[0])
	}
}

// split_dataset reads the source dataset, creates exactly one training dataset with the picked images
// and the filtered annotation, and records the split (R1.1–R1.5).
func TestSplitDatasetUploads(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "split")
	f := newFakePlatform(t)
	images := []testImage{}
	for class, label := range []string{"0", "1"} {
		for i := 0; i < 10; i++ {
			images = append(images, testImage{
				Name: fmt.Sprintf("img%d_%02d.png", class, i),
				Data: fmt.Sprintf("content-%d-%d", class, i),
				Tags: []string{label},
			})
		}
	}
	f.dataset(7, "mnist-check-120", images)

	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	svc.Plat = f.client()
	res, err := svc.Do(ctx, chat.ID, platform.Request{Method: http.MethodPost, Path: platform.AssessPrefix + "/splits",
		Body: json.RawMessage(`{"dataset_id":7}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || res.HTTPStatus != http.StatusOK {
		t.Fatalf("split: %+v", res)
	}
	var answer splitAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.TrainDatasetID != 42 || answer.TrainImages != 14 || answer.KeptImages != 6 || answer.SkippedImages != 0 {
		t.Fatalf("answer: %+v", answer)
	}
	if answer.CheckImages != (CheckImages{Validation: 2, Holdout: 4, MinValidation: 5, MinHoldout: 20, Measurable: false}) {
		t.Errorf("check_images: %+v", answer.CheckImages)
	}
	if answer.CheckWarning == "" {
		t.Errorf("a check that is not measurable carries no check_warning")
	}
	if answer.Name != "mnist-check-120-train" {
		t.Errorf("default name: %q", answer.Name)
	}
	creations := f.creations()
	if len(creations) != 1 {
		t.Fatalf("POST /datasets went out %d times", len(creations))
	}
	cd := creations[0]
	if firstOf(cd.form["name"]) != "mnist-check-120-train" ||
		firstOf(cd.form["is_classification_dataset"]) != "false" ||
		firstOf(cd.form["includes_annotation_file"]) != "true" ||
		firstOf(cd.form["dataset_type"]) != "AgriImageDataResource" {
		t.Errorf("form: %+v", cd.form)
	}
	// The images of the training part and, last, the filtered annotation.
	if len(cd.names) != 15 || cd.names[len(cd.names)-1] != "annotations.xml" {
		t.Fatalf("uploaded files: %v", cd.names)
	}
	ann, err := ParseCVAT(cd.data["annotations.xml"])
	if err != nil {
		t.Fatal(err)
	}
	if len(ann.Images) != 14 {
		t.Fatalf("the annotation carries %d images, want 14", len(ann.Images))
	}
	for _, im := range ann.Images {
		if im.Name == "annotations.xml" {
			t.Errorf("the annotation names itself as an image: %v", cd.names)
		}
		if _, ok := cd.data[im.Name]; !ok {
			t.Errorf("the annotation names %s, but the upload does not carry it", im.Name)
		}
	}
	// The split is recorded: the training dataset knows its source (R1.5).
	sp, err := st.SplitOfTrainDataset(ctx, chat.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if sp.SourceDatasetID != 7 || sp.TrainImages != 14 || sp.KeptImages != 6 {
		t.Fatalf("recorded split: %+v", sp)
	}
	if sp, err := st.SplitOfSourceDataset(ctx, chat.ID, 7); err != nil || sp.TrainDatasetID != 42 {
		t.Fatalf("split of the source: %+v %v", sp, err)
	}
}

// A share outside the accepted steps and a call without a dataset are refused before anything goes out.
func TestSplitRefusesBadShare(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "share")
	f := newFakePlatform(t)
	f.dataset(7, "ds", []testImage{{Name: "a.png", Data: "a", Tags: []string{"0"}}})
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	svc.Plat = f.client()
	for _, body := range []string{`{"dataset_id":7,"train_share":55}`, `{"dataset_id":7,"train_share":95}`, `{"train_share":70}`} {
		res, err := svc.Do(ctx, chat.ID, platform.Request{Method: http.MethodPost, Path: platform.AssessPrefix + "/splits",
			Body: json.RawMessage(body)})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "error" {
			t.Errorf("%s: %+v", body, res)
		}
	}
	if len(f.creations()) != 0 {
		t.Error("a refused call created a dataset")
	}
}

// A CVAT export of a task made from the object store names its images with a path prefix
// ("agent-test/30/a.png"); the filter must still match the file names the split picked (live check of #168).
func TestFilterCVATImagesWithPathPrefix(t *testing.T) {
	images := []testImage{
		{Name: "agent-test/30/a.png", Data: "a", Tags: []string{"cow"}},
		{Name: "agent-test/30/b.png", Data: "b", Tags: []string{"pig"}},
	}
	filtered, err := FilterCVATImages(testAnnotation(images), map[string]bool{"a.png": true})
	if err != nil {
		t.Fatal(err)
	}
	ann, err := ParseCVAT(filtered)
	if err != nil {
		t.Fatal(err)
	}
	if len(ann.Images) != 1 || ann.Images[0].Name != "agent-test/30/a.png" {
		t.Fatalf("filtered images: %+v", ann.Images)
	}
}

// shaItems builds n items per class with distinct SHAs.
func shaItems(classes []string, n int) []Item {
	var out []Item
	for _, c := range classes {
		for i := 0; i < n; i++ {
			out = append(out, Item{SHA: fmt.Sprintf("%s-%04d", c, i), Class: c})
		}
	}
	return out
}

// The demo run: mnist-mini has 2 x 15 images, and at 70 % the check finds 2 validation and 4 held-out
// images (#220).
func TestCheckSetsOfMnistMini(t *testing.T) {
	items := shaItems([]string{"0", "1"}, 15)
	train, kept := SelectTrain(items, 70)
	if got := CheckSetsOf(train, kept); got != (CheckImages{Validation: 2, Holdout: 4, MinValidation: 5, MinHoldout: 20, Measurable: false}) {
		t.Errorf("share 70: %+v", got)
	}
	train, kept = SelectTrain(items, 50)
	got := CheckSetsOf(train, kept)
	if got.Validation != 4 || got.Holdout != 6 || got.Measurable {
		t.Errorf("share 50: %+v", got)
	}
}

func TestCheckSetsOfExcludesSeenSHA(t *testing.T) {
	train := []Item{{SHA: "a", Class: "0"}, {SHA: "b", Class: "0"}}
	kept := []Item{{SHA: "a", Class: "0"}, {SHA: "c", Class: "0"}, {SHA: "d", Class: "0"}}
	got := CheckSetsOf(train, kept)
	if got.Validation+got.Holdout != 2 {
		t.Errorf("a kept-back image seen in training counts: %+v", got)
	}
}

// The prediction and the check use the same rule: CheckSetsOf equals PartitionImages on the unseen images.
func TestCheckSetsMatchCheck(t *testing.T) {
	rng := rand.New(rand.NewSource(220))
	for round := 0; round < 40; round++ {
		nClasses := 1 + rng.Intn(5)
		var items []Item
		for c := 0; c < nClasses; c++ {
			n := rng.Intn(80)
			for i := 0; i < n; i++ {
				items = append(items, Item{SHA: fmt.Sprintf("%d-%d-%d", round, c, rng.Int()), Class: fmt.Sprint(c)})
			}
		}
		share := 50 + 10*rng.Intn(5)
		train, kept := SelectTrain(items, share)
		seen := map[string]bool{}
		for _, it := range train {
			seen[it.SHA] = true
		}
		var unseen []checkImage
		for _, it := range kept {
			if !seen[it.SHA] {
				unseen = append(unseen, checkImage{Item: it})
			}
		}
		v, h := PartitionImages(unseen)
		got := CheckSetsOf(train, kept)
		if got.Validation != len(v) || got.Holdout != len(h) {
			t.Fatalf("round %d share %d: predicted %d/%d, check %d/%d", round, share, got.Validation, got.Holdout, len(v), len(h))
		}
	}
}

func TestSuggestShare(t *testing.T) {
	share, ci, ok := SuggestShare(shaItems([]string{"0", "1"}, 50), 90)
	if !ok || share != 70 || !ci.Measurable {
		t.Errorf("2 x 50 from 90: %d %+v %v", share, ci, ok)
	}
	if _, _, ok := SuggestShare(shaItems([]string{"0", "1"}, 15), 70); ok {
		t.Errorf("2 x 15 has no share that reaches the minimums")
	}
}

func TestImagesNeeded(t *testing.T) {
	for _, c := range []struct{ classes, share, want int }{{2, 50, 60}, {2, 70, 100}, {10, 70, 100}} {
		if got := ImagesNeeded(c.classes, c.share); got != c.want {
			t.Errorf("ImagesNeeded(%d, %d) = %d, want %d", c.classes, c.share, got, c.want)
		}
	}
	if ImagesNeeded(0, 50) != ImagesNeeded(1, 50) {
		t.Errorf("no class counts as one")
	}
}

// runSplit runs the split handler on a dataset of n images per class and answers the decoded answer.
func runSplit(t *testing.T, n int, body string) (splitAnswer, *fakePlatform) {
	t.Helper()
	st := testStore(t)
	chat := testChat(t, st, "split")
	f := newFakePlatform(t)
	var images []testImage
	for class, label := range []string{"0", "1"} {
		for i := 0; i < n; i++ {
			images = append(images, testImage{
				Name: fmt.Sprintf("img%d_%03d.png", class, i),
				Data: fmt.Sprintf("content-%d-%d", class, i),
				Tags: []string{label},
			})
		}
	}
	f.dataset(7, "small", images)
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	svc.Plat = f.client()
	res, err := svc.Do(context.Background(), chat.ID, platform.Request{Method: http.MethodPost, Path: platform.AssessPrefix + "/splits",
		Body: json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("split: %+v", res)
	}
	var answer splitAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	return answer, f
}

func TestSplitDatasetWarnsSmallCheck(t *testing.T) {
	answer, f := runSplit(t, 15, `{"dataset_id":7,"train_share":70}`)
	if answer.CheckImages.Measurable {
		t.Fatalf("check_images: %+v", answer.CheckImages)
	}
	if !strings.HasPrefix(answer.CheckWarning, "The later check will not be measurable: of the 6 kept-back images it will use 2 as validation images (at least 5) and 4 as held-out images (at least 20).") ||
		!strings.Contains(answer.CheckWarning, "about 60 annotated images") {
		t.Errorf("check_warning: %q", answer.CheckWarning)
	}
	if answer.SuggestedTrainShare != 0 {
		t.Errorf("suggested_train_share: %d", answer.SuggestedTrainShare)
	}
	if !strings.HasSuffix(answer.Note, "Read check_warning before the training.") {
		t.Errorf("note: %q", answer.Note)
	}
	if n := len(f.creations()); n != 1 {
		t.Errorf("the dataset was created %d times", n)
	}
}

func TestSplitDatasetSuggestsShare(t *testing.T) {
	answer, _ := runSplit(t, 50, `{"dataset_id":7,"train_share":90}`)
	if answer.CheckImages.Measurable || answer.SuggestedTrainShare != 70 {
		t.Fatalf("answer: %+v", answer)
	}
	if !strings.Contains(answer.CheckWarning, "With train_share 70 it would get") ||
		!strings.Contains(answer.CheckWarning, "ask whether to split again with train_share 70") {
		t.Errorf("check_warning: %q", answer.CheckWarning)
	}
}
