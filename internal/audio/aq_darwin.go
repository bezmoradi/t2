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

// OpenStream opens the default microphone through AudioQueue. A variable so
// tests can substitute a fake.
//
// This replaced PortAudio. PortAudio needed a Homebrew library, froze its
// device list at initialisation, could not interrupt a blocking read, and its
// CoreAudio backend deadlocked sporadically in Pa_StopStream (see
// Recorder.Stop), which was what made t2 go deaf every few hours of heavy
// use. AudioQueue is a system framework, follows the default microphone, and
// installs no listeners of its own on the audio unit.
var OpenStream = openAudioQueue

type audioQueue struct {
	s *C.t2_stream
}

func openAudioQueue(sampleRate, frames int) (Stream, error) {
	var status C.int
	s := C.t2_stream_open(C.int(sampleRate), C.int(frames), &status)
	if s == nil {
		if status == C.T2_ERR_NOMEM {
			return nil, fmt.Errorf("opening the microphone: out of memory for %d-frame buffers", frames)
		}
		return nil, fmt.Errorf("opening the microphone: AudioQueue error %d", int(status))
	}
	return &audioQueue{s: s}, nil
}

func (q *audioQueue) Available() (int, error) {
	return int(C.t2_stream_available(q.s)), nil
}

func (q *audioQueue) Read(into []int16) error {
	got := int(C.t2_stream_read(q.s, (*C.int16_t)(unsafe.Pointer(&into[0])), C.int(len(into))))
	if got != len(into) {
		return fmt.Errorf("microphone read returned %d of %d frames", got, len(into))
	}
	return nil
}

func (q *audioQueue) Overflows() int {
	return int(C.t2_stream_overflows(q.s))
}

func (q *audioQueue) Close() error {
	if st := C.t2_stream_close(q.s); st != 0 {
		return fmt.Errorf("closing the microphone: AudioQueue error %d", int(st))
	}
	return nil
}
