// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"agw/internal/platform"
	"agw/internal/store"
)

// The split of a dataset before a training (R1.2): the gateway creates a training dataset from the
// annotated images of a source dataset and keeps the rest back, so that a later check can measure the
// model on images it did not see. The selection lives in this file as pure functions (the work of
// SelectTrain and of FilterCVATImages is testable without a platform); the handler below does the
// writing, and the gateway alone decides which images go where.

// The share of the images that goes into the training dataset (R1.1). The rule of SelectTrain reads the
// share in steps of ten percent (position mod 10), so only such a share is accepted: a share of 55 would
// silently mean 50.
const (
	DefaultTrainShare = 70
	MinTrainShare     = 50
	MaxTrainShare     = 90
	// ShareStep is the step the accepted shares move in (50, 60, 70, 80, 90).
	ShareStep = 10
	// splitModulus is the modulo of the per-class selection rule.
	splitModulus = 10
)

// MsgSplitNotClassification is the refusal of a dataset that is annotated with boxes or polygons: the
// check measures classification (one tag per image, P8), and a split of a detection dataset would put
// whole pictures into a training set whose template reads them differently.
const MsgSplitNotClassification = "split_dataset supports classification datasets (one tag per image); this dataset has boxes or polygons"

// MsgTrainShare is the refusal of a share that is not in the accepted steps.
const MsgTrainShare = "train_share must be 50, 60, 70, 80 or 90"

// ErrNotClassification is the error of a dataset that is annotated with boxes or polygons: the gateway
// measures classification models only (P8). The message is the one the agent sees.
var ErrNotClassification = errors.New(MsgSplitNotClassification)

// MsgNoAnnotations is the refusal of a dataset without an annotation file.
const MsgNoAnnotations = "the dataset archive carries no annotations/annotations.xml; a split needs the annotations"

// SelectTrain splits the annotated images of one dataset into the part that goes into the new training
// dataset and the images that are kept back for checks (R1.2). Class by class the images are sorted by
// their SHA-256, and every tenth position goes into the training set while the position is below the
// share; the rest is kept back. The result does not depend on the order the images arrive in and is
// sorted (classes alphabetically, within a class by SHA-256).
//
// Images without a tag or with several tags are not part of items: the caller counts them, and they go
// into neither set.
func SelectTrain(items []Item, share int) (train, kept []Item) {
	byClass := map[string][]Item{}
	for _, it := range items {
		byClass[it.Class] = append(byClass[it.Class], it)
	}
	classes := make([]string, 0, len(byClass))
	for c := range byClass {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	for _, c := range classes {
		list := byClass[c]
		sort.Slice(list, func(i, j int) bool { return list[i].SHA < list[j].SHA })
		for i, it := range list {
			if i%splitModulus < share/splitModulus {
				train = append(train, it)
			} else {
				kept = append(kept, it)
			}
		}
	}
	return train, kept
}

// cvatImage is one <image> of a CVAT annotation: its name, the labels of its <tag> elements and how many
// shapes (boxes, polygons, polylines, masks) it carries.
type cvatImage struct {
	Name   string
	Tags   []string
	Shapes int
}

// cvatAnnotation is what the gateway reads out of an annotations.xml.
type cvatAnnotation struct {
	Images []cvatImage
	Labels []string
}

// ParseCVAT reads the image names, their tags and their shapes out of a CVAT annotation (the contract of
// the training templates, datasets/cvat_dataset.py). An unreadable document is an error.
func ParseCVAT(data []byte) (cvatAnnotation, error) {
	var out cvatAnnotation
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return cvatAnnotation{}, fmt.Errorf("the annotation is not readable XML: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "image":
			im := cvatImage{Name: attrOf(se, "name")}
			if err := readImageBody(dec, &im); err != nil {
				return cvatAnnotation{}, err
			}
			out.Images = append(out.Images, im)
		case "label":
			var l struct {
				Name string `xml:"name"`
			}
			if err := dec.DecodeElement(&l, &se); err != nil {
				return cvatAnnotation{}, fmt.Errorf("the annotation is not readable XML: %w", err)
			}
			if l.Name != "" {
				out.Labels = append(out.Labels, l.Name)
			}
		}
	}
	return out, nil
}

