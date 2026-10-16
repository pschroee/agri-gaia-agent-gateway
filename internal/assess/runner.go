// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"agw/internal/sandbox"
)

// The runner runs a model in a container of its own, outside the agent's sandbox (P6, R0.11). The
// container has no network, no socket, no volume and a read-only root; the model and the inputs go in
// as one tar over stdin, the raw predictions come back as one JSON line over stdout. The script is part
// of the image, so the agent can read it but not change it; the gateway decides what goes in and reads
// the answer strictly.

const (
	// MaxClasses is the largest number of outputs a classification check accepts; more is a mistake
	// (segmentation masks, a detector's anchors) and is refused by the script (R0.13).
	MaxClasses = 1000
	// MaxOutputBytes is the largest answer of the check container (P6: 8 MiB). A longer one is an error
	// and not read to the end.
	MaxOutputBytes = 8 << 20
	// CheckMemoryMB, CheckCPUs and CheckPids are the limits of one check container (R0.11).
	CheckMemoryMB = 2048
	CheckCPUs     = 1
	CheckPids     = 64
	// CheckTmpfs is the only writable place in the check container: the script unpacks the tar there.
	CheckTmpfs = "rw,noexec,nosuid,size=1g"
	// CheckLabel marks a check container; `docker ps` shows it and RemoveManaged collects leftovers.
	CheckLabel = "agw.check"
	// ScaleToUnit is the default preprocessing of the training templates: byte values times 1/255.
	ScaleToUnit = 0.00392156862745098
	// TarRecord is the record size of a tar archive (tar(1)'s blocking factor 20 × 512 bytes, python's
	// tarfile.RECORDSIZE). BuildTar pads the archive to a whole number of records; see there.
	TarRecord = 20 * 512
)

// Runtime is what the runner needs from the container runtime: one command in a fresh, sealed
// container (sandbox.Runtime.RunOnce; a fake in tests).
type Runtime interface {
	RunOnce(ctx context.Context, spec sandbox.Spec, stdin io.Reader, maxOut int64) (stdout []byte, exit int, stderrTail string, err error)
}

// Image is one picture of a check: its id (the SHA-256) and the bytes with the file ending of the
// archive they came from.
type Image struct {
	ID   string
	Ext  string // ".png", ".jpg", … (the script opens the file, so the ending must fit)
	Data []byte
}

// Tensor is one named input tensor of a check that asks with test requests instead of images (R3).
type Tensor struct {
	Name  string
	Shape []int
	Data  []float32 // little endian in the tar (R0.12)
}

// Pre is the preprocessing the script applies to the images: data, not code (R0.12). Empty fields fall
// back to what the training templates do (RGB, 1/255, NCHW, no mean/std).
type Pre struct {
	Color  string    `json:"color,omitempty"`  // rgb | gray
	Size   []int     `json:"size,omitempty"`   // [h, w]; empty: from the model's input shape
	Scale  float64   `json:"scale,omitempty"`  // multiplier of the pixel values
	Mean   []float64 `json:"mean,omitempty"`   // subtracted after scaling
	Std    []float64 `json:"std,omitempty"`    // divided after subtracting
	Layout string    `json:"layout,omitempty"` // nchw | nhwc
}

// Job is what one check runs: the model, its inputs and the preprocessing.
type Job struct {
	Model   []byte
	Images  []Image
	Tensors []Tensor
	Pre     Pre
}

// ModelInfo is what the script read out of the ONNX file (R0.13).
type ModelInfo struct {
	Inputs    []TensorInfo `json:"inputs"`
	Outputs   []TensorInfo `json:"outputs"`
	LoadError string       `json:"load_error"`
}

// TensorInfo is one input or output of the model.
type TensorInfo struct {
	Name  string `json:"name"`
	Shape []int  `json:"shape"`
	Type  string `json:"type"`
}

