package audio

import (
	"encoding/binary"
	"math"
	"sync"
)

const (
	TargetSampleRate = 16000
	TargetChannels   = 1
	ChunkDurationMs  = 20
	ChunkSamples     = TargetSampleRate * ChunkDurationMs / 1000 // 320 samples
	ChunkBytes       = ChunkSamples * 2                          // 640 bytes (s16le)
)

// SampleFormat identifies the raw PCM encoding coming from WASAPI or a WAV file.
type SampleFormat int

const (
	FormatFloat32 SampleFormat = iota
	FormatInt16
	FormatInt24
	FormatInt32
)

func (f SampleFormat) String() string {
	switch f {
	case FormatFloat32:
		return "float32"
	case FormatInt16:
		return "int16"
	case FormatInt24:
		return "int24"
	case FormatInt32:
		return "int32"
	default:
		return "unknown"
	}
}

// StreamFormat describes an incoming raw audio stream format.
type StreamFormat struct {
	SampleRate int
	Channels   int
	Format     SampleFormat
}

// Resampler converts arbitrary-rate, multi-channel float/PCM audio into 16 kHz mono
// signed 16-bit little-endian PCM using a stateful Lanczos-3 windowed-sinc resampler.
type Resampler struct {
	srcRate   int
	dstRate   int
	channels  int
	format    SampleFormat
	ratio     float64 // srcRate / dstRate
	cutoff    float64 // anti-aliasing low-pass cutoff relative to source Nyquist
	taps      int     // half-width of sinc kernel in source samples
	history   []float64
	pos       float64 // current fractional read position in history
	outBuf    []byte  // leftover output bytes not yet a full 20ms chunk
}

// NewResampler creates a streaming resampler targeting 16 kHz mono PCM16.
func NewResampler(src StreamFormat) *Resampler {
	if src.SampleRate <= 0 {
		src.SampleRate = 48000
	}
	if src.Channels <= 0 {
		src.Channels = 1
	}
	ratio := float64(src.SampleRate) / float64(TargetSampleRate)
	cutoff := 1.0
	if ratio > 1.0 {
		// Low-pass slightly below target Nyquist to suppress aliasing cleanly.
		cutoff = 0.92 / ratio
	}
	const a = 3.0 // Lanczos-3 lobes
	taps := int(math.Ceil(a * math.Max(1.0, ratio)))
	if taps < 4 {
		taps = 4
	}
	return &Resampler{
		srcRate:  src.SampleRate,
		dstRate:  TargetSampleRate,
		channels: src.Channels,
		format:   src.Format,
		ratio:    ratio,
		cutoff:   cutoff,
		taps:     taps,
		history:  make([]float64, taps), // leading zero padding
		pos:      float64(taps),
	}
}