// readImageBody reads one <image> element to its end and collects its tags and shapes. The decoder has
// just read the start element.
func readImageBody(dec *xml.Decoder, im *cvatImage) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("the annotation is not readable XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tag":
				if l := attrOf(t, "label"); l != "" {
					im.Tags = append(im.Tags, l)
				}
			case "box", "polygon", "polyline", "mask":
				im.Shapes++
			}
			depth++
		case xml.EndElement:
			if depth == 0 {
				return nil
			}
			depth--
		}
	}
}

func attrOf(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// FilterCVATImages writes the annotation again with only the images of keep (R1.3). The rest of the
// document — the <meta> block above all — is copied byte for byte, and the result is valid XML. keep
// names the images to keep (their file names).
func FilterCVATImages(data []byte, keep map[string]bool) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var out bytes.Buffer
	last := int64(0)
	for {
		before := dec.InputOffset()
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the annotation is not readable XML: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "image" {
			continue
		}
		kept := keep[path.Base(attrOf(se, "name"))] // CVAT exports may carry a path prefix ("bucket/17/a.png")
		if err := dec.Skip(); err != nil {
			return nil, fmt.Errorf("the annotation is not readable XML: %w", err)
		}
		if kept {
			continue
		}
		// Everything before this element (including the images kept so far) is written now; the element
		// itself is left out.
		out.Write(data[last:before])
		last = dec.InputOffset()
	}
	out.Write(data[last:])
	result := out.Bytes()
	if _, err := ParseCVAT(result); err != nil {
		return nil, fmt.Errorf("the filtered annotation is not valid XML: %w", err)
	}
	return result, nil
}

// DatasetFiles is what the gateway read out of a dataset archive: the images by file name and the
// annotation.
type DatasetFiles struct {
	Images     map[string][]byte
	Annotation []byte
}

// Sha256Hex is the SHA-256 of these bytes as a lower case hex string; the gateway identifies an image by
// its content, never by its name (P1).
func Sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SplitStats is what the split found in a source dataset: how many images carry exactly one tag, how many
// carry none or several, the number of shapes and the classes of the annotation.
type SplitStats struct {
	Tagged  int
	Skipped int
	Shapes  int
	Classes []string
}

// ReadDataset splits a dataset archive into its images (by file name) and its CVAT annotation. The
// annotation sits under datasets/<id>/annotations/annotations.xml (datasets/cvat_dataset.py:36); every
// other file outside the annotation directory is an image.
func ReadDataset(zr *zip.Reader) (DatasetFiles, error) {
	out := DatasetFiles{Images: map[string][]byte{}}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := f.Name
		if isAnnotationFile(name) {
			data, err := readZipFile(f)
			if err != nil {
				return DatasetFiles{}, err
			}
			out.Annotation = data
			continue
		}
		if strings.Contains(name, "/annotations/") {
			continue // further files of the annotation directory (a converted copy, a mask per image)
		}
		data, err := readZipFile(f)
		if err != nil {
			return DatasetFiles{}, err
		}
		out.Images[path.Base(name)] = data
	}
	if len(out.Annotation) == 0 {
		return DatasetFiles{}, errors.New(MsgNoAnnotations)
	}
	return out, nil
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// isAnnotationFile names the annotation of the dataset, whatever prefix the archive uses.
func isAnnotationFile(name string) bool {
	return name == "annotations/annotations.xml" || strings.HasSuffix(name, "/annotations/annotations.xml")
}

// SplitOf builds the items of a split out of a dataset archive: every image with exactly one tag is an
// item (SHA-256 and class), images without a tag or with several are counted in Skipped. A dataset with
// shapes (boxes, polygons) is refused (P8).
func SplitOf(files DatasetFiles) (items []Item, stats SplitStats, err error) {
	ann, err := ParseCVAT(files.Annotation)
	if err != nil {
		return nil, SplitStats{}, err
	}
	stats.Classes = ann.Labels
	have := map[string]bool{}
	for _, im := range ann.Images {
		have[path.Base(im.Name)] = true
		stats.Shapes += im.Shapes
		if im.Shapes > 0 || len(im.Tags) != 1 {
			if im.Shapes == 0 {
				stats.Skipped++
			}
			continue
		}
		data, ok := files.Images[path.Base(im.Name)]
		if !ok {
			return nil, SplitStats{}, fmt.Errorf("the archive has no image for the annotation entry %q", im.Name)
		}
		stats.Tagged++
		items = append(items, Item{SHA: Sha256Hex(data), Class: im.Tags[0]})
	}
	for name := range files.Images {
		if !have[name] {
			stats.Skipped++
		}
	}
	if stats.Shapes > 0 {
		return nil, stats, ErrNotClassification
	}
	return items, stats, nil
}

