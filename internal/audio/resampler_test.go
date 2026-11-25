package audio

import (
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"
)

func makeFloat32StereoSine(sampleRate int, freqHz float64, durationSec float64, amp float64) []byte {
	n := int(float64(sampleRate) * durationSec)
	buf := make([]byte, n*2*4)
	for i := 0; i < n; i++ {
		v := float32(amp * math.Sin(2*math.Pi*freqHz*float64(i)/float64(sampleRate)))
		bits := math.Float32bits(v)
		// Same signal in L and R
		binary.LittleEndian.PutUint32(buf[i*8:i*8+4], bits)
		binary.LittleEndian.PutUint32(buf[i*8+4:i*8+8], bits)
	}
	return buf
}

func TestResampler48kStereoFloatTo16kMono(t *testing.T) {
	raw := makeFloat32StereoSine(48000, 1000.0, 0.5, 0.5)
	r := NewResampler(StreamFormat{SampleRate: 48000, Channels: 2, Format: FormatFloat32})

	// Feed in realistic WASAPI 10ms chunks (480 frames * 8 bytes = 3840 bytes)
	var out []byte
	const step = 480 * 8
	for off := 0; off < len(raw); off += step {
		end := off + step
		if end > len(raw) {
			end = len(raw)
		}
		pcm, _ := r.Process(raw[off:end], 100)
		out = append(out, pcm...)
	}

	outSamples := len(out) / 2
	// Expected ~8000 samples (0.5s * 16000 Hz) minus filter lookahead (~10 samples)
	if outSamples < 7950 || outSamples > 8050 {
		t.Fatalf("unexpected resampled sample count: got %d, want ~8000", outSamples)
	}

	// Verify dominant frequency via zero-crossing count on middle region (avoiding startup transient)
	zeroCrossings := 0
	prev := int16(binary.LittleEndian.Uint16(out[2000:2002]))
	for i := 1001; i < 7000; i++ {
		cur := int16(binary.LittleEndian.Uint16(out[i*2 : i*2+2]))
		if (prev <= 0 && cur > 0) || (prev >= 0 && cur < 0) {
			zeroCrossings++
		}
		prev = cur
	}
	// 6000 samples at 16 kHz = 0.375 s; 1000 Hz sine has 2000 zero-crossings/sec -> ~750 crossings
	if zeroCrossings < 730 || zeroCrossings > 770 {
		t.Fatalf("unexpected zero crossings for 1kHz sine: got %d, want ~750", zeroCrossings)
	}
}

func TestResampler44100Int16And24Bit(t *testing.T) {
	// 44.1 kHz mono int16
	n := 4410 // 100 ms
	raw16 := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(12000 * math.Sin(2*math.Pi*440*float64(i)/44100))
		binary.LittleEndian.PutUint16(raw16[i*2:i*2+2], uint16(v))
	}
	r16 := NewResampler(StreamFormat{SampleRate: 44100, Channels: 1, Format: FormatInt16})
	pcm, lvl := r16.Process(raw16, 100)
	if len(pcm)/2 < 1550 || len(pcm)/2 > 1650 {
		t.Fatalf("44.1k->16k expected ~1600 samples, got %d", len(pcm)/2)
	}
	if lvl <= 0.1 {
		t.Fatalf("expected non-zero meter level, got %f", lvl)
	}

	// 24-bit signed negative value decoding sanity check
	raw24 := []byte{0x00, 0x00, 0xC0} // -0.5 in 24-bit two's complement (-4194304 / 8388608)
	mono := DecodeMonoFloat(raw24, 1, FormatInt24)
	if len(mono) != 1 || math.Abs(mono[0]-(-0.5)) > 1e-4 {
		t.Fatalf("24-bit decode failed: got %v, want -0.5", mono)
	}
}

