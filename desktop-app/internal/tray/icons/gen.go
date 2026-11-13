//go:build ignore

// gen draws the tray's template icons: a house outline while Share is off,
// a filled house while it is on. macOS tints template icons for the menu
// bar, so only the alpha channel matters. Run it with `go generate ./...`.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

const (
	size   = 64 // pixels; Wails scales the image to the menu bar's height
	sample = 4  // subpixels per pixel and axis, for smooth edges
	stroke = 5.0
)

type pt struct{ x, y float64 }

// The house, in a 64×64 box: a roof chevron, the walls and a door.
var (
	roof  = []pt{{10, 31}, {32, 11}, {54, 31}}
	walls = []pt{{17, 26}, {17, 52}, {47, 52}, {47, 26}}
	door  = []pt{{28, 52}, {28, 41}, {36, 41}, {36, 52}}
	// The filled silhouette: roof triangle and walls, door cut out.
	silhouette = []pt{{32, 8.5}, {57.5, 31.5}, {49.5, 31.5}, {49.5, 54.5}, {14.5, 54.5}, {14.5, 31.5}, {6.5, 31.5}}
	doorHole   = []pt{{28.5, 54.5}, {28.5, 42}, {35.5, 42}, {35.5, 54.5}}
)

func main() {
	write("share-off.png", func(p pt) bool {
		return nearLine(p, roof) || nearLine(p, walls) || nearLine(p, door)
	})
	write("share-on.png", func(p pt) bool {
		return inside(p, silhouette) && !inside(p, doorHole)
	})
}

func write(name string, covered func(pt) bool) {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			n := 0
			for sy := range sample {
				for sx := range sample {
					p := pt{float64(x) + (float64(sx)+0.5)/sample, float64(y) + (float64(sy)+0.5)/sample}
					if covered(p) {
						n++
					}
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{A: uint8(255 * n / (sample * sample))})
		}
	}
	f, err := os.Create(name)
	if err != nil {
		log.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}

// nearLine reports whether p is within half a stroke of the polyline, which
// gives round joins and caps.
func nearLine(p pt, line []pt) bool {
	for i := 1; i < len(line); i++ {
		if distToSegment(p, line[i-1], line[i]) <= stroke/2 {
			return true
		}
	}
	return false
}

func distToSegment(p, a, b pt) float64 {
	dx, dy := b.x-a.x, b.y-a.y
	t := ((p.x-a.x)*dx + (p.y-a.y)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(p.x-(a.x+t*dx), p.y-(a.y+t*dy))
}

// inside is the even-odd rule.
func inside(p pt, poly []pt) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		a, b := poly[i], poly[j]
		if (a.y > p.y) != (b.y > p.y) && p.x < (b.x-a.x)*(p.y-a.y)/(b.y-a.y)+a.x {
			in = !in
		}
	}
	return in
}