// --- the handler ---

// splitRequest is the body of the split call (the tool fills it; the agent cannot reach the path).
type splitRequest struct {
	DatasetID  int64  `json:"dataset_id"`
	Name       string `json:"name"`
	TrainShare int    `json:"train_share"`
}

// splitAnswer is what the split tool answers: the new dataset and the numbers of the real split.
type splitAnswer struct {
	TrainDatasetID int64    `json:"train_dataset_id"`
	Name           string   `json:"name"`
	TrainImages    int      `json:"train_images"`
	KeptImages     int      `json:"kept_images"`
	SkippedImages  int      `json:"skipped_images"`
	Classes        []string `json:"classes"`
	Note           string   `json:"note"`
}

// splitDataset is POST /_agw/assess/splits (R1.1–R1.3): it reads the source dataset, picks the images of
// the training part and creates the training dataset. The whole call is one approved write (the route
// carries Create Dataset plus Update Dataset of the source, so the approval names the download); the
// reads, the download and the upload go through the user's token.
func (s *Service) splitDataset(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	if s.Plat == nil || s.St == nil {
		return unavailableResult(), nil
	}
	var body splitRequest
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return errResult("the split call is unreadable: " + err.Error()), nil
	}
	if body.DatasetID <= 0 {
		return errResult("dataset_id missing"), nil
	}
	if body.TrainShare == 0 {
		body.TrainShare = DefaultTrainShare
	}
	if !validShare(body.TrainShare) {
		return errResult(MsgTrainShare), nil
	}
	source, err := s.datasetInfo(ctx, chatID, body.DatasetID)
	if err != nil {
		return resultOf(err), nil
	}
	if body.Name == "" {
		body.Name = source.Name + "-train"
	}
	files, err := s.datasetFiles(ctx, chatID, body.DatasetID)
	if err != nil {
		return resultOf(err), nil
	}
	items, stats, err := SplitOf(files)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if len(items) == 0 {
		return errResult("the dataset has no annotated images (one tag per image): nothing to split"), nil
	}
	train, kept := SelectTrain(items, body.TrainShare)
	if len(train) == 0 {
		return errResult("the training share leaves no image for the training dataset"), nil
	}
	description := fmt.Sprintf("Training part of dataset %s (%d of %d images); created by the agent gateway for held-out checks",
		source.Name, len(train), len(items))
	trainID, err := s.createTrainingDataset(ctx, chatID, createSpec{
		Name: body.Name, Description: description, Classes: source.Classes, Train: train, Files: files,
	})
	if err != nil {
		return resultOf(err), nil
	}
	if err := s.St.RecordSplit(ctx, store.Split{
		ChatID: chatID, SourceDatasetID: body.DatasetID, TrainDatasetID: trainID,
		TrainImages: len(train), KeptImages: len(kept),
	}); err != nil {
		return platform.Result{}, err
	}
	answer := splitAnswer{
		TrainDatasetID: trainID, Name: body.Name, TrainImages: len(train), KeptImages: len(kept),
		SkippedImages: stats.Skipped, Classes: source.Classes,
		Note: fmt.Sprintf("Training dataset %d is ready; %d images of dataset %d are kept back for checks, and the gateway decides which. Train on the training dataset and check the model afterwards with assess_model.",
			trainID, len(kept), body.DatasetID),
	}
	return jsonResult(answer)
}

// doer is the platform of this service as the Doer the tools and the reads of the gateway expect; every
// call carries the chat, so that token exchange and the log work as everywhere else.
func (s *Service) doer(chatID string) platform.Doer {
	return func(ctx context.Context, r platform.Request) (platform.Result, error) {
		return s.Plat.Do(ctx, chatID, r)
	}
}