func TestGainAndSoftClipAndMeterLevel(t *testing.T) {
	// Silence should yield meter level 0
	if lvl := RMSToMeterLevel(0); lvl != 0 {
		t.Fatalf("silence level = %f, want 0", lvl)
	}
	// Increasing RMS yields monotonically increasing meter level
	l1 := RMSToMeterLevel(0.01)
	l2 := RMSToMeterLevel(0.1)
	l3 := RMSToMeterLevel(0.7)
	if !(l1 < l2 && l2 < l3 && l3 <= 1.0) {
		t.Fatalf("meter levels not monotonic: %f, %f, %f", l1, l2, l3)
	}

	// 300% gain on loud 0.8 signal must soft-clip without int16 wraparound!
	r := NewResampler(StreamFormat{SampleRate: 16000, Channels: 1, Format: FormatFloat32})
	raw := make([]byte, 160*4)
	for i := 0; i < 160; i++ {
		binary.LittleEndian.PutUint32(raw[i*4:i*4+4], math.Float32bits(0.8))
	}
	pcm, lvl := r.Process(raw, 300)
	if lvl < 0.9 {
		t.Fatalf("expected high meter level for 300%% clipped signal, got %f", lvl)
	}
	for i := 0; i < len(pcm)/2; i++ {
		s := int16(binary.LittleEndian.Uint16(pcm[i*2 : i*2+2]))
		if s < 30000 {
			t.Fatalf("sample %d wrapped or dropped under 300%% gain: %d", i, s)
		}
	}
}

func TestChunkerAndPreRollRing(t *testing.T) {
	var ch Chunker
	// Push 1000 bytes -> 1 chunk of 640, 360 left
	c1 := ch.Push(make([]byte, 1000))
	if len(c1) != 1 || len(c1[0]) != ChunkBytes {
		t.Fatalf("expected 1 chunk of %d, got %d", ChunkBytes, len(c1))
	}
	// Push 300 bytes -> 660 total -> 1 chunk of 640, 20 left
	c2 := ch.Push(make([]byte, 300))
	if len(c2) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(c2))
	}
	rem := ch.Flush()
	if len(rem) != 20 {
		t.Fatalf("expected 20 flush bytes, got %d", len(rem))
	}

	// PreRollRing holds max 3 chunks in FIFO order
	ring := NewPreRollRing(3)
	for i := byte(1); i <= 5; i++ {
		ring.Push([]byte{i, i})
	}
	drained := ring.Drain()
	if len(drained) != 3 {
		t.Fatalf("expected 3 drained chunks, got %d", len(drained))
	}
	if drained[0][0] != 3 || drained[1][0] != 4 || drained[2][0] != 5 {
		t.Fatalf("unexpected pre-roll order: %v", drained)
	}
	if len(ring.Drain()) != 0 {
		t.Fatalf("ring should be empty after drain")
	}
}

func TestWAVWriteAndReadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test.wav")
	w, err := CreateWAV(p)
	if err != nil {
		t.Fatalf("CreateWAV: %v", err)
	}
	pcm := make([]byte, ChunkBytes*5)
	for i := 0; i < len(pcm)/2; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:i*2+2], uint16(int16(i*10)))
	}
	if err := w.WritePCM16(pcm); err != nil {
		t.Fatalf("WritePCM16: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wav, err := ReadWAV(p)
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if wav.Format.SampleRate != TargetSampleRate || wav.Format.Channels != 1 || wav.Format.Format != FormatInt16 {
		t.Fatalf("unexpected wav format: %+v", wav.Format)
	}
	if len(wav.Data) != len(pcm) {
		t.Fatalf("data length mismatch: got %d, want %d", len(wav.Data), len(pcm))
	}
}

func TestResamplerFlushShortInput(t *testing.T) {
	// Very short 10ms 48kHz stereo input (480 frames -> ~160 output samples)
	raw := makeFloat32StereoSine(48000, 500.0, 0.01, 0.4)
	r := NewResampler(StreamFormat{SampleRate: 48000, Channels: 2, Format: FormatFloat32})
	out, _ := r.Process(raw, 100)
	out = append(out, r.Flush(100)...)
	if len(out)/2 < 150 {
		t.Fatalf("expected >=150 samples after Flush on 10ms 48kHz audio, got %d", len(out)/2)
	}
}
