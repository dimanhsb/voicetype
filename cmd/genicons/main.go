// Command genicons generates anti-aliased PNG icons (16..256 px) and multi-size
// Windows .ico files for VoiceType using 4x supersampled signed-distance-field rendering.
// Design: A dark rounded tile with a teal microphone and sound arcs.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

type theme struct {
	name       string
	bgTop      [3]float64
	bgBot      [3]float64
	border     [3]float64
	micPrimary [3]float64
	micBright  [3]float64
	arcColor   [3]float64
	dotColor   [3]float64
	hasDot     bool
}

var themes = []theme{
	{
		name:       "voicetype",
		bgTop:      [3]float64{24, 30, 38},
		bgBot:      [3]float64{13, 17, 23},
		border:     [3]float64{45, 212, 191},
		micPrimary: [3]float64{20, 184, 166},
		micBright:  [3]float64{94, 234, 212},
		arcColor:   [3]float64{45, 212, 191},
		hasDot:     false,
	},
	{
		name:       "voicetype_rec",
		bgTop:      [3]float64{30, 22, 28},
		bgBot:      [3]float64{17, 13, 18},
		border:     [3]float64{244, 63, 94},
		micPrimary: [3]float64{244, 63, 94},
		micBright:  [3]float64{253, 164, 175},
		arcColor:   [3]float64{45, 212, 191},
		dotColor:   [3]float64{244, 63, 94},
		hasDot:     true,
	},
	{
		name:       "voicetype_edit",
		bgTop:      [3]float64{30, 27, 20},
		bgBot:      [3]float64{18, 16, 12},
		border:     [3]float64{245, 158, 11},
		micPrimary: [3]float64{245, 158, 11},
		micBright:  [3]float64{252, 211, 77},
		arcColor:   [3]float64{45, 212, 191},
		dotColor:   [3]float64{245, 158, 11},
		hasDot:     true,
	},
	{
		name:       "voicetype_err",
		bgTop:      [3]float64{36, 18, 20},
		bgBot:      [3]float64{20, 10, 12},
		border:     [3]float64{239, 68, 68},
		micPrimary: [3]float64{239, 68, 68},
		micBright:  [3]float64{252, 165, 165},
		arcColor:   [3]float64{239, 68, 68},
		dotColor:   [3]float64{239, 68, 68},
		hasDot:     true,
	},
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func smoothstep(edge0, edge1, x float64) float64 {
	t := clamp01((x - edge0) / (edge1 - edge0))
	return t * t * (3 - 2*t)
}

func lerp3(a, b [3]float64, t float64) [3]float64 {
	return [3]float64{
		a[0] + (b[0]-a[0])*t,
		a[1] + (b[1]-a[1])*t,
		a[2] + (b[2]-a[2])*t,
	}
}

func sdRoundedBox(px, py, bx, by, r float64) float64 {
	qx := math.Abs(px) - bx + r
	qy := math.Abs(py) - by + r
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
}

func sdCapsule(px, py, ax, ay, bx, by, r float64) float64 {
	pax, pay := px-ax, py-ay
	bax, bay := bx-ax, by-ay
	h := clamp01((pax*bax + pay*bay) / (bax*bax + bay*bay))
	return math.Hypot(pax-bax*h, pay-bay*h) - r
}

func sdArc(px, py, cx, cy, radius, halfThick, startAng, endAng float64) float64 {
	dx, dy := px-cx, py-cy
	ang := math.Atan2(dy, dx)
	// Normalize into [-pi, pi] relative to arc midpoint
	mid := 0.5 * (startAng + endAng)
	halfSpan := 0.5 * (endAng - startAng)
	diff := ang - mid
	for diff > math.Pi {
		diff -= 2 * math.Pi
	}
	for diff < -math.Pi {
		diff += 2 * math.Pi
	}
	if math.Abs(diff) <= halfSpan {
		return math.Abs(math.Hypot(dx, dy)-radius) - halfThick
	}
	// Distance to endpoints
	e1x := cx + radius*math.Cos(startAng)
	e1y := cy + radius*math.Sin(startAng)
	e2x := cx + radius*math.Cos(endAng)
	e2y := cy + radius*math.Sin(endAng)
	d1 := math.Hypot(px-e1x, py-e1y)
	d2 := math.Hypot(px-e2x, py-e2y)
	return math.Min(d1, d2) - halfThick
}

func blend(dst [4]float64, src [3]float64, alpha float64) [4]float64 {
	a := clamp01(alpha)
	outA := a + dst[3]*(1-a)
	if outA <= 1e-6 {
		return [4]float64{}
	}
	return [4]float64{
		(src[0]*a + dst[0]*dst[3]*(1-a)) / outA,
		(src[1]*a + dst[1]*dst[3]*(1-a)) / outA,
		(src[2]*a + dst[2]*dst[3]*(1-a)) / outA,
		outA,
	}
}

func renderIcon(size int, th theme) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	aa := 1.4 / float64(size)

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var acc [4]float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					u := ((float64(x)+(float64(sx)+0.5)/ss)/float64(size))*2.0 - 1.0
					v := ((float64(y)+(float64(sy)+0.5)/ss)/float64(size))*2.0 - 1.0

					var col [4]float64

					// 1. Dark rounded tile background
					dTile := sdRoundedBox(u, v, 0.90, 0.90, 0.28)
					tileAlpha := 1.0 - smoothstep(-aa, aa, dTile)
					if tileAlpha > 0 {
						tGrad := clamp01((v + 0.9) / 1.8)
						bg := lerp3(th.bgTop, th.bgBot, tGrad)
						// Subtle top inner sheen
						glow := math.Exp(-((u)*(u)+(v+0.45)*(v+0.45))*2.4) * 0.14
						bg = lerp3(bg, th.micBright, glow)
						col = blend(col, bg, tileAlpha)

						// Subtle rim highlight along rounded tile border
						borderDist := math.Abs(dTile + 0.025)
						borderAlpha := (1.0 - smoothstep(0.0, 0.032+aa, borderDist)) * 0.42 * tileAlpha
						col = blend(col, th.border, borderAlpha)
					}

					// 2. Microphone capsule (centered, slightly above middle)
					dCap := sdCapsule(u, v, 0.0, -0.34, 0.0, -0.02, 0.19)
					capAlpha := 1.0 - smoothstep(-aa, aa, dCap)
					if capAlpha > 0 {
						tMic := clamp01((v + 0.52) / 0.72)
						mc := lerp3(th.micBright, th.micPrimary, tMic)
						// Subtle horizontal highlight on capsule
						hl := clamp01(1.0 - math.Abs(u+0.06)/0.14)
						mc = lerp3(mc, [3]float64{255, 255, 255}, hl*0.18)
						col = blend(col, mc, capAlpha)
					}

					// 3. Microphone cradle U-arc (bottom half circle)
					cradleThick := 0.052
					if size <= 24 {
						cradleThick = 0.068
					}
					dCradle := sdArc(u, v, 0.0, -0.03, 0.31, cradleThick, 0.08, math.Pi-0.08)
					// 4. Vertical stem & base
					dStem := sdCapsule(u, v, 0.0, 0.28, 0.0, 0.50, cradleThick*0.92)
					dBase := sdCapsule(u, v, -0.20, 0.52, 0.20, 0.52, cradleThick*0.92)
					dStand := math.Min(dCradle, math.Min(dStem, dBase))
					standAlpha := 1.0 - smoothstep(-aa, aa, dStand)
					if standAlpha > 0 {
						col = blend(col, th.micPrimary, standAlpha)
					}

					// 5. Sound arcs (left and right of capsule)
					arcThick := 0.042
					if size <= 24 {
						arcThick = 0.058
					}
					// Inner arcs
					dArcR1 := sdArc(u, v, 0.0, -0.18, 0.43, arcThick, -0.62, 0.62)
					dArcL1 := sdArc(u, v, 0.0, -0.18, 0.43, arcThick, math.Pi-0.62, math.Pi+0.62)
					dArcInner := math.Min(dArcR1, dArcL1)
					aInner := (1.0 - smoothstep(-aa, aa, dArcInner)) * 0.92
					if aInner > 0 {
						col = blend(col, th.arcColor, aInner)
					}

					// Outer arcs (show cleanly at >= 20px)
					if size >= 20 {
						dArcR2 := sdArc(u, v, 0.0, -0.18, 0.62, arcThick*0.92, -0.58, 0.58)
						dArcL2 := sdArc(u, v, 0.0, -0.18, 0.62, arcThick*0.92, math.Pi-0.58, math.Pi+0.58)
						dArcOuter := math.Min(dArcR2, dArcL2)
						aOuter := (1.0 - smoothstep(-aa, aa, dArcOuter)) * 0.58
						if aOuter > 0 {
							col = blend(col, th.arcColor, aOuter)
						}
					}

					// 6. Optional corner status dot for recording / editing / error tray states
					if th.hasDot {
						dDot := math.Hypot(u-0.56, v+0.56) - 0.20
						aDot := 1.0 - smoothstep(-aa, aa, dDot)
						if aDot > 0 {
							col = blend(col, th.dotColor, aDot)
						}
					}

					acc[0] += col[0] * col[3]
					acc[1] += col[1] * col[3]
					acc[2] += col[2] * col[3]
					acc[3] += col[3]
				}
			}
			inv := 1.0 / float64(ss*ss)
			a := acc[3] * inv
			var r, g, b uint8
			if a > 1e-6 {
				r = uint8(math.Round(clamp01((acc[0]*inv/a)/255.0) * 255.0))
				g = uint8(math.Round(clamp01((acc[1]*inv/a)/255.0) * 255.0))
				b = uint8(math.Round(clamp01((acc[2]*inv/a)/255.0) * 255.0))
			}
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: uint8(math.Round(clamp01(a) * 255.0))})
		}
	}
	return img
}

