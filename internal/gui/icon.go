package gui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"sync"
)

// The tray icons are drawn in memory: a colored disc with a white bolt. No
// image files ship with the binary.

const (
	iconSize = 64
	// iconSamples is the supersampling grid per pixel edge, for smooth
	// edges.
	iconSamples = 4
)

// toneColors are the disc colors.
var toneColors = map[Tone]color.NRGBA{
	ToneGreen: {R: 0x2e, G: 0xa0, B: 0x43, A: 0xff},
	ToneBlue:  {R: 0x6b, G: 0x5b, B: 0xe0, A: 0xff},
	ToneAmber: {R: 0xe0, G: 0x9b, B: 0x12, A: 0xff},
	ToneGray:  {R: 0x7d, G: 0x82, B: 0x8a, A: 0xff},
}

// Color returns the color that stands for tone.
func (t Tone) Color() color.NRGBA {
	if c, ok := toneColors[t]; ok {
		return c
	}
	return toneColors[ToneGray]
}

// Name is a short lowercase name for tone, used as the icon's resource name.
func (t Tone) Name() string {
	switch t {
	case ToneGreen:
		return "green"
	case ToneBlue:
		return "blue"
	case ToneAmber:
		return "amber"
	default:
		return "gray"
	}
}

// bolt is the lightning glyph, on the 64x64 grid.
var bolt = [][2]float64{{37, 9}, {17, 36}, {30, 36}, {26, 55}, {47, 27}, {34, 27}}

var (
	iconMu    sync.Mutex
	iconCache = map[Tone][]byte{}
)

// IconPNG returns the tray icon for tone as PNG bytes. The result is cached
// and must not be modified.
func IconPNG(t Tone) []byte {
	iconMu.Lock()
	defer iconMu.Unlock()
	if b, ok := iconCache[t]; ok {
		return b
	}
	var buf bytes.Buffer
	// Encoding an in-memory NRGBA image cannot fail.
	_ = png.Encode(&buf, drawIcon(t.Color()))
	iconCache[t] = buf.Bytes()
	return iconCache[t]
}

func drawIcon(disc color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	const center, radius = iconSize / 2.0, iconSize/2.0 - 2
	total := float64(iconSamples * iconSamples)
	for y := 0; y < iconSize; y++ {
		for x := 0; x < iconSize; x++ {
			var inDisc, inBolt int
			for sy := 0; sy < iconSamples; sy++ {
				for sx := 0; sx < iconSamples; sx++ {
					px := float64(x) + (float64(sx)+0.5)/iconSamples
					py := float64(y) + (float64(sy)+0.5)/iconSamples
					dx, dy := px-center, py-center
					if dx*dx+dy*dy > radius*radius {
						continue
					}
					inDisc++
					if inPolygon(bolt, px, py) {
						inBolt++
					}
				}
			}
			if inDisc == 0 {
				continue
			}
			// Blend white over the disc color by the bolt's coverage.
			w := float64(inBolt) / float64(inDisc)
			mix := func(c uint8) uint8 { return uint8(float64(c)*(1-w) + 255*w + 0.5) }
			img.SetNRGBA(x, y, color.NRGBA{
				R: mix(disc.R), G: mix(disc.G), B: mix(disc.B),
				A: uint8(255*float64(inDisc)/total + 0.5),
			})
		}
	}
	return img
}

// inPolygon is the even-odd rule.
func inPolygon(poly [][2]float64, x, y float64) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		xi, yi, xj, yj := poly[i][0], poly[i][1], poly[j][0], poly[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}
