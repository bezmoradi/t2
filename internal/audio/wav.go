package audio

import (
	"encoding/binary"
	"fmt"
	"os"
)

// WriteWAV writes mono 16-bit PCM at SampleRate to path.
//
// This exists so that no failure anywhere in the pipeline - a dial that timed
// out, a connection that dropped mid-sentence, a paste that would not go
// through - costs the user audio they already spoke. Whatever was captured
// lands on disk and the path is printed.
func WriteWAV(path string, pcm []byte) error {
	const (
		channels      = 1
		bitsPerSample = 16
	)

	byteRate := SampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()

	le := binary.LittleEndian
	header := make([]byte, 0, 44)
	put32 := func(v uint32) { header = le.AppendUint32(header, v) }
	put16 := func(v uint16) { header = le.AppendUint16(header, v) }

	header = append(header, "RIFF"...)
	put32(uint32(36 + len(pcm)))
	header = append(header, "WAVE"...)

	header = append(header, "fmt "...)
	put32(16) // PCM chunk size
	put16(1)  // PCM format
	put16(channels)
	put32(SampleRate)
	put32(uint32(byteRate))
	put16(uint16(blockAlign))
	put16(bitsPerSample)

	header = append(header, "data"...)
	put32(uint32(len(pcm)))

	if _, err := f.Write(header); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := f.Write(pcm); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return f.Close()
}
