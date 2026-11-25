//go:build windows

package audio

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

func TestParseWaveFormatSDK40ByteExtensibleLayout(t *testing.T) {
	// Exact 40-byte Windows SDK WAVEFORMATEXTENSIBLE packed layout (pshpack1.h):
	//   [0..17]  WAVEFORMATEX (18 bytes)
	//   [18..19] wValidBitsPerSample (2 bytes)
	//   [20..23] dwChannelMask (4 bytes)
	//   [24..39] SubFormat GUID (16 bytes)
	makeExt := func(rate uint32, ch, bits uint16, sub guid) []byte {
		b := make([]byte, 40)
		binary.LittleEndian.PutUint16(b[0:2], waveFormatExtensible)
		binary.LittleEndian.PutUint16(b[2:4], ch)
		binary.LittleEndian.PutUint32(b[4:8], rate)
		blockAlign := ch * (bits / 8)
		binary.LittleEndian.PutUint32(b[8:12], rate*uint32(blockAlign))
		binary.LittleEndian.PutUint16(b[12:14], blockAlign)
		binary.LittleEndian.PutUint16(b[14:16], bits)
		binary.LittleEndian.PutUint16(b[16:18], 22) // cbSize = 22
		binary.LittleEndian.PutUint16(b[18:20], bits)
		binary.LittleEndian.PutUint32(b[20:24], 0x3)
		binary.LittleEndian.PutUint32(b[24:28], sub.Data1)
		binary.LittleEndian.PutUint16(b[28:30], sub.Data2)
		binary.LittleEndian.PutUint16(b[30:32], sub.Data3)
		copy(b[32:40], sub.Data4[:])
		return b
	}

	bFloat := makeExt(48000, 2, 32, subtypeIEEEFloat)
	sf, err := parseWaveFormat((*waveFormatEx)(unsafe.Pointer(&bFloat[0])))
	if err != nil {
		t.Fatalf("parseWaveFormat(float32 extensible) failed: %v", err)
	}
	if sf.SampleRate != 48000 || sf.Channels != 2 || sf.Format != FormatFloat32 {
		t.Fatalf("unexpected float32 stream format: %+v", sf)
	}

	bPCM16 := makeExt(44100, 1, 16, subtypePCM)
	sf2, err := parseWaveFormat((*waveFormatEx)(unsafe.Pointer(&bPCM16[0])))
	if err != nil {
		t.Fatalf("parseWaveFormat(pcm16 extensible) failed: %v", err)
	}
	if sf2.SampleRate != 44100 || sf2.Channels != 1 || sf2.Format != FormatInt16 {
		t.Fatalf("unexpected pcm16 stream format: %+v", sf2)
	}
}

func TestRawPreRollRingWrapAndAlignment(t *testing.T) {
	var r rawPreRollRing
	r.configure(4096)
	// Push 3000 bytes then 3000 bytes -> should keep latest 4096 bytes in exact order
	p1 := make([]byte, 3000)
	p2 := make([]byte, 3000)
	for i := range p1 {
		p1[i] = 1
	}
	for i := range p2 {
		p2[i] = 2
	}
	r.push(p1)
	r.push(p2)
	out := r.drain(8)
	if len(out) != 4096 {
		t.Fatalf("drained len = %d, want 4096", len(out))
	}
	// First 1096 bytes should be from p1 (1), last 3000 bytes from p2 (2)
	if out[0] != 1 || out[1095] != 1 || out[1096] != 2 || out[4095] != 2 {
		t.Fatalf("wraparound ordering wrong: [0]=%d [1095]=%d [1096]=%d [4095]=%d", out[0], out[1095], out[1096], out[4095])
	}
}
