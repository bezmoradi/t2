//go:build darwin && cgo

package audio

/*
#cgo LDFLAGS: -framework AudioToolbox -framework CoreAudio -framework CoreFoundation
#include "aq_darwin.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

type audioQueue struct {
	stream *C.t2_stream
}

func OpenStream(sampleRate, frames int) (Stream, error) {
	var status C.int
	stream := C.t2_stream_open(C.int(sampleRate), C.int(frames), &status)
	if stream == nil {
		if status == C.T2_ERR_NOMEM {
			return nil, fmt.Errorf("opening the microphone: out of memory for %d-frame buffers", frames)
		}
		return nil, fmt.Errorf("opening the microphone: AudioQueue error %d", int(status))
	}
	return &audioQueue{stream: stream}, nil
}

func (q *audioQueue) Available() (int, error) {
	return int(C.t2_stream_available(q.stream)), nil
}

func (q *audioQueue) Read(into []int16) error {
	got := int(C.t2_stream_read(q.stream, (*C.int16_t)(unsafe.Pointer(&into[0])), C.int(len(into))))
	if got != len(into) {
		return fmt.Errorf("microphone read returned %d of %d frames", got, len(into))
	}
	return nil
}

func (q *audioQueue) Overflows() int {
	return int(C.t2_stream_overflows(q.stream))
}

func (q *audioQueue) Close() error {
	if st := C.t2_stream_close(q.stream); st != 0 {
		return fmt.Errorf("closing the microphone: AudioQueue error %d", int(st))
	}
	return nil
}
