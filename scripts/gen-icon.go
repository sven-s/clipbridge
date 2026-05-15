//go:build ignore

// gen-icon generates a 1024x1024 ClipSync app icon at assets/icon.png.
// Run via: go run scripts/gen-icon.go
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

func main() {
	const size = 1024
	const corner = 220 // squircle corner radius

	img := image.NewRGBA(image.Rect(0, 0, size, size))

	// Background: blue gradient squircle (rounded square)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if !inSquircle(x, y, size, corner) {
				continue
			}
			// Linear gradient: top-left lighter, bottom-right darker
			t := float64(x+y) / float64(2*size)
			r := lerp(82, 56, t)
			g := lerp(155, 110, t)
			b := lerp(255, 230, t)
			img.SetRGBA(x, y, color.RGBA{r, g, b, 255})
		}
	}

	// Two overlapping rings to suggest "sync"
	cx, cy := float64(size)/2, float64(size)/2
	off := 90.0
	drawRing(img, cx-off, cy, 250, 35, color.RGBA{255, 255, 255, 255})
	drawRing(img, cx+off, cy, 250, 35, color.RGBA{255, 255, 255, 230})

	if err := os.MkdirAll("assets", 0755); err != nil {
		panic(err)
	}
	f, err := os.Create("assets/icon.png")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

func inSquircle(x, y, size, r int) bool {
	// Inside rectangle minus rounded corners
	if x < r && y < r {
		dx, dy := float64(r-x), float64(r-y)
		return dx*dx+dy*dy <= float64(r*r)
	}
	if x >= size-r && y < r {
		dx, dy := float64(x-(size-r-1)), float64(r-y)
		return dx*dx+dy*dy <= float64(r*r)
	}
	if x < r && y >= size-r {
		dx, dy := float64(r-x), float64(y-(size-r-1))
		return dx*dx+dy*dy <= float64(r*r)
	}
	if x >= size-r && y >= size-r {
		dx, dy := float64(x-(size-r-1)), float64(y-(size-r-1))
		return dx*dx+dy*dy <= float64(r*r)
	}
	return true
}

func drawRing(img *image.RGBA, cx, cy, radius, thickness float64, c color.RGBA) {
	rOuter := radius
	rInner := radius - thickness
	for y := int(cy - rOuter); y <= int(cy+rOuter); y++ {
		for x := int(cx - rOuter); x <= int(cx+rOuter); x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			d := math.Sqrt(dx*dx + dy*dy)
			if d <= rOuter && d >= rInner {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func lerp(a, b float64, t float64) uint8 {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	return uint8(a + (b-a)*t)
}
