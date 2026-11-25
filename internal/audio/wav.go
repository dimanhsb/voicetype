package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// WAVData holds decoded raw PCM bytes and their stream format from a WAV file.
type WAVData struct {
	Format StreamFormat
	Data   []byte
}

// ReadWAV reads a RIFF/WAVE file (PCM 16/24/32-bit or IEEE float 32-bit) into memory.
func ReadWAV(path string) (*WAVData, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read wav: %w", err)
	}
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, fmt.Errorf("invalid wav header in %s", path)
	}

	var fmtFound, dataFound bool
	var audioFormat uint16
	var channels uint16
	var sampleRate uint32
	var bitsPerSample uint16
	var pcmData []byte

	pos := 12
	for pos+8 <= len(b) {
		chunkID := string(b[pos : pos+4])
		chunkSize := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		pos += 8
		if chunkSize < 0 || pos+chunkSize > len(b) {
			break
		}
		chunk := b[pos : pos+chunkSize]
		switch chunkID {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, fmt.Errorf("short fmt chunk in %s", path)
			}
			audioFormat = binary.LittleEndian.Uint16(chunk[0:2])
			channels = binary.LittleEndian.Uint16(chunk[2:4])
			sampleRate = binary.LittleEndian.Uint32(chunk[4:8])
			bitsPerSample = binary.LittleEndian.Uint16(chunk[14:16])
			// Handle WAVE_FORMAT_EXTENSIBLE (0xFFFE)
			if audioFormat == 0xFFFE && len(chunk) >= 40 {
				subFormatTag := binary.LittleEndian.Uint16(chunk[24:26])
				audioFormat = subFormatTag
			}
			fmtFound = true
		case "data":
			pcmData = make([]byte, len(chunk))
			copy(pcmData, chunk)
			dataFound = true
		}
		pos += chunkSize
		if chunkSize%2 == 1 {
			pos++ // RIFF word alignment
		}
	}

	if !fmtFound || !dataFound {
		return nil, fmt.Errorf("missing fmt or data chunk in %s", path)
	}

	var sf SampleFormat
	switch {
	case audioFormat == 3 && bitsPerSample == 32:
		sf = FormatFloat32
	case audioFormat == 1 && bitsPerSample == 16:
		sf = FormatInt16
	case audioFormat == 1 && bitsPerSample == 24:
		sf = FormatInt24
	case audioFormat == 1 && bitsPerSample == 32:
		sf = FormatInt32
	default:
		return nil, fmt.Errorf("unsupported wav format tag=%d bits=%d", audioFormat, bitsPerSample)
	}

	return &WAVData{
		Format: StreamFormat{
			SampleRate: int(sampleRate),
			Channels:   int(channels),
			Format:     sf,
		},
		Data: pcmData,
	}, nil
}

// WAVWriter streams 16 kHz mono signed 16-bit PCM frames to a WAV file on disk.
// Used by the debug setting save_debug_wavs to capture regression audio samples.
type WAVWriter struct {
	mu        sync.Mutex
	file      *os.File
	dataBytes uint32
	closed    bool
}

// CreateWAV opens a new 16 kHz mono s16le WAV file and writes a placeholder header.
func CreateWAV(path string) (*WAVWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir wav dir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create wav: %w", err)
	}
	header := makeWAVHeader(TargetSampleRate, TargetChannels, 16, 0)
	if _, err := f.Write(header); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &WAVWriter{file: f}, nil
}

// WritePCM16 appends signed 16-bit little-endian PCM bytes to the WAV file.
func (w *WAVWriter) WritePCM16(pcm []byte) error {
	if w == nil || len(pcm) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.file == nil {
		return nil
	}
	n, err := w.file.Write(pcm)
	w.dataBytes += uint32(n)
	return err
}

// Close patches the RIFF and data chunk lengths and closes the file.
func (w *WAVWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.file == nil {
		return nil
	}
	w.closed = true
	header := makeWAVHeader(TargetSampleRate, TargetChannels, 16, w.dataBytes)
	var firstErr error
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		firstErr = fmt.Errorf("seek wav header: %w", err)
	} else if _, err := w.file.Write(header); err != nil {
		firstErr = fmt.Errorf("write wav header: %w", err)
	}
	if err := w.file.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("close wav: %w", err)
	}
	w.file = nil
	return firstErr
}

func makeWAVHeader(sampleRate, channels, bitsPerSample int, dataSize uint32) []byte {
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], 36+dataSize)
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(hdr[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(sampleRate))
	byteRate := uint32(sampleRate * channels * bitsPerSample / 8)
	binary.LittleEndian.PutUint32(hdr[28:32], byteRate)
	blockAlign := uint16(channels * bitsPerSample / 8)
	binary.LittleEndian.PutUint16(hdr[32:34], blockAlign)
	binary.LittleEndian.PutUint16(hdr[34:36], uint16(bitsPerSample))
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], dataSize)
	return hdr
}