// validShare reports whether a train share is in the accepted steps.
func validShare(share int) bool {
	return share >= MinTrainShare && share <= MaxTrainShare && share%ShareStep == 0
}

// datasetInfo is the part of a dataset the split and the summaries read.
type datasetInfo struct {
	Name    string   `json:"name"`
	Classes []string `json:"annotation_labels"`
}

// datasetInfo reads the metadata of a dataset (a reading call with the user's token).
func (s *Service) datasetInfo(ctx context.Context, chatID string, id int64) (datasetInfo, error) {
	var out datasetInfo
	req := platform.Request{Method: http.MethodGet, Path: "/datasets/" + strconv.FormatInt(id, 10)}
	res, stop, err := platform.ReadJSON(ctx, s.doer(chatID), req, &out)
	s.log(ctx, chatID, "GET "+req.Path, callResult(res, err))
	if stop {
		return datasetInfo{}, callError("GET "+req.Path, res, err)
	}
	return out, nil
}

// datasetFiles downloads a dataset and reads its archive (R1.4). The temporary file is removed whatever
// happens; the size is limited to MaxDownloadBytes.
func (s *Service) datasetFiles(ctx context.Context, chatID string, id int64) (DatasetFiles, error) {
	callPath := "/datasets/" + strconv.FormatInt(id, 10) + "/download"
	file, err := s.downloadToTemp(ctx, chatID, callPath)
	if err != nil {
		return DatasetFiles{}, err
	}
	defer os.Remove(file)
	zr, err := zip.OpenReader(file)
	if err != nil {
		return DatasetFiles{}, errors.New("the dataset archive is not a ZIP: " + err.Error())
	}
	defer zr.Close()
	files, err := ReadDataset(&zr.Reader)
	if err != nil {
		return DatasetFiles{}, err
	}
	s.log(ctx, chatID, "GET "+callPath, fmt.Sprintf("ok: %d images, %d bytes of annotation", len(files.Images), len(files.Annotation)))
	return files, nil
}

// downloadToTemp streams a platform call into a temporary file of the gateway (R1.4: the archive never
// stands in memory) and answers its path; the caller removes it.
func (s *Service) downloadToTemp(ctx context.Context, chatID, callPath string) (string, error) {
	body, _, err := s.Plat.Stream(ctx, chatID, platform.Request{Method: http.MethodGet, Path: callPath})
	if err != nil {
		s.log(ctx, chatID, "GET "+callPath, "error: "+err.Error())
		return "", err
	}
	defer body.Close()
	f, err := os.CreateTemp("", "agw-assess-*.zip")
	if err != nil {
		return "", err
	}
	name := f.Name()
	limit := s.maxDownloadBytes()
	n, err := io.Copy(f, io.LimitReader(body, limit+1))
	cerr := f.Close()
	s.log(ctx, chatID, "GET "+callPath, fmt.Sprintf("ok: %d bytes", n))
	if err != nil || cerr != nil {
		os.Remove(name)
		return "", errors.Join(err, cerr)
	}
	if n > limit {
		os.Remove(name)
		return "", fmt.Errorf("the archive is larger than %d MB (AGW_ASSESS_DOWNLOAD_MAX_MB)", limit>>20)
	}
	return name, nil
}

// maxDownloadBytes is the limit of one download (AGW_ASSESS_DOWNLOAD_MAX_MB).
func (s *Service) maxDownloadBytes() int64 {
	if s.MaxDownloadBytes > 0 {
		return s.MaxDownloadBytes
	}
	return DefaultDownloadMaxBytes
}

// DefaultDownloadMaxBytes limits a download when nothing is configured (512 MiB, R1.4).
const DefaultDownloadMaxBytes = 512 << 20

// createSpec is what the gateway needs to create a training dataset (R1.3).
type createSpec struct {
	Name        string
	Description string
	Classes     []string
	Train       []Item
	Files       DatasetFiles
}

