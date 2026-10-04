// Command icongen renders client2api.ico, the icon both the executables and the
// installer carry.
//
// The mark is a hub with three spokes -- one API surface in front of several
// upstream providers.  Everything is drawn as geometry (no font, no SVG
// rasteriser) and rendered at 4x before a box filter, so the 16px frame stays
// legible and the edges stay clean.
//
// The ICO is written with uncompressed 32-bit DIB frames rather than PNG
// frames: NSIS reads the file directly for the installer's own icon, and older
// shell code paths still expect DIB below 256px.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
)

// ss is the supersampling factor: frames are drawn at size*ss and box-filtered
// back down, which is where the antialiasing comes from.
const ss = 4

func main() {
	out := flag.String("out", "client2api.ico", "path of the .ico to write")
	flag.Parse()

	// 16 through 256 covers the shell's small icons, the taskbar, and the
	// "extra large" Explorer view.
	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	frames := make([]*image.RGBA, 0, len(sizes))
	for _, size := range sizes {
		frames = append(frames, draw(size))
	}
	if err := writeICO(*out, frames); err != nil {
		fmt.Fprintln(os.Stderr, "icongen:", err)
		os.Exit(1)
	}
}

// palette is the icon's gradient: a sky-to-blue sweep that reads as software
// without landing on the purple end of the scale.
var (
	bgTop    = color.RGBA{R: 0x0e, G: 0xa5, B: 0xe9, A: 0xff}
	bgBottom = color.RGBA{R: 0x25, G: 0x63, B: 0xeb, A: 0xff}
	ink      = color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
)

func draw(size int) *image.RGBA {
	n := size * ss
	buf := image.NewRGBA(image.Rect(0, 0, n, n))
	f := float64(n)

	// Rounded-square background, inset so the shape does not touch the canvas.
	pad := f * 0.055
	radius := f * 0.225
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			if !inRoundRect(px, py, pad, pad, f-pad, f-pad, radius) {
				continue
			}
			t := (px + py) / (2 * f)
			buf.SetRGBA(x, y, mix(bgTop, bgBottom, t))
		}
	}

	// Hub-and-spoke glyph: three providers feeding one centre.
	const (
		spokeAngle  = -90 // first spoke points straight up
		spokeSpread = 120
	)
	center := [2]float64{f / 2, f / 2}
	nodeDist := f * 0.275
	nodeR := f * 0.088
	centerR := f * 0.105
	stroke := f * 0.058

	nodes := make([][2]float64, 0, 3)
	for i := 0; i < 3; i++ {
		rad := (spokeAngle + float64(i)*spokeSpread) * math.Pi / 180
		nodes = append(nodes, [2]float64{
			center[0] + nodeDist*math.Cos(rad),
			center[1] + nodeDist*math.Sin(rad),
		})
	}

	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5

			// Spokes sit under the nodes and the centre.
			for _, node := range nodes {
				if distToSegment(px, py, center[0], center[1], node[0], node[1]) <= stroke/2 {
					over(buf, x, y, ink, 0.72)
					break
				}
			}
			for _, node := range nodes {
				if dist(px, py, node[0], node[1]) <= nodeR {
					over(buf, x, y, ink, 1)
					break
				}
			}
			if dist(px, py, center[0], center[1]) <= centerR {
				over(buf, x, y, ink, 1)
			}
		}
	}

	return downsample(buf, size)
}

func inRoundRect(px, py, x0, y0, x1, y1, r float64) bool {
	cx := clamp(px, x0+r, x1-r)
	cy := clamp(py, y0+r, y1-r)
	return dist(px, py, cx, cy) <= r
}

func dist(ax, ay, bx, by float64) float64 {
	return math.Hypot(ax-bx, ay-by)
}

func distToSegment(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	denom := dx*dx + dy*dy
	if denom == 0 {
		return dist(px, py, ax, ay)
	}
	t := clamp(((px-ax)*dx+(py-ay)*dy)/denom, 0, 1)
	return dist(px, py, ax+t*dx, ay+t*dy)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	t = clamp(t, 0, 1)
	lerp := func(x, y uint8) uint8 {
		return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t))
	}
	return color.RGBA{R: lerp(a.R, b.R), G: lerp(a.G, b.G), B: lerp(a.B, b.B), A: lerp(a.A, b.A)}
}

