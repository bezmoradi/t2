package audio

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

const (
	wavHeaderSize      = 44
	riffChunkIDAndSize = 8
	pcmFormatChunkSize = 16
	pcmFormat          = 1
	wavChannels        = 1
	wavBitsPerSample   = BytesPerSample * 8
	wavBytesPerFrame   = wavChannels * BytesPerSample
	wavBytesPerSecond  = SampleRate * wavBytesPerFrame
)

func WriteWAV(path string, pcm []byte) error {
	le := binary.LittleEndian
	header := make([]byte, 0, wavHeaderSize)
	put32 := func(v uint32) { header = le.AppendUint32(header, v) }
	put16 := func(v uint16) { header = le.AppendUint16(header, v) }

	header = append(header, "RIFF"...)
	put32(uint32(wavHeaderSize - riffChunkIDAndSize + len(pcm)))
	header = append(header, "WAVE"...)

	header = append(header, "fmt "...)
	put32(pcmFormatChunkSize)
	put16(pcmFormat)
	put16(wavChannels)
	put32(SampleRate)
	put32(wavBytesPerSecond)
	put16(wavBytesPerFrame)
	put16(wavBitsPerSample)

	header = append(header, "data"...)
	put32(uint32(len(pcm)))

	return replaceWithPrivateFile(path, header, pcm)
}

func replaceWithPrivateFile(path string, parts ...[]byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()

	for _, part := range parts {
		if _, err := f.Write(part); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("saving %s: %w", path, err)
	}
	return nil
}