func encodeICO(pngs map[int][]byte, sizes []int) ([]byte, error) {
	var buf bytes.Buffer
	// ICONDIR header (6 bytes)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))          // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))          // type = 1 (ICO)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(sizes))) // count

	offset := uint32(6 + 16*len(sizes))
	for _, sz := range sizes {
		data := pngs[sz]
		w := byte(sz)
		h := byte(sz)
		if sz >= 256 {
			w = 0
			h = 0
		}
		buf.WriteByte(w)
		buf.WriteByte(h)
		buf.WriteByte(0) // palette
		buf.WriteByte(0) // reserved
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1))          // planes
		_ = binary.Write(&buf, binary.LittleEndian, uint16(32))         // bpp
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(data)))  // size
		_ = binary.Write(&buf, binary.LittleEndian, offset)             // offset
		offset += uint32(len(data))
	}
	for _, sz := range sizes {
		buf.Write(pngs[sz])
	}
	return buf.Bytes(), nil
}

func main() {
	outDir := filepath.Join("assets")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		panic(err)
	}
	// Also embed ICO copies in internal/tray/icons for //go:embed
	trayIconsDir := filepath.Join("internal", "tray", "icons")
	if err := os.MkdirAll(trayIconsDir, 0o755); err != nil {
		panic(err)
	}

	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	for _, th := range themes {
		pngData := make(map[int][]byte, len(sizes))
		for _, sz := range sizes {
			img := renderIcon(sz, th)
			var b bytes.Buffer
			if err := png.Encode(&b, img); err != nil {
				panic(err)
			}
			pngData[sz] = b.Bytes()
			if th.name == "voicetype" {
				p := filepath.Join(outDir, fmt.Sprintf("icon_%d.png", sz))
				if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
					panic(err)
				}
			}
		}
		icoBytes, err := encodeICO(pngData, sizes)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(outDir, th.name+".ico"), icoBytes, 0o644); err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(trayIconsDir, th.name+".ico"), icoBytes, 0o644); err != nil {
			panic(err)
		}
	}
	fmt.Println("Generated icons in assets/ and internal/tray/icons/")
}
