// Renders brand/icon-1024.png from the same geometry as logo.svg, so the app
// icon needs no SVG rasteriser and is pixel-exact monochrome.
package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
)

func inTri(px, py, x1, y1, x2, y2, x3, y3 float64) bool {
	d1 := (px-x2)*(y1-y2) - (x1-x2)*(py-y2)
	d2 := (px-x3)*(y2-y3) - (x2-x3)*(py-y3)
	d3 := (px-x1)*(y3-y1) - (x3-x1)*(py-y1)
	neg := d1 < 0 || d2 < 0 || d3 < 0
	pos := d1 > 0 || d2 > 0 || d3 > 0
	return !(neg && pos)
}

func main() {
	const n = 1024
	s := float64(n) / 256
	img := image.NewGray(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			px, py := (float64(x)+0.5)/s, (float64(y)+0.5)/s
			c := uint8(0)
			if inTri(px, py, 72, 200, 72, 56, 184, 200) {
				c = 255
			}
			if inTri(px, py, 96, 200, 96, 128, 152, 200) {
				c = 0
			}
			if px >= 56 && px < 200 && py >= 208 && py < 220 {
				c = 255
			}
			img.SetGray(x, y, color.Gray{c})
		}
	}
	out := "icon-1024.png"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	f, _ := os.Create(out)
	defer f.Close()
	png.Encode(f, img)
}
