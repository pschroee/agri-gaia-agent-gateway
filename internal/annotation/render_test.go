// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"testing"
)

// flatPicture is a picture of one colour; the tests draw on it and read the pixels back.
func flatPicture(w, h int, c color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	return img
}

// white is what the tests expect where nothing was drawn: the JPEG round trip keeps a flat colour within a
// few steps, so the checks compare with a tolerance.
var white = color.RGBA{R: 250, G: 250, B: 250, A: 255}

// jpegTolerance is how far a pixel may be off the expected colour after the JPEG round trip. It is wide
// enough for the ringing along a drawn line and still far below the distance between the colours the
// review draws with (red is 230,25,43 against white).
const jpegTolerance = 100

// decodeRendered reads a rendered picture back.
func decodeRendered(t *testing.T, jpegBytes []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		t.Fatalf("the rendered picture is not readable: %v", err)
	}
	return img
}

// near reports whether the pixel at x,y is within tol of the wanted colour on every channel.
func near(t *testing.T, img image.Image, x, y int, want color.RGBA, tol int) bool {
	t.Helper()
	if !image.Pt(x, y).In(img.Bounds()) {
		t.Fatalf("pixel %d,%d is outside the picture %v", x, y, img.Bounds())
	}
	r, g, b, _ := img.At(x, y).RGBA()
	got := [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}
	exp := [3]int{int(want.R), int(want.G), int(want.B)}
	for i := range got {
		if d := got[i] - exp[i]; d > tol || d < -tol {
			return false
		}
	}
	return true
}

// Scale renders a picture at the given longest side, shrinking a big one and blowing up a small one (V3d:
// an MNIST picture of 28 px says nothing to a model at its own size).
func TestScale(t *testing.T) {
	for _, c := range []struct{ w, h, max, wantW, wantH int }{
		{1024, 1024, 512, 512, 512},
		{1000, 500, 512, 512, 256},
		{500, 1000, 512, 256, 512},
		{28, 28, 512, 512, 512},   // MNIST: blown up, the picture is what a model can read
		{100, 50, 512, 512, 256},  // a picture that fits is blown up as well
		{512, 512, 512, 512, 512}, // exactly at the target
		{1000, 500, 0, 1000, 500}, // no target: nothing is scaled
		{0, 0, 512, 0, 0},
	} {
		gotW, gotH := Scale(c.w, c.h, c.max)
		if gotW != c.wantW || gotH != c.wantH {
			t.Errorf("Scale(%d, %d, %d) = %d, %d, want %d, %d", c.w, c.h, c.max, gotW, gotH, c.wantW, c.wantH)
		}
	}
}