func sinc(x float64) float64 {
	if math.Abs(x) < 1e-9 {
		return 1.0
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

// lanczosKernel computes a low-pass filtered Lanczos-3 kernel value at offset dx (in source samples).
func (r *Resampler) lanczosKernel(dx float64) float64 {
	const a = 3.0
	// Scale window with downsampling factor so the filter always spans 2*a lobes of the output rate.
	scale := math.Max(1.0, r.ratio)
	u := dx / scale
	if math.Abs(u) >= a {
		return 0.0
	}
	return sinc(dx*r.cutoff) * sinc(u/a) * r.cutoff
}

// DecodeMonoFloat converts raw interleaved input bytes into mono float64 samples in [-1, 1].
func DecodeMonoFloat(raw []byte, channels int, format SampleFormat) []float64 {
	if channels <= 0 || len(raw) == 0 {
		return nil
	}
	var bps int
	switch format {
	case FormatFloat32, FormatInt32:
		bps = 4
	case FormatInt24:
		bps = 3
	case FormatInt16:
		bps = 2
	default:
		return nil
	}
	frameBytes := bps * channels
	numFrames := len(raw) / frameBytes
	if numFrames == 0 {
		return nil
	}

	mono := make([]float64, numFrames)
	invCh := 1.0 / float64(channels)

	for i := 0; i < numFrames; i++ {
		base := i * frameBytes
		var sum float64
		for ch := 0; ch < channels; ch++ {
			off := base + ch*bps
			var s float64
			switch format {
			case FormatFloat32:
				bits := binary.LittleEndian.Uint32(raw[off : off+4])
				v := float64(math.Float32frombits(bits))
				if math.IsNaN(v) || math.IsInf(v, 0) {
					v = 0
				}
				s = v
			case FormatInt16:
				v := int16(binary.LittleEndian.Uint16(raw[off : off+2]))
				s = float64(v) / 32768.0
			case FormatInt24:
				// Sign-extend 24-bit little-endian integer
				b0 := int32(raw[off])
				b1 := int32(raw[off+1]) << 8
				b2 := int32(int8(raw[off+2])) << 16
				s = float64(b0|b1|b2) / 8388608.0
			case FormatInt32:
				v := int32(binary.LittleEndian.Uint32(raw[off : off+4]))
				s = float64(v) / 2147483648.0
			}
			sum += s
		}
		mono[i] = sum * invCh
	}
	return mono
}

// Process takes raw interleaved bytes in the source format, resamples to 16 kHz mono,
// applies gainPercent (50..300), and returns:
//   - pcm16: converted signed 16-bit little-endian PCM bytes
//   - level: perceptual audio meter level in [0.0, 1.0]
func (r *Resampler) Process(raw []byte, gainPercent int) (pcm16 []byte, level float64) {
	mono := DecodeMonoFloat(raw, r.channels, r.format)
	if len(mono) == 0 {
		return nil, 0
	}
	return r.ProcessMonoFloat(mono, gainPercent)
}

// ProcessMonoFloat resamples mono float64 samples in [-1, 1] to 16 kHz s16le with gain.
func (r *Resampler) ProcessMonoFloat(mono []float64, gainPercent int) (pcm16 []byte, level float64) {
	if len(mono) == 0 {
		return nil, 0
	}
	gain := float64(gainPercent) / 100.0
	if gain < 0.1 {
		gain = 0.1
	}

	var outSamples []float64
	if r.srcRate == r.dstRate {
		// Fast path: 1:1 sample rate, no resampling needed
		outSamples = mono
	} else {
		r.history = append(r.history, mono...)
		limit := float64(len(r.history) - r.taps)
		if limit > r.pos {
			est := int((limit-r.pos)/r.ratio) + 2
			outSamples = make([]float64, 0, est)
			for r.pos < limit {
				center := int(math.Floor(r.pos))
				start := center - r.taps + 1
				if start < 0 {
					start = 0
				}
				end := center + r.taps
				if end >= len(r.history) {
					end = len(r.history) - 1
				}
				var acc, wsum float64
				for idx := start; idx <= end; idx++ {
					w := r.lanczosKernel(r.pos - float64(idx))
					acc += r.history[idx] * w
					wsum += w
				}
				if wsum > 1e-9 {
					acc /= wsum
				}
				outSamples = append(outSamples, acc)
				r.pos += r.ratio
			}
		}
		// Compact consumed history while retaining `taps` look-behind samples
		drop := int(math.Floor(r.pos)) - r.taps
		if drop > 0 && drop < len(r.history) {
			copy(r.history, r.history[drop:])
			r.history = r.history[:len(r.history)-drop]
			r.pos -= float64(drop)
		}
	}

	if len(outSamples) == 0 {
		return nil, 0
	}

	pcm16 = make([]byte, len(outSamples)*2)
	var sumSq float64
	for i, s := range outSamples {
		scaled := SoftClip(s * gain)
		sumSq += scaled * scaled
		sample := FloatToInt16(scaled)
		binary.LittleEndian.PutUint16(pcm16[i*2:i*2+2], uint16(sample))
	}
	rms := math.Sqrt(sumSq / float64(len(outSamples)))
	level = RMSToMeterLevel(rms)
	return pcm16, level
}

// Flush drains any remaining samples held in the resampler's lookahead window
// by padding with zeros. Call at the end of a finite input (e.g. a WAV file).
func (r *Resampler) Flush(gainPercent int) []byte {
	if r.srcRate == r.dstRate || r.taps <= 0 {
		return nil
	}
	pad := make([]float64, r.taps)
	pcm, _ := r.ProcessMonoFloat(pad, gainPercent)
	return pcm
}

// SoftClip smoothly limits peaks above 0.92 and strictly bounds output to [-1.0, 1.0].
func SoftClip(x float64) float64 {
	if math.IsNaN(x) {
		return 0
	}
	if x > 0.92 {
		// Smooth tanh knee between 0.92 and 1.0
		return 0.92 + 0.08*math.Tanh((x-0.92)/0.08)
	}
	if x < -0.92 {
		return -0.92 + 0.08*math.Tanh((x+0.92)/0.08)
	}
	return x
}

// FloatToInt16 converts a normalized sample in [-1.0, 1.0] to signed 16-bit PCM.
func FloatToInt16(x float64) int16 {
	if x >= 1.0 {
		return 32767
	}
	if x <= -1.0 {
		return -32768
	}
	v := math.Round(x * 32767.0)
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// RMSToMeterLevel maps linear RMS amplitude [0..1] to a perceptual meter level [0..1]
// using a -55 dBFS noise floor and -3 dBFS full-scale ceiling.
func RMSToMeterLevel(rms float64) float64 {
	if rms <= 1e-6 {
		return 0
	}
	db := 20.0 * math.Log10(rms)
	const minDB = -55.0
	const maxDB = -3.0
	if db <= minDB {
		return 0
	}
	if db >= maxDB {
		return 1
	}
	return (db - minDB) / (maxDB - minDB)
}

// ComputePCM16Level calculates the perceptual meter level [0..1] of s16le PCM bytes.
func ComputePCM16Level(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sumSq float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:i*2+2]))) / 32768.0
		sumSq += v * v
	}
	return RMSToMeterLevel(math.Sqrt(sumSq / float64(n)))
}