// Prediction is the answer for one image: the class with the highest value and the probabilities, or
// the reason why this image could not be run.
type Prediction struct {
	ID    string    `json:"id"`
	OK    bool      `json:"ok"`
	Error string    `json:"error"`
	Top   int       `json:"top"`
	Probs []float64 `json:"probs"`
}

// TensorOut is one output tensor of a check that asked with tensors (R3).
type TensorOut struct {
	Name  string    `json:"name"`
	Shape []int     `json:"shape"`
	Data  []float64 `json:"data"`
}

// Output is the answer of the script: what the model looks like, the predictions of the images and the
// output tensors of a tensor check (R0.13). Go computes every figure from it (P5).
type Output struct {
	Model       ModelInfo     `json:"model"`
	Predictions []Prediction  `json:"predictions"`
	Tensors     [][]TensorOut `json:"tensors"`
	// Error: why the whole check could not run (no input of the model, an unreadable job). Single images
	// carry their own error in their prediction.
	Error string `json:"error,omitempty"`
}

// Runner runs checks in containers. Configured false: this setup has no runtime for checks and every
// check answers "not configured".
type Runner struct {
	rt    Runtime
	image string
}

// NewRunner builds the runner; image is AGW_IMAGE (the execution image with the check script), rt the
// container runtime. Either missing: the runner is not configured.
func NewRunner(rt Runtime, image string) *Runner {
	return &Runner{rt: rt, image: image}
}

// Configured reports whether checks can run here at all (G1.11: the capability "checks").
func (r *Runner) Configured() bool { return r != nil && r.rt != nil && r.image != "" }

// Run sends the job to a fresh check container and reads its answer (R0.10–R0.13). The container is
// always removed; the time limit comes from ctx.
func (r *Runner) Run(ctx context.Context, j Job) (Output, error) {
	var out Output
	if !r.Configured() {
		return out, errors.New("checks are not configured on this instance")
	}
	if len(j.Model) == 0 {
		return out, errors.New("the check has no model")
	}
	if len(j.Images) == 0 && len(j.Tensors) == 0 {
		return out, errors.New("the check has no inputs")
	}
	blob, err := BuildTar(j)
	if err != nil {
		return out, err
	}
	spec := sandbox.Spec{
		Name:        "agw-check-" + randomSuffix(),
		Image:       r.image,
		Entrypoint:  []string{"python3", "/opt/agw/check/predict.py"},
		InternalNet: "none",
		MemoryMB:    CheckMemoryMB,
		CPUs:        CheckCPUs,
		Pids:        CheckPids,
		Tmpfs:       map[string]string{"/tmp": CheckTmpfs},
		Labels:      map[string]string{CheckLabel: "1"},
	}
	raw, exit, stderr, err := r.rt.RunOnce(ctx, spec, bytes.NewReader(blob), MaxOutputBytes)
	if err != nil {
		return out, err
	}
	if exit != 0 {
		return out, fmt.Errorf("the check container exited with %d: %s", exit, trimSpace(stderr))
	}
	// Strict: an answer with a field the gateway does not know is not trusted (R0.13).
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return Output{}, fmt.Errorf("the check container answered in a form the gateway does not know: %w", err)
	}
	if dec.More() {
		return Output{}, errors.New("the check container answered more than one JSON value")
	}
	if out.Model.LoadError != "" {
		return Output{}, fmt.Errorf("the model could not be loaded: %s", out.Model.LoadError)
	}
	if out.Error != "" {
		return Output{}, errors.New("the check could not run: " + out.Error)
	}
	for _, p := range out.Predictions {
		if len(p.Probs) > MaxClasses {
			return Output{}, fmt.Errorf("too many outputs for a classification check (%d, at most %d)", len(p.Probs), MaxClasses)
		}
	}
	return out, nil
}