// createTrainingDataset creates the training dataset on the platform (R1.3): the images of the training
// part in SHA order and, as the last file, the CVAT annotation with only those images. The gateway fills
// the upload itself, so the agent never holds the images (P11).
func (s *Service) createTrainingDataset(ctx context.Context, chatID string, spec createSpec) (int64, error) {
	form := map[string][]string{
		"name":                      {spec.Name},
		"description":               {spec.Description},
		"dataset_type":              {"AgriImageDataResource"},
		"metadata":                  {"{}"},
		"is_classification_dataset": {"false"},
		"includes_annotation_file":  {"true"},
	}
	if len(spec.Classes) > 0 {
		form["annotation_labels"] = spec.Classes
	}
	ann, err := ParseCVAT(spec.Files.Annotation)
	if err != nil {
		return 0, err
	}
	// The name of every image in the archive, by its SHA-256 (the split knows the images by content).
	nameOf := map[string]string{}
	for _, im := range ann.Images {
		base := path.Base(im.Name)
		if data, ok := spec.Files.Images[base]; ok {
			nameOf[Sha256Hex(data)] = base
		}
	}
	shas := make([]string, 0, len(spec.Train))
	keep := map[string]bool{}
	for _, it := range spec.Train {
		name, ok := nameOf[it.SHA]
		if !ok {
			return 0, errors.New("the split picked an image the archive does not hold")
		}
		if keep[name] {
			continue
		}
		keep[name] = true
		shas = append(shas, it.SHA)
	}
	sort.Strings(shas)
	var files []platform.File
	var uploads []platform.Upload
	for _, sha := range shas {
		name := nameOf[sha]
		files = append(files, platform.File{Field: "files", Path: "/agw/" + sha + path.Ext(name)})
		uploads = append(uploads, platform.Upload{Field: "files", Name: name, Data: spec.Files.Images[name], SHA256: sha})
	}
	filtered, err := FilterCVATImages(spec.Files.Annotation, keep)
	if err != nil {
		return 0, err
	}
	files = append(files, platform.File{Field: "files", Path: "/agw/annotations.xml"})
	uploads = append(uploads, platform.Upload{Field: "files", Name: "annotations.xml", Data: filtered, SHA256: Sha256Hex(filtered)})
	res, err := s.Plat.Do(ctx, chatID, platform.Request{Method: http.MethodPost, Path: "/datasets", Form: form, Files: files, Uploads: uploads})
	s.log(ctx, chatID, "POST /datasets ("+spec.Name+", "+strconv.Itoa(len(uploads))+" files)", callResult(res, err))
	if err != nil {
		return 0, err
	}
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return 0, fmt.Errorf("creating the training dataset failed: %s", messageOf(res, err))
	}
	var created struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal([]byte(res.Body), &created); err != nil {
		return 0, errors.New("the platform's answer to POST /datasets is not readable: " + err.Error())
	}
	id, err := created.ID.Int64()
	if err != nil || id <= 0 {
		return 0, errors.New("the platform's answer to POST /datasets names no dataset id")
	}
	return id, nil
}

// messageOf builds the text of a failed platform call.
func messageOf(res platform.Result, err error) string {
	if err != nil {
		return err.Error()
	}
	if res.Message != "" {
		return res.Message
	}
	return fmt.Sprintf("HTTP %d", res.HTTPStatus)
}

// callError turns a failed read into an error with the text the agent should see.
func callError(what string, res platform.Result, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if res.Message != "" {
		return fmt.Errorf("%s: %s", what, res.Message)
	}
	return fmt.Errorf("%s: the platform answered HTTP %d", what, res.HTTPStatus)
}

// errResult is an answer the agent sees as an error.
func errResult(msg string) platform.Result {
	return platform.Result{Status: "error", Message: msg}
}

// errStatus carries an answer that is already the answer (a refused or unavailable call).
type errStatus struct{ res platform.Result }

func (e errStatus) Error() string { return e.res.Message }

// resultOf turns an error into an answer; an error that already carries a result keeps it.
func resultOf(err error) platform.Result {
	var st errStatus
	if errors.As(err, &st) {
		return st.res
	}
	return errResult(err.Error())
}

// callResult names the outcome of one internal call for the log.
func callResult(res platform.Result, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	if res.Status != "ok" {
		return res.Status + ": " + res.Message
	}
	return fmt.Sprintf("HTTP %d", res.HTTPStatus)
}

// log records one call the check service made on its own (R1.3).
func (s *Service) log(ctx context.Context, chatID, detail, result string) {
	if s.LogCall != nil {
		s.LogCall(ctx, chatID, detail, result)
	}
}
