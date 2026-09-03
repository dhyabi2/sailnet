// Renders the Sailnet mark (the same geometry as brand/logo.svg) to a PNG:
//
//	go run ./cmd/brandicon out.png [size]
//
// Navy and light green only, no anti-aliasing tricks, pixel-exact at any size.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
)

var (
	navy   = color.NRGBA{0x0C, 0x1B, 0x33, 255}
	green  = color.NRGBA{0x8C, 0xF0, 0xBE, 255}
	green2 = color.NRGBA{0xD2, 0xF7, 0xE4, 255} // the aft sail, a lighter green
)

func inTri(px, py, x1, y1, x2, y2, x3, y3 float64) bool {
	d1 := (px-x2)*(y1-y2) - (x1-x2)*(py-y2)
	d2 := (px-x3)*(y2-y3) - (x2-x3)*(py-y3)
	d3 := (px-x1)*(y3-y1) - (x3-x1)*(py-y1)
	neg := d1 < 0 || d2 < 0 || d3 < 0
	pos := d1 > 0 || d2 > 0 || d3 > 0
	return !(neg && pos)
}

func inQuad(px, py float64, q [8]float64) bool {
	return inTri(px, py, q[0], q[1], q[2], q[3], q[4], q[5]) || inTri(px, py, q[0], q[1], q[4], q[5], q[6], q[7])
}

func main() {
	out, n := "icon-1024.png", 1024
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if len(os.Args) > 2 {
		fmt.Sscan(os.Args[2], &n)
	}
	s := float64(n) / 48
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			px, py := (float64(x)+0.5)/s, (float64(y)+0.5)/s
			c := navy
			if inTri(px, py, 24, 4, 24, 30, 8, 30) {
				c = green
			}
			if inTri(px, py, 27, 8, 27, 30, 40, 30) {
				c = green2
			}
			if inQuad(px, py, [8]float64{6, 34, 42, 34, 38, 42, 10, 42}) {
				c = green
			}
			img.SetNRGBA(x, y, c)
		}
	}
	f, err := os.Create(out)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	png.Encode(f, img)
}
