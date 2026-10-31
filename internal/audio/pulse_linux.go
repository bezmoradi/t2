package audio

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

var OpenStream = openParecord

// maxBuffered caps the audio held between the reader goroutine and the
// capture loop. The loop drains it every few milliseconds, so this is only
// reached if the loop stalls; past it the oldest audio is dropped and
// counted as an overflow, as the device ring buffer does on macOS.
const maxBuffered = SampleRate * 2 * 4 // 4s of mono int16

// parecord captures through PulseAudio's client tool, which PipeWire's
// pipewire-pulse serves as well. Running it as a process keeps the Linux
// build free of cgo and of libpulse's headers, and makes the stop the
// AudioQueue watchdog exists for impossible to hang: Close kills a process.
//
// The process lives only for one recording, so the microphone - and the
// desktop's microphone-in-use indicator - is on only while the keys are held.
type parecord struct {
	cmd    *exec.Cmd
	frames int

	mu        sync.Mutex
	buf       []byte // captured, not yet read: s16le
	overflows int
	exited    bool
	waitErr   error

	stderr tail
	done   chan struct{} // closed once the process has exited and been reaped
}

func openParecord(sampleRate, frames int) (Stream, error) {
	path, err := exec.LookPath("parecord")
	if err != nil {
		return nil, errors.New("audio capture needs parecord: install pulseaudio-utils (Debian, Ubuntu, Fedora) or libpulse (Arch); it works with PipeWire as well as PulseAudio")
	}

	latency := max(frames*1000/sampleRate, 10)
	p := &parecord{frames: frames, done: make(chan struct{})}
	p.cmd = exec.Command(path,
		"--raw", "--format=s16le",
		fmt.Sprintf("--rate=%d", sampleRate), "--channels=1",
		fmt.Sprintf("--latency-msec=%d", latency),
		"--client-name=t2", "--stream-name=Dictation",
	)
	p.cmd.Stderr = &p.stderr
	// If t2 dies without closing the stream, the microphone must not stay
	// open behind it.
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	out, err := p.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opening the microphone: %v", err)
	}
	if err := p.cmd.Start(); err != nil {
		return nil, fmt.Errorf("opening the microphone: %v", err)
	}
	go p.pump(out)
	return p, nil
}

// pump moves audio from the process into buf until the process ends.
func (p *parecord) pump(out io.Reader) {
	defer close(p.done)

	chunk := make([]byte, 4096)
	for {
		n, err := out.Read(chunk)
		if n > 0 {
			p.mu.Lock()
			p.buf = append(p.buf, chunk[:n]...)
			if over := len(p.buf) - maxBuffered; over > 0 {
				over += over % 2 // drop whole samples
				p.buf = p.buf[over:]
				p.overflows++
			}
			p.mu.Unlock()
		}
		if err != nil {
			break
		}
	}

	werr := p.cmd.Wait()
	p.mu.Lock()
	p.exited = true
	p.waitErr = werr
	p.mu.Unlock()
}

func (p *parecord) Available() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.buf) / 2
	if p.exited && n < p.frames {
		// parecord only exits on its own when it cannot record: no sound
		// server, no source, the server going away.
		msg := p.stderr.String()
		if msg == "" && p.waitErr != nil {
			msg = p.waitErr.Error()
		}
		if msg == "" {
			msg = "it exited"
		}
		return n, fmt.Errorf("parecord stopped: %s", msg)
	}
	return n, nil
}

func (p *parecord) Read(into []int16) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.buf) < len(into)*2 {
		return fmt.Errorf("microphone read: %d of %d frames buffered", len(p.buf)/2, len(into))
	}
	for i := range into {
		into[i] = int16(uint16(p.buf[2*i]) | uint16(p.buf[2*i+1])<<8)
	}
	p.buf = p.buf[len(into)*2:]
	return nil
}

func (p *parecord) Overflows() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.overflows
}

func (p *parecord) Close() error {
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
	return nil
}

// tail keeps the last few hundred bytes written to it, on one line:
// parecord's error message, without letting a chatty process grow it.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - 512; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(strings.Fields(strings.ReplaceAll(string(t.buf), "\n", " ; ")), " ")
}
