// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package imagereview

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
)

// JPEGQuality of a rendered image: enough for a model, and it keeps the base64 of a 512 px picture at a
// few tens of kilobytes.
const JPEGQuality = 80

// Scale answers the size a picture is rendered at: its longest side is maxSide, whether that shrinks the
// picture or blows it up. A picture of 28 px (MNIST) would say almost nothing to a model at its own size,
// and one of 4000 px would cost without need: what is fixed is the size, not the direction.
func Scale(width, height, maxSide int) (int, int) {
	if width <= 0 || height <= 0 {
		return 0, 0
	}
	if maxSide <= 0 {
		return width, height
	}
	if width >= height {
		return maxSide, max(1, height*maxSide/width)
	}
	return max(1, width*maxSide/height), maxSide
}

// Resize scales a picture to the given size by picking the nearest pixel of the original. The standard
// library has no scaler of its own, and a smooth one would not read any better once the JPEG has been over
// it.
func Resize(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*b.Dx()/w, sy))
		}
	}
	return dst
}

// EncodeJPEG renders a picture at the given longest side and answers it as JPEG. Every picture goes out
// at this size, also a small one (V3d).
func EncodeJPEG(img image.Image, maxSide int) ([]byte, error) {
	b := img.Bounds()
	w, h := Scale(b.Dx(), b.Dy(), maxSide)
	if w <= 0 || h <= 0 {
		return nil, errors.New("the picture has no size")
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, Resize(img, w, h), &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
