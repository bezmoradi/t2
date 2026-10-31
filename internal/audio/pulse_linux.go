package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxBufferedSeconds = 4
	maxBufferedBytes   = SampleRate * BytesPerSample * maxBufferedSeconds
	minLatencyMillis   = 10
	pipeReadBytes      = 4096
	pactlTimeout       = 2 * time.Second
	parecordTermGrace  = time.Second
	stderrTailBytes    = 512
)

type parecord struct {
	cmd    *exec.Cmd
	frames int

	mu        sync.Mutex
	unreadLE  []byte
	overflows int
	exited    bool
	waitErr   error

	stderr stderrTail
	reaped chan struct{}
}

const noParecord = "audio capture needs parecord: install pulseaudio-utils (Debian, Ubuntu, Fedora) or libpulse (Arch); it works with PipeWire as well as PulseAudio"

func killedWithT2() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func installed(tool string) bool {
	_, err := exec.LookPath(tool)
	return err == nil
}

func isMonitor(source string) bool { return strings.HasSuffix(source, ".monitor") }

func OpenStream(sampleRate, frames int) (Stream, error) {
	path, err := exec.LookPath("parecord")
	if err != nil {
		return nil, errors.New(noParecord)
	}

	latencyMillis := max(frames*1000/sampleRate, minLatencyMillis)
	p := &parecord{frames: frames, reaped: make(chan struct{})}
	p.cmd = exec.Command(path,
		"--raw", "--format=s16le",
		fmt.Sprintf("--rate=%d", sampleRate), "--channels=1",
		fmt.Sprintf("--latency-msec=%d", latencyMillis),
		"--client-name=t2", "--stream-name=Dictation",
	)
	p.cmd.Stderr = &p.stderr
	p.cmd.SysProcAttr = killedWithT2()

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

func Limitation() string {
	if !installed("parecord") {
		return noParecord
	}
	if !installed("pactl") {
		return ""
	}

	out, err := pactl("list", "short", "sources")
	if err != nil {
		return fmt.Sprintf("cannot reach the sound server, so t2 cannot record (pactl: %v)", err)
	}
	var mics, monitors []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		source := fields[1]
		if isMonitor(source) {
			monitors = append(monitors, source)
		} else {
			mics = append(mics, source)
		}
	}
	if len(mics) == 0 {
		return fmt.Sprintf("no microphone found: the sound server has only monitors of outputs (%s), which record what the computer plays, not your voice. Connect a microphone", strings.Join(monitors, ", "))
	}

	if defaultSource, err := pactl("get-default-source"); err == nil && isMonitor(defaultSource) {
		return fmt.Sprintf("the default input is %s, a monitor of an output, which records what the computer plays, not your voice: choose a microphone in %s", defaultSource, SoundSettings())
	}
	return ""
}

func pactl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pactlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pactl", args...).Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("no answer within %v", pactlTimeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				firstLine := strings.SplitN(stderr, "\n", 2)[0]
				return "", errors.New(firstLine)
			}
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *parecord) pump(out io.Reader) {
	defer close(p.reaped)

	chunk := make([]byte, pipeReadBytes)
	for {
		n, err := out.Read(chunk)
		if n > 0 {
			p.mu.Lock()
			p.unreadLE = append(p.unreadLE, chunk[:n]...)
			if excess := len(p.unreadLE) - maxBufferedBytes; excess > 0 {
				excess += excess % BytesPerSample
				p.unreadLE = p.unreadLE[excess:]
				p.overflows++
			}
			p.mu.Unlock()
		}
		if err != nil {
			break
		}
	}

	waitErr := p.cmd.Wait()
	p.mu.Lock()
	p.exited = true
	p.waitErr = waitErr
	p.mu.Unlock()
}

func (p *parecord) Available() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	buffered := len(p.unreadLE) / BytesPerSample
	if p.exited && buffered < p.frames {
		msg := p.stderr.String()
		if msg == "" && p.waitErr != nil {
			msg = p.waitErr.Error()
		}
		if msg == "" {
			msg = "it exited"
		}
		return buffered, fmt.Errorf("parecord stopped: %s", msg)
	}
	return buffered, nil
}

func (p *parecord) Read(into []int16) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.unreadLE) < len(into)*BytesPerSample {
		return fmt.Errorf("microphone read: %d of %d frames buffered", len(p.unreadLE)/BytesPerSample, len(into))
	}
	for i := range into {
		into[i] = int16(binary.LittleEndian.Uint16(p.unreadLE[BytesPerSample*i:]))
	}
	p.unreadLE = p.unreadLE[len(into)*BytesPerSample:]
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
	case <-p.reaped:
	case <-time.After(parecordTermGrace):
		p.cmd.Process.Kill()
		<-p.reaped
	}
	return nil
}

type stderrTail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *stderrTail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if excess := len(t.buf) - stderrTailBytes; excess > 0 {
		t.buf = t.buf[excess:]
	}
	return len(b), nil
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(strings.Fields(strings.ReplaceAll(string(t.buf), "\n", " ; ")), " ")
}