// over composites src onto the buffer at the given coverage.
func over(buf *image.RGBA, x, y int, src color.RGBA, cov float64) {
	dst := buf.RGBAAt(x, y)
	a := cov * float64(src.A) / 255
	buf.SetRGBA(x, y, color.RGBA{
		R: uint8(math.Round(float64(src.R)*a + float64(dst.R)*(1-a))),
		G: uint8(math.Round(float64(src.G)*a + float64(dst.G)*(1-a))),
		B: uint8(math.Round(float64(src.B)*a + float64(dst.B)*(1-a))),
		A: uint8(math.Round(float64(src.A)*a + float64(dst.A)*(1-a))),
	})
}

// downsample box-filters the supersampled buffer.  Samples are accumulated
// premultiplied so edge pixels do not pick up a dark fringe.
func downsample(buf *image.RGBA, size int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var sumA, sumR, sumG, sumB float64
			for dy := 0; dy < ss; dy++ {
				for dx := 0; dx < ss; dx++ {
					c := buf.RGBAAt(x*ss+dx, y*ss+dy)
					a := float64(c.A) / 255
					sumA += a
					sumR += float64(c.R) * a
					sumG += float64(c.G) * a
					sumB += float64(c.B) * a
				}
			}
			cover := sumA / float64(ss*ss)
			if sumA == 0 {
				out.SetRGBA(x, y, color.RGBA{})
				continue
			}
			scale := 1 / sumA
			out.SetRGBA(x, y, color.RGBA{
				R: uint8(math.Round(clamp(sumR*scale, 0, 255))),
				G: uint8(math.Round(clamp(sumG*scale, 0, 255))),
				B: uint8(math.Round(clamp(sumB*scale, 0, 255))),
				A: uint8(math.Round(cover * 255)),
			})
		}
	}
	return out
}

func writeICO(path string, frames []*image.RGBA) error {
	var entries, payload bytes.Buffer
	offset := 6 + 16*len(frames)

	for _, frame := range frames {
		dib := encodeDIB(frame)
		w, h := frame.Bounds().Dx(), frame.Bounds().Dy()
		// 256 is encoded as 0 in the directory, per the ICO format.
		bw, bh := byte(w), byte(h)
		if w >= 256 {
			bw = 0
		}
		if h >= 256 {
			bh = 0
		}
		entries.Write([]byte{bw, bh, 0, 0})
		writeU16(&entries, 1)  // colour planes
		writeU16(&entries, 32) // bits per pixel
		writeU32(&entries, uint32(len(dib)))
		writeU32(&entries, uint32(offset))
		offset += len(dib)
		payload.Write(dib)
	}

	var out bytes.Buffer
	writeU16(&out, 0) // reserved
	writeU16(&out, 1) // type: icon
	writeU16(&out, uint16(len(frames)))
	out.Write(entries.Bytes())
	out.Write(payload.Bytes())
	return os.WriteFile(path, out.Bytes(), 0o644)
}

// encodeDIB renders one frame as a BITMAPINFOHEADER followed by bottom-up BGRA
// pixels and a fully-opaque AND mask (the alpha channel carries transparency).
func encodeDIB(img *image.RGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	var b bytes.Buffer

	writeU32(&b, 40)            // biSize
	writeU32(&b, uint32(w))     // biWidth
	writeU32(&b, uint32(h*2))   // biHeight: XOR image + AND mask
	writeU16(&b, 1)             // biPlanes
	writeU16(&b, 32)            // biBitCount
	writeU32(&b, 0)             // biCompression: BI_RGB
	writeU32(&b, uint32(w*h*4)) // biSizeImage
	writeU32(&b, 0)             // biXPelsPerMeter
	writeU32(&b, 0)             // biYPelsPerMeter
	writeU32(&b, 0)             // biClrUsed
	writeU32(&b, 0)             // biClrImportant

	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.RGBAAt(x, y)
			b.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}

	rowBytes := ((w + 31) / 32) * 4
	for y := h - 1; y >= 0; y-- {
		b.Write(make([]byte, rowBytes))
	}
	return b.Bytes()
}

func writeU16(b *bytes.Buffer, v uint16) {
	_ = binary.Write(b, binary.LittleEndian, v)
}

func writeU32(b *bytes.Buffer, v uint32) {
	_ = binary.Write(b, binary.LittleEndian, v)
}
