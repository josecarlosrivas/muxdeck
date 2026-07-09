// Command genicons renders the muxdeck PWA icons (a "deck" of session bars)
// with the standard library only, so icon regeneration needs nothing but Go:
//
//	go run ./tools/genicons web/icons
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

var (
	bg     = color.RGBA{0x14, 0x16, 0x1a, 0xff}
	accent = color.RGBA{0x5e, 0xea, 0xd4, 0xff}
)

func rect(img *image.RGBA, x0, y0, x1, y1 float64, c color.RGBA) {
	for y := int(y0); y < int(y1); y++ {
		for x := int(x0); x < int(x1); x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func render(size int) *image.RGBA {
	s := float64(size)
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	rect(img, 0, 0, s, s, bg)
	// Three session bars of differing lengths, plus a prompt block.
	bars := []struct{ y, h, w float64 }{
		{0.24, 0.10, 0.58},
		{0.42, 0.10, 0.42},
		{0.60, 0.10, 0.66},
	}
	for _, b := range bars {
		rect(img, 0.17*s, b.y*s, (0.17+b.w)*s, (b.y+b.h)*s, accent)
	}
	return img
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: genicons <output-dir>")
		os.Exit(2)
	}
	dir := os.Args[1]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, size := range map[string]int{
		"icon-192.png":         192,
		"icon-512.png":         512,
		"apple-touch-icon.png": 180,
	} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := png.Encode(f, render(size)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		f.Close()
	}
}
