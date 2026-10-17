// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // the frames of a task may be PNG as well as JPEG
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"agw/internal/imagereview"
)

// The rendering of the images the image review looks at (V3d). The review sends pictures of the user's
// dataset to the model of the chat, so they are shrunk to a size that keeps one picture at a few hundred
// input tokens, and the automatic annotation is drawn on them as frames, one colour per class. The
// standard library carries no scaler and no font, and neither is needed: the model needs to see the
// picture and where a box sits, not a caption.
//
// Nothing here calls a model. The rendered picture and the line that explains the drawing go to
// review.go, which asks the model and reads its answer.

const (
	// ReviewImageSide is the longest side a rendered image is sent at in pixels (V3d): small enough to
	// keep the cost of one picture down, large enough to judge a box on it. Every picture goes out at this
	// size, also a small one — MNIST carries 28 px images, and at their own size a model would see next to
	// nothing of them.
	ReviewImageSide = 512
	// maxFrameBytes bounds one picture CVAT answers.
	maxFrameBytes = 16 << 20
	// jpegQuality of a rendered image: enough for a model, and it keeps the base64 of a 512 px picture
	// at a few tens of kilobytes.
	jpegQuality = 80
	// boxLineWidth is the width of a drawn frame in pixels of the rendered image.
	boxLineWidth = 3
)

// reviewColours are the colours of the drawn frames, one per class of the dataset, in the order the
// legend of the prompt walks through them. From the ninth class on they repeat: a legend with more than
// eight colours says nothing to a model anyway.
var reviewColours = []struct {
	name string
	rgba color.RGBA
}{
	{"red", color.RGBA{R: 230, G: 25, B: 43, A: 255}},
	{"green", color.RGBA{R: 50, G: 200, B: 50, A: 255}},
	{"blue", color.RGBA{R: 40, G: 90, B: 230, A: 255}},
	{"yellow", color.RGBA{R: 240, G: 220, B: 40, A: 255}},
	{"magenta", color.RGBA{R: 230, G: 60, B: 200, A: 255}},
	{"cyan", color.RGBA{R: 60, G: 215, B: 215, A: 255}},
	{"orange", color.RGBA{R: 250, G: 150, B: 40, A: 255}},
	{"white", color.RGBA{R: 250, G: 250, B: 250, A: 255}},
}

// ColourOf names the colour of the class at this place in the class list of the dataset and answers it.
// The name is what the prompt says ("a frame in red marks an object of the class "3""), the value is what
// the drawing uses.
func ColourOf(index int) (string, color.RGBA) {
	c := reviewColours[((index%len(reviewColours))+len(reviewColours))%len(reviewColours)]
	return c.name, c.rgba
}

// Box is one box of the automatic annotation of an image, to be drawn on the picture (V3d). Points are
// the corners [x1,y1,x2,y2] in the coordinates of the original image.
type Box struct {
	// Colour is the name of the colour of the box's class, as ColourOf answers it.
	Colour string
	Points []float64
}

// classPlaces is the place of every class in the class list of the dataset; the place is the colour a box
// of that class is drawn in and named after. A class named twice keeps its first place.
func classPlaces(classes []string) map[string]int {
	out := make(map[string]int, len(classes))
	for i, c := range classes {
		if _, seen := out[c]; !seen {
			out[c] = i
		}
	}
	return out
}

// reviewBoxes are the boxes one image's automatic annotation is drawn as, each in the colour of its class.
// An object without points — a tag of a classification — is not drawn: its class stands in the line of the
// prompt instead.
func reviewBoxes(objs []AutoObject, places map[string]int) []Box {
	out := make([]Box, 0, len(objs))
	for _, ob := range objs {
		if len(ob.Points) < 4 {
			continue
		}
		colour, _ := ColourOf(places[ob.Class])
		out = append(out, Box{Colour: colour, Points: ob.Points})
	}
	return out
}

// reviewLine is the line the prompt carries for one image: which class every drawn colour stands for. An
// image nothing is annotated on says so, so that the model can name an object that is missing.
func reviewLine(frame int, objs []AutoObject, places map[string]int) string {
	head := "frame " + strconv.Itoa(frame) + ": "
	if len(objs) == 0 {
		return head + "nothing is annotated on this image"
	}
	seen := map[string]bool{}
	parts := make([]string, 0, len(objs))
	for _, ob := range objs {
		colour, _ := ColourOf(places[ob.Class])
		key := colour + "\x00" + ob.Class
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(ob.Points) >= 4 {
			parts = append(parts, fmt.Sprintf("a frame in %s marks an object of the class %q", colour, ob.Class))
		} else {
			parts = append(parts, fmt.Sprintf("the image is annotated with the class %q (colour %s)", ob.Class, colour))
		}
	}
	return head + strings.Join(parts, "; ")
}