// Chunker accumulates arbitrary-sized s16le byte slices and emits fixed ChunkBytes (640 B = 20 ms) frames.
type Chunker struct {
	buf []byte
}

// Push appends pcm bytes and returns zero or more complete 20ms chunks.
func (c *Chunker) Push(pcm []byte) [][]byte {
	if len(pcm) == 0 {
		return nil
	}
	c.buf = append(c.buf, pcm...)
	var out [][]byte
	for len(c.buf) >= ChunkBytes {
		frame := make([]byte, ChunkBytes)
		copy(frame, c.buf[:ChunkBytes])
		out = append(out, frame)
		c.buf = c.buf[ChunkBytes:]
	}
	return out
}

// Flush returns any remaining partial bytes (zero-padded or raw) and resets the buffer.
func (c *Chunker) Flush() []byte {
	if len(c.buf) == 0 {
		return nil
	}
	rem := make([]byte, len(c.buf))
	copy(rem, c.buf)
	c.buf = c.buf[:0]
	return rem
}

// Reset discards buffered bytes.
func (c *Chunker) Reset() {
	c.buf = c.buf[:0]
}

// PreRollRing stores the most recent maxChunks of 20ms PCM frames while idle,
// so pressing the hotkey immediately flushes pre-roll audio with zero lost syllables.
type PreRollRing struct {
	mu        sync.Mutex
	maxChunks int
	chunks    [][]byte
}

// NewPreRollRing creates a ring holding up to maxChunks frames (e.g. 8 chunks = 160 ms).
func NewPreRollRing(maxChunks int) *PreRollRing {
	if maxChunks < 1 {
		maxChunks = 1
	}
	return &PreRollRing{
		maxChunks: maxChunks,
		chunks:    make([][]byte, 0, maxChunks),
	}
}

// Push adds a chunk copy, evicting the oldest chunk if full.
func (p *PreRollRing) Push(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	cp := make([]byte, len(chunk))
	copy(cp, chunk)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.chunks) >= p.maxChunks {
		copy(p.chunks, p.chunks[1:])
		p.chunks[len(p.chunks)-1] = cp
	} else {
		p.chunks = append(p.chunks, cp)
	}
}

// Drain returns all stored pre-roll chunks in chronological order and clears the ring.
func (p *PreRollRing) Drain() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.chunks) == 0 {
		return nil
	}
	out := make([][]byte, len(p.chunks))
	copy(out, p.chunks)
	p.chunks = p.chunks[:0]
	return out
}

// Clear empties the ring.
func (p *PreRollRing) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chunks = p.chunks[:0]
}