// Finite reports whether every probability of a prediction is a finite number. The script reports a
// non-finite output as ok:false; this is the same check on the Go side, so that a figure never rests on
// NaN or infinity.
func Finite(p Prediction) bool {
	for _, v := range p.Probs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// jobDoc is the job.json of the tar (R0.12).
type jobDoc struct {
	Mode    string      `json:"mode"` // images | tensors
	Images  []jobImage  `json:"images,omitempty"`
	Tensors []jobTensor `json:"tensors,omitempty"`
	Pre     jobPre      `json:"pre"`
	// MaxClasses bounds the outputs of a classification check; the script answers an error beyond it.
	MaxClasses int `json:"max_classes"`
}

type jobImage struct {
	ID   string `json:"id"`
	File string `json:"file"`
}

type jobTensor struct {
	Name  string `json:"name"`
	Shape []int  `json:"shape"`
	File  string `json:"file"`
}

type jobPre struct {
	Color  string    `json:"color"`
	Size   []int     `json:"size"`
	Scale  float64   `json:"scale"`
	Mean   []float64 `json:"mean"`
	Std    []float64 `json:"std"`
	Layout string    `json:"layout"`
}

// imageFile names an image inside the tar: images/<sha>.<ext>. The id is the SHA-256 of the bytes, so
// the name carries no dataset or file name of the user (P11).
func imageFile(im Image) string {
	ext := im.Ext
	if ext == "" {
		ext = ".png"
	}
	return "images/" + im.ID + ext
}

// BuildTar builds the tar that goes into the check container: job.json, model.onnx and the inputs
// (R0.12).
func BuildTar(j Job) ([]byte, error) {
	doc := jobDoc{MaxClasses: MaxClasses, Pre: jobPre{
		Color: j.Pre.Color, Size: j.Pre.Size, Scale: j.Pre.Scale, Mean: j.Pre.Mean, Std: j.Pre.Std, Layout: j.Pre.Layout,
	}}
	if doc.Pre.Color == "" {
		doc.Pre.Color = "rgb"
	}
	if doc.Pre.Scale == 0 {
		doc.Pre.Scale = ScaleToUnit
	}
	if doc.Pre.Layout == "" {
		doc.Pre.Layout = "nchw"
	}
	if len(j.Images) > 0 {
		doc.Mode = "images"
		for _, im := range j.Images {
			doc.Images = append(doc.Images, jobImage{ID: im.ID, File: imageFile(im)})
		}
	} else {
		doc.Mode = "tensors"
		for i, t := range j.Tensors {
			doc.Tensors = append(doc.Tensors, jobTensor{Name: t.Name, Shape: t.Shape, File: fmt.Sprintf("tensors/%d.f32", i)})
		}
	}
	jobJSON, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, data []byte) error {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add("job.json", append(jobJSON, '\n')); err != nil {
		return nil, err
	}
	if err := add("model.onnx", j.Model); err != nil {
		return nil, err
	}
	for _, im := range j.Images {
		if err := add(imageFile(im), im.Data); err != nil {
			return nil, err
		}
	}
	for i, t := range j.Tensors {
		if err := add(fmt.Sprintf("tensors/%d.f32", i), float32Bytes(t.Data)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	// Pad to a whole record, as tar(1) does: Go's tar.Writer ends the archive with its end-of-archive
	// marker but stops there, so a small archive is only a few blocks long. A reader that works in
	// records — python's tarfile reads TarRecord bytes at a time — would then wait for the rest of its last
	// record, and with the write side of the connection left open (see sandbox.RunOnce) that wait never
	// ends: the check hung after its inputs had arrived (found in the live test of R0, together with the
	// lost output). With the padding every record is complete and the reader stops at the marker.
	if rest := buf.Len() % TarRecord; rest != 0 {
		buf.Write(make([]byte, TarRecord-rest))
	}
	return buf.Bytes(), nil
}

// float32Bytes writes floats little endian, as the script reads them (R0.12).
func float32Bytes(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(x))
	}
	return out
}

// randomSuffix names a check container (8 hex digits); the gateway sets the name, never the agent.
func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

// trimSpace cuts the stderr tail into one line for the error message.
func trimSpace(s string) string { return strings.Join(strings.Fields(s), " ") }