// RenderFrame renders the picture at the given size and draws the boxes of the automatic annotation on it
// as frames in their colour (V3d). Without a box — a classification, whose class stands in the line of the
// prompt instead — nothing is drawn: a frame around everything would say nothing. The answer is a JPEG.
func RenderFrame(img image.Image, boxes []Box, maxSide int) ([]byte, error) {
	b := img.Bounds()
	w, h := imagereview.Scale(b.Dx(), b.Dy(), maxSide)
	if w <= 0 || h <= 0 {
		return nil, errors.New("the picture has no size")
	}
	dst := imagereview.Resize(img, w, h)
	// The boxes come in the coordinates of the original picture; the drawing goes by the ratio of the
	// rendered picture to it.
	fx, fy := float64(w)/float64(b.Dx()), float64(h)/float64(b.Dy())
	for _, box := range boxes {
		drawBox(dst, box, fx, fy)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawBox draws one box as a frame of boxLineWidth pixels along its edges (V3d). The corners come in the
// coordinates of the original picture and are scaled by fx and fy; what lies outside the rendered
// picture is cut off, and a box that has no finite corners is left out — a NaN from a function's answer
// must not end up as a stray line over the picture.
func drawBox(dst *image.RGBA, box Box, fx, fy float64) {
	if len(box.Points) < 4 {
		return
	}
	for _, p := range box.Points[:4] {
		if math.IsNaN(p) || math.IsInf(p, 0) {
			return
		}
	}
	w, h := dst.Bounds().Dx(), dst.Bounds().Dy()
	x1, x2 := box.Points[0]*fx, box.Points[2]*fx
	y1, y2 := box.Points[1]*fy, box.Points[3]*fy
	if x1 > x2 {
		x1, x2 = x2, x1
	}
	if y1 > y2 {
		y1, y2 = y2, y1
	}
	left, right := clampInt(int(x1), 0, w-1), clampInt(int(x2), 0, w-1)
	top, bottom := clampInt(int(y1), 0, h-1), clampInt(int(y2), 0, h-1)
	if right <= left || bottom <= top {
		return
	}
	line := image.NewUniform(rgbaOf(box.Colour))
	for i := 0; i < boxLineWidth; i++ {
		if y := top + i; y <= bottom {
			draw.Draw(dst, image.Rect(left, y, right+1, y+1), line, image.Point{}, draw.Src)
			draw.Draw(dst, image.Rect(left, bottom-i, right+1, bottom-i+1), line, image.Point{}, draw.Src)
		}
		if x := left + i; x <= right {
			draw.Draw(dst, image.Rect(x, top, x+1, bottom+1), line, image.Point{}, draw.Src)
			draw.Draw(dst, image.Rect(right-i, top, right-i+1, bottom+1), line, image.Point{}, draw.Src)
		}
	}
}

// rgbaOf is the value of a colour name; a name no colour carries is drawn in red, so that a box is never
// invisible by accident.
func rgbaOf(name string) color.RGBA {
	for _, c := range reviewColours {
		if c.name == name {
			return c.rgba
		}
	}
	return reviewColours[0].rgba
}

// clampInt bounds a value to a range.
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// jobOf answers the job of a task that carries a frame: CVAT splits a task into jobs of consecutive
// frames, and both a picture and a link into the UI go through the job.
func jobOf(jobs []Job, frame int) (Job, bool) {
	for _, j := range jobs {
		if j.ID > 0 && frame >= j.StartFrame && frame <= j.StopFrame {
			return j, true
		}
	}
	return Job{}, false
}

// framePicture reads one picture of a task or of a job (V3d): type=frame answers the picture itself,
// quality=compressed the JPEG CVAT already holds for the UI. The number is the frame's place in the
// task — a job serves the frames of its segment with the numbers of the task (checked against CVAT
// 2.4.2: the job endpoint hands the number to the frame provider of the task unchanged). Live to
// confirm (V3d, Haltepunkt).
func (c *CVAT) framePicture(ctx context.Context, s session, path string, frame int) ([]byte, error) {
	q := url.Values{"type": {"frame"}, "number": {strconv.Itoa(frame)}, "quality": {"compressed"}}
	return c.do(ctx, s, http.MethodGet, path, q, nil, maxFrameBytes)
}

// JobFrame reads one picture through the job that carries the frame (V3d).
func (c *CVAT) JobFrame(ctx context.Context, s session, jobID int64, frame int) ([]byte, error) {
	return c.framePicture(ctx, s, cvatAPI+"/jobs/"+strconv.FormatInt(jobID, 10)+"/data", frame)
}

// TaskFrame reads one picture through the task itself, for the case that no job of the list carries the
// frame (V3d).
func (c *CVAT) TaskFrame(ctx context.Context, s session, taskID int64, frame int) ([]byte, error) {
	return c.framePicture(ctx, s, cvatAPI+"/tasks/"+strconv.FormatInt(taskID, 10)+"/data", frame)
}

// frameError names a picture CVAT did not answer, with the frame it belongs to.
func frameError(frame int, err error) error {
	return fmt.Errorf("the picture of frame %d was not readable: %w", frame, err)
}
