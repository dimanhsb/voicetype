//go:build windows

package sound

import (
	"encoding/binary"
	"math"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	winmm          = windows.NewLazySystemDLL("winmm.dll")
	procPlaySoundW = winmm.NewProc("PlaySoundW")
)

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndMemory    = 0x0004
)

type Cue int

const (
	CueStart Cue = iota
	CueSubmit
	CueEdit
	CueClear
	CueError
)

var (
	initOnce sync.Once
	wavStart []byte
	wavSub   []byte
	wavEdit  []byte
	wavClear []byte
	wavErr   []byte
)

type tone struct {
	freqHz     float64
	durationMs int
	gain       float64
}

func synthWAV(tones []tone) []byte {
	const sr = 22050
	var samples []int16
	for _, t := range tones {
		n := sr * t.durationMs / 1000
		for i := 0; i < n; i++ {
			tm := float64(i) / float64(sr)
			// Smooth Hann-like attack/decay envelope to avoid clicks
			env := 1.0
			attack := float64(sr) * 0.008
			decay := float64(n) * 0.65
			if float64(i) < attack {
				env = float64(i) / attack
			} else if float64(i) > float64(n)-decay {
				rem := float64(n-i) / decay
				env = rem * rem
			}
			v := math.Sin(2*math.Pi*t.freqHz*tm) * env * t.gain
			samples = append(samples, int16(v*32767))
		}
	}

	dataSize := uint32(len(samples) * 2)
	buf := make([]byte, 44+dataSize)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 36+dataSize)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], 1)
	binary.LittleEndian.PutUint32(buf[24:28], sr)
	binary.LittleEndian.PutUint32(buf[28:32], sr*2)
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataSize)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[44+i*2:44+i*2+2], uint16(s))
	}
	return buf
}

func ensureSynthesized() {
	initOnce.Do(func() {
		wavStart = synthWAV([]tone{{587.33, 55, 0.14}, {880.00, 75, 0.15}}) // D5 -> A5
		wavSub = synthWAV([]tone{{783.99, 50, 0.13}, {1046.50, 70, 0.12}})  // G5 -> C6
		wavEdit = synthWAV([]tone{{659.25, 65, 0.12}})                       // E5
		wavClear = synthWAV([]tone{{523.25, 50, 0.12}, {392.00, 70, 0.12}})  // C5 -> G4
		wavErr = synthWAV([]tone{{311.13, 90, 0.16}, {261.63, 120, 0.16}})   // Eb4 -> C4
	})
}

// Play triggers a subtle synthesized sound cue asynchronously if enabled is true.
func Play(cue Cue, enabled bool) {
	if !enabled {
		return
	}
	ensureSynthesized()
	var data []byte
	switch cue {
	case CueStart:
		data = wavStart
	case CueSubmit:
		data = wavSub
	case CueEdit:
		data = wavEdit
	case CueClear:
		data = wavClear
	case CueError:
		data = wavErr
	default:
		return
	}
	if len(data) == 0 {
		return
	}
	_, _, _ = procPlaySoundW.Call(
		uintptr(unsafe.Pointer(&data[0])),
		0,
		uintptr(sndMemory|sndAsync|sndNoDefault),
	)
}