// The boxes are drawn where the annotation sits in the original picture, also after the picture was
// shrunk (V3d): a picture of 1024 px with a box at [100,100,300,500] draws at [50,50,150,250] of the
// 512 px picture the model sees.
func TestRenderBoxes(t *testing.T) {
	picture := flatPicture(1024, 1024, white)
	colour, rgba := ColourOf(0)
	if colour != "red" {
		t.Fatalf("the first class is drawn in %q, expected red", colour)
	}
	raw, err := RenderFrame(picture, []Box{{Colour: colour, Points: []float64{100, 100, 300, 500}}}, ReviewImageSide)
	if err != nil {
		t.Fatal(err)
	}
	img := decodeRendered(t, raw)
	if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 512 {
		t.Fatalf("the rendered picture is %v, expected 512x512", b)
	}
	// The frame sits on the edges of the box: three pixels wide, the corner and its inside are drawn. The very
	// corner pixel (150,250) is not among them: it shares its chroma block in the JPEG with three white pixels
	// and comes back grey (measured 121,109,95 against red 230,25,43). The pixels beside it on both edges do
	// carry the colour, so the frame reaches the corner.
	for _, p := range [][2]int{{50, 50}, {52, 52}, {50, 150}, {150, 249}, {149, 250}, {100, 250}} {
		if !near(t, img, p[0], p[1], rgba, jpegTolerance) {
			t.Errorf("the frame is missing at %d,%d", p[0], p[1])
		}
	}
	// Outside the frame and inside the box the picture stays as it was.
	for _, p := range [][2]int{{49, 49}, {54, 54}, {100, 150}, {160, 260}} {
		if !near(t, img, p[0], p[1], white, jpegTolerance) {
			t.Errorf("something was drawn at %d,%d, but the box is not there", p[0], p[1])
		}
	}
	// A second class is drawn in its own colour.
	other, otherRGBA := ColourOf(1)
	second, err := RenderFrame(picture, []Box{{Colour: other, Points: []float64{600, 600, 900, 900}}}, ReviewImageSide)
	if err != nil {
		t.Fatal(err)
	}
	if !near(t, decodeRendered(t, second), 300, 300, otherRGBA, jpegTolerance) {
		t.Errorf("the box of the second class is not drawn in %s", other)
	}
	// A box whose corners are not finite is left out: a NaN out of a function's answer must not become a
	// stray line over the picture.
	broken, err := RenderFrame(picture, []Box{{Colour: colour, Points: []float64{math.NaN(), 10, 300, 500}}}, ReviewImageSide)
	if err != nil {
		t.Fatal(err)
	}
	if !near(t, decodeRendered(t, broken), 50, 50, white, jpegTolerance) {
		t.Error("a box with a NaN corner was drawn")
	}
}

// A classification carries its class in the line of the prompt instead of on the picture: nothing is drawn
// (V3d).
func TestRenderClassificationNoDrawing(t *testing.T) {
	objs := []AutoObject{{Class: "3", Type: "tag"}}
	boxes := reviewBoxes(objs, map[string]int{"3": 0})
	if len(boxes) != 0 {
		t.Fatalf("a tag was turned into a box: %+v", boxes)
	}
	raw, err := RenderFrame(flatPicture(1024, 1024, white), boxes, ReviewImageSide)
	if err != nil {
		t.Fatal(err)
	}
	img := decodeRendered(t, raw)
	// Every corner and the middle of the picture still carry the picture itself.
	for _, p := range [][2]int{{2, 2}, {256, 256}, {509, 509}, {50, 50}} {
		if !near(t, img, p[0], p[1], white, jpegTolerance) {
			t.Errorf("something was drawn at %d,%d, but a classification has nothing to draw", p[0], p[1])
		}
	}
	// The line of the prompt names the class, so that the model can judge it.
	if line := reviewLine(7, objs, map[string]int{"3": 0}); line != `frame 7: the image is annotated with the class "3" (colour red)` {
		t.Errorf("line of a classification: %q", line)
	}
	if line := reviewLine(7, nil, nil); line != "frame 7: nothing is annotated on this image" {
		t.Errorf("line of an empty image: %q", line)
	}
	// A detector's line names the colour of every drawn class.
	line := reviewLine(3, []AutoObject{{Class: "pig", Points: []float64{1, 2, 3, 4}}, {Class: "cow", Points: []float64{5, 6, 7, 8}}},
		map[string]int{"pig": 0, "cow": 1})
	if line != `frame 3: a frame in red marks an object of the class "pig"; a frame in green marks an object of the class "cow"` {
		t.Errorf("line of a detector: %q", line)
	}
}

// ColourOf walks through its colours and repeats from the ninth class on.
func TestColourOf(t *testing.T) {
	if name, _ := ColourOf(0); name != "red" {
		t.Errorf("the first colour is %q", name)
	}
	if name, _ := ColourOf(1); name != "green" {
		t.Errorf("the second colour is %q", name)
	}
	first, firstRGBA := ColourOf(0)
	again, againRGBA := ColourOf(len(reviewColours))
	if again != first || againRGBA != firstRGBA {
		t.Errorf("the ninth class is drawn in %q, expected %q again", again, first)
	}
	if name, _ := ColourOf(-1); name != "white" {
		t.Errorf("a negative place is drawn in %q, expected the last colour", name)
	}
}
