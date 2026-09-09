# T2 — End-to-End Review

Reviewed 2026-09-09 against the working tree (uncommitted changes on top of `8d781a1`).

What I did: read all ~1,530 production lines and ~310 test lines; built and ran the suite
(`go build`, `go vet`, `staticcheck`, `go test ./... -race` are all clean on go1.26.0
darwin/arm64); wrote three throwaway probe tests against the processor (Appendix A); measured
two latencies on this machine; and checked the AssemblyAI streaming docs and pricing page as
of today for every protocol and billing fact a finding depends on. I did **not** run a real
recording.

Severity scale:

- **P0** — loses recordings or money in routine daily use.
- **P1** — loses data under conditions you will hit some weeks.
- **P2** — wrong or misleading, but rare or recoverable.
- **P3** — hygiene.

Line numbers refer to the working tree. `B` = bug, `S` = design smell I'd change anyway.

---

## 1. The short version

Five things I would fix before dictating with this every day, in order:

1. **Capture starts roughly half a second after key-down, and the quick-press filter is
   measured from capture start, not from key-down.** One-second holds are discarded as
   "quick press", and every recording loses its first syllable. (B1)
2. **A partial that arrives after a final replaces the whole transcript.** A two-sentence
   dictation loses the first sentence whenever the server takes more than a second to format
   the last turn. Verified with a probe. (B2)
3. **AssemblyAI bills wall-clock session time, and this tool leaves sessions open**: from
   startup until the first press, and after every quick-press or silent recording until the
   server's 3-hour cap. (B3)
4. **A late `Termination` from one recording ends the next recording early**, and late
   `Turn`s land in the wrong recording. §5.2 is real and worse than described. Verified with a
   probe. (B4)
5. **Failures inside a recording are reported as success.** An input overflow, a dropped
   connection, or a mic that failed to open all end with a truncated or empty result and a ✅.
   (B6, B7, B8)

Plus one thing to check today: the code does not pin `speech_model`, and the current API
reference lists a default that the pricing page bills at three times the rate you probably
think you're paying. (B5)

The structural change that removes most of the concurrency findings at once is small, not
large: make one press equal one `Session` value that owns its socket, its reader goroutine,
and its transcript state (S1). The shared `wsConn` pointer, the `terminated` flag, the
identity check, the health counters, and the cross-session contamination all exist because
`Client` is one object pretending to be every connection in turn.

---

## 2. Bugs, severity-ranked

### P0

#### B1 — Capture starts ~0.4–0.6 s after key-down; short holds are discarded as "quick press"

**Where:** `internal/app/daemon.go:129-147` (dial, 150 ms sleep, beep, *then* `pressTime`,
*then* `recorder.Start()`); `daemon.go:159-169` (duration measured from `pressTime`);
`internal/hotkeys/simple.go:125` (edges carry no timestamp).

**Measured on this machine:** TLS handshake to `streaming.assemblyai.com` 60–140 ms (plus the
HTTP upgrade round trip on top), `osascript` spawn 93–105 ms (that is what `beeep.Beep` does
on macOS, see B16), the hardcoded sleep 150 ms, poll tick 0–100 ms, PortAudio open/start
unmeasured (tens of ms). Between key-down and the first captured sample: roughly 400–600 ms.

**Failure scenarios:**

- Hold Ctrl+Shift for 1.0 s and say "yes". Capture starts at ~0.5 s. `recordingDuration` ≈
  0.5 s < 800 ms → "⚡ Quick press detected - skipped". The recording is discarded and the
  message blames you. The effective minimum hold is ~1.3 s, not 0.8 s.
- Every recording: if you start speaking as you press, which is what people do, the first
  syllable is never captured. "Send the report" arrives as "end the report".
- Press again while the previous `OnRelease` is still running (it takes 1.5–2.5 s: up to 1 s
  termination wait + 200 ms clipboard sleep + `osascript` paste + two beeps). Your press and
  release edges queue in `events`. When `OnPress` finally runs, `waitForRelease` returns
  immediately from the queued release, duration ≈ 0 → skipped. A real 2-second dictation is
  lost with no indication that anything was said.

**Fix:**

- Timestamp edges in the poller: `type edge struct { pressed bool; at time.Time }`. Compute
  hold duration from edge times, never from `time.Now()` inside the handlers.
- Start capture **first**, before any network call or beep, into an in-memory buffer (a
  60-second recording is under 2 MB). Dial concurrently. When `Begin` arrives, flush the
  buffer and stream live from there. The user never loses the first word, and dial latency
  becomes invisible instead of merely shorter (this is my answer to §5.4).
- Move paste and beeps off the listen goroutine (S4).

#### B2 — A partial arriving after a final replaces the entire transcript

**Where:** `internal/transcription/processor.go:34` (every partial overwrites
`currentTranscript`); `processor.go:108` (consume returns `currentTranscript` whenever any
final exists).

**Verified (Appendix A, probe 1):** final(turn 0) = "First sentence.", then partial(turn 1) =
"Second sentence", then consume → returns `"Second sentence "` with `isFinal == true`.

**Failure scenarios:**

- Say "First sentence. Second sentence." and release. Turn 0's formatted final arrived while
  you were still talking; turn 1's partials then overwrote `currentTranscript`. The server
  takes more than 1 s after `Terminate` to format turn 1 (or the network hiccups), the 1 s
  timeout fires, consume returns "Second sentence ". "First sentence." is gone and the terminal
  prints ✅. This is the ordinary case for any dictation with a pause in it, on any day the
  server is slow.
- After the last final, the server emits a partial for a *new* turn from trailing noise (ASR
  models are prone to "you" / "thank you" on silence). That partial replaces the entire
  transcript with "you".
- Fallback path, no finals at all (probe 2): partial(0) = "Hello there my good friend",
  partial(1) = "Yes" → consume returns only turn 0. `len(transcript) > len(best)` keeps the
  longer *earlier* turn and drops the later one.

**Fix:** partials must never touch the finals aggregate. Keep `partials map[int]string`
(latest per turn; v3 partials are cumulative within a turn, so latest is best) and
`finals map[int]string`. At consume, walk turn orders ascending and take `finals[t]` if
present, else `partials[t]`. That single change fixes both probes and makes the heuristic at
`processor.go:36` unnecessary (see S6). Add probe 1 as a regression test; the existing
processor tests never exercise "final, then partial, then consume".

#### B3 — Sessions outlive recordings, and AssemblyAI bills wall-clock session time

**Where:** `daemon.go:63` (connect at startup, idle until the first press);
`daemon.go:165-179` (quick-press and silence paths return without `Terminate`);
`internal/transcription/assemblyai.go:172` (`IsConnected` reuses any un-terminated
connection); `assemblyai.go:240-243` (the client never closes its side after `Termination`);
`assemblyai.go:70-71` (`Connect` overwrites `wsConn` without closing the old one).

**Facts (AssemblyAI docs, checked today, Appendix A):** Universal-Streaming is billed on "the
total duration a WebSocket connection remains open, rather than the amount of audio
processed. Idle time is billed at the same rate as active audio streaming." Sessions that are
not closed auto-close after 3 hours "and are billed for the full duration". The docs' own
guidance is "close connections immediately when calls end".

**Failure scenarios:**

- Launch `t2` with your terminal at 09:00, first dictation at 10:30 → 90 minutes billed for
  zero audio.
- The last thing you do in the evening is Ctrl+Shift+T in the browser. Quick-press path, no
  `Terminate`, session stays open, auto-closed at the 3-hour cap, 3 hours billed. **Every**
  skip path leaves a live session behind, so "every press now reconnects" (§4.1) is only true
  after a *successful* recording.
- A silence-skipped session is reused for the next real recording: the new session starts
  with N seconds of room noise already streamed, and its `turn_order` continues from whatever
  the noise produced. Any late `Turn` from that noise lands in the next recording (feeds B4).

**Cost bound:** 3 h × $0.15 = $0.45 per leaked session at the Universal-Streaming English
rate, or $1.35 at the Pro rate (B5). A few dozen recordings a day is a few dozen chances a
day to leak one.

**Fix:** adopt one policy: **a session never exists outside a recording.**

- Don't connect in `Initialize`. If you want a key check at startup, connect, wait for
  `Begin`, send `Terminate`, wait for `Termination`, close.
- `OnRelease`: always send `Terminate`, including on skip paths (no need to wait for
  `Termination` on those). After `Termination` or the timeout, `Close()` the socket yourself so
  the reader exits and the session is provably over.
- `Connect`: if `wsConn != nil`, close it first.
- Belt and braces: add `inactivity_timeout=30` to the URL (documented today: seconds, range
  5–3600, "no timeout if not set"). Then anything `t2` ever leaks is killed server-side.
- Log `audio_duration_seconds` vs `session_duration_seconds` from every `Termination` (S10),
  so you can see exactly what you pay for per recording.

#### B4 — Cross-session contamination (§5.2) confirmed, plus a worse variant: a stale `Termination` ends the next recording early

**Where:** `processor.go:89-94` (`SignalTermination`, called from whichever reader goroutine
happens to receive it); `processor.go:70-73` (`Reset` drains the signal, but only at press
time); `daemon.go:185-188` (the wait); `assemblyai.go:236` and `:242` (a reader for an *old*
connection keeps firing callbacks into the one shared processor).

**Verified (Appendix A, probe 3):** `Reset()`, then `SignalTermination()`, then
`<-WaitForTermination()` returns immediately.

**Interleaving:**

1. Recording N: `Terminate` sent; server is slow; the 1 s timeout fires; consume the partial;
   `Reset`.
2. Press N+1: `OnPress` → `Reset` (nothing to drain yet) → dial → recording.
3. Session N's reader is still alive (the server hasn't hung up yet). Its final `Turn` arrives
   and lands in N+1's `finalTranscripts`. Its `Termination` arrives and `sessionTerminated`
   now holds a stale `true`.
4. Release N+1: `Terminate` → the `select` returns **immediately** on the stale signal →
   consume runs before N+1's final exists → N+1 pastes its best partial (or N's leaked final
   joined with N+1's partial). N+1's real final then arrives during N+2. Cascade.

The identity check in `handleResponses` (`assemblyai.go:183`) protects the *pointer*. It does
nothing for the *callbacks*.

**Fix (recommended):** make the session a value (S1). `Connect` returns a
`*Session{conn, done chan, its own transcript state, result}`; the reader goroutine only ever
writes to *its own* session; the daemon holds `current *Session`. No generation counter, no
shared `wsConn`, no `terminated` flag, no identity check, and a stale reader can only mutate a
dead session nobody will read. This is *less* code than today's client.

If you want the minimal patch instead: a `session uint64` generation, captured at press,
stamped onto the callbacks by a per-connection closure, and checked **under
`transcriptMutex`** in `ProcessTranscript` and `SignalTermination`. Tearing the reader down
before consuming is not sufficient on its own: the callback can already be inside the mutex
when you close the socket.

#### B5 — `speech_model` is not pinned; today's documented default is the expensive one

**Where:** `assemblyai.go:55-58` (only `sample_rate` and `format_turns` are set).

**Facts (checked today):** the API reference lists `speech_model` default
`universal-3-5-pro`; the pricing page lists Universal-3.5 Pro Realtime at **$0.45/hr** and
Universal-Streaming English at **$0.15/hr**. I cannot see which one your sessions actually
land on. The `Begin` message echoes the applied `configuration`, including the model, and the
code drops `Begin` on the floor (`assemblyai.go:212`).

**Fix:** log `Begin.configuration` once (S10), check your usage page, and set
`speech_model=universal-streaming-english` explicitly (or whatever the cheapest model that
meets your accuracy bar is) so a server-side default change can never silently triple your
bill. While there, set `encoding=pcm_s16le` explicitly too; relying on the default for the
wire format is the kind of thing that breaks on a docs update.

### P1

#### B6 — PortAudio input overflow is treated as fatal; capture dies silently and the tool still says ✅

**Where:** `internal/audio/recorder.go:188-204` (any `Read()` error → log → `return`), which
leaves `r.recording == true`.

**Fact:** `Pa_ReadStream` returns `paInputOverflowed` *as a status alongside valid data* ("if
input data was discarded by PortAudio after the previous call and before this call",
`portaudio.h`). The Go binding's `newError` turns every non-zero status into an error, so
`portaudio.InputOverflowed` arrives as `err != nil` with a full, valid buffer.

**Failure scenario:** anything stalls the loop longer than the ring buffer's headroom (a few
hundred milliseconds at 16 kHz with 1024-frame buffers and default latency): a `SendAudio`
that blocks (B7, §5.1), or the process simply not being scheduled for a while. The next
`Read()` returns `InputOverflowed`. The goroutine logs "Error reading from stream: Input
overflowed" to stderr and exits. You keep talking into a dead capture; nothing beeps. On
release, `Stop()` succeeds, the RMS gate passes on what was captured, `Terminate` is sent, and
a **truncated transcript is pasted with "✅ Pasted to active application"**.

**Fix:** `if errors.Is(err, portaudio.InputOverflowed) { overflows++ }` and keep the data.
On every other exit path, record the reason in the recorder and have `Stop()` return it, so
`OnRelease` can print "recording stopped early after 3.2 s: …" and not print ✅. Also delete
the 10 ms sleep at `recorder.go:290` (S7): `Read()` already blocks until a buffer is full; the
sleep only burns headroom.

#### B7 — Connection loss mid-recording: a log line every 74 ms, then a fragment pasted as success

**Where:** `assemblyai.go:96-98` (nil connection → a plain error string);
`assemblyai.go:105-107` and `recorder.go:276-278` (two different string-matching lists in two
packages, and neither includes `broken pipe`, the usual macOS error when writing to a
peer-closed socket, nor `i/o timeout` from the 5 s write deadline); `recorder.go:282` (log per
chunk); `daemon.go:182-203` (`Terminate` on a nil connection returns nil, the 1 s wait runs
for nothing, the partial is pasted with ✅).

**Failure scenario:** Wi-Fi drops 4 s into a 20 s dictation. Writes fail with `write: broken
pipe`: matched by nothing, so logged ~13 times a second. The reader notices and nils
`wsConn`; now every chunk logs "WebSocket connection not established". Release: `Terminate`
no-ops, 1 s wait, the best partial from the first 4 s is pasted, ✅. Sixteen seconds of speech
are gone and the terminal scrolled 200 lines.

**Fix:** `SendAudio` returns sentinel errors (`ErrNotConnected`, `ErrConnectionClosed`),
classified once with `errors.Is` against `net.ErrClosed`, `syscall.EPIPE`,
`syscall.ECONNRESET`, `os.ErrDeadlineExceeded`, and `websocket.IsCloseError`. The recorder
stops *sending* on those but keeps *capturing* into the local buffer (B1, S9). `OnRelease`
reports "connection lost after 4.1 s; 20 s of audio saved to …" instead of ✅. Never print ✅
when the text came from a partial or the capture ended early; print what actually happened.

#### B8 — `recorder.Start()` failure is ignored: start beep, no recording, silent release

**Where:** `daemon.go:147` (return value discarded); `daemon.go:154-156` (`OnRelease` returns
silently when not recording).

**Failure scenario:** another app holds the mic exclusively, the input device just
disconnected, or Microphone permission was denied for this terminal. `OpenDefaultStream`
fails; `Start` logs to stderr and returns an error nobody reads; the start beep already
played; you dictate; release → nothing: no stop beep, no message. A session was also dialed
and left open (B3).

**Fix:** check the error, print it, play an error sound, terminate the session.

#### B9 — The silence gate is an absolute number; a quieter mic makes every recording "No speech detected"

**Where:** `recorder.go:21` (`SilenceThreshold = 150.0`, about −47 dBFS); `daemon.go:173`.

**Failure scenarios:**

- Switch from the MacBook mic to AirPods or a USB mic with lower gain, or lower the input
  level in System Settings. Peak RMS of normal speech drops under 150 → every recording is
  skipped, no transcription is attempted, nothing tells you why.
- The reverse: the start beep plays through the speakers just before capture opens
  (`osascript -e beep` returns when the script finishes, not when the sound does). If the mic
  hears the tail of it, max RMS is set by the beep and the gate can never fire. Worth
  confirming with one log line; it would explain any "why did it transcribe silence" moments.

**Fix:** print the measured level when skipping ("max level 92, threshold 150"); make the gate
relative (max chunk RMS > 4× the quietest chunk, i.e. dynamic range) or at least configurable;
ignore the first ~200 ms when computing the max.

#### B10 — A press on a dead network blocks everything for up to 45 seconds

**Where:** `assemblyai.go:65` (`websocket.DefaultDialer`, whose `HandshakeTimeout` is 45 s,
with no connect timeout); `daemon.go:130`.

**Failure scenario:** the laptop wakes from sleep before Wi-Fi is up (black hole, no RST).
Press → dial hangs → no beep, no message. Every edge you generate meanwhile queues. After the
timeout: "❌ Connection failed", then the queued press/release pairs replay as zero-length
quick presses (beep, beep, ⚡, beep, beep, ⚡).

**Fix:** a dialer with `HandshakeTimeout: 5 * time.Second` and
`NetDialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext`; include
`resp.StatusCode` in the error (a 401 and a timeout are different problems). With B1's
capture-first design, a dial failure becomes "saved your 12 s of audio to …" rather than a
lost recording.

### P2

#### B11 — The version gate refuses to run in three legitimate situations

**Where:** `cmd/t2/main.go:15-23` (the gate runs before `flag.Parse()` at `:30`);
`internal/version/checker.go:41-43` (`VERSION != newVersion`).

**Failure scenarios:**

- You bump `version.go` to v1.0.8 locally before pushing (your git log shows exactly this
  workflow: "bump version to v1.0.6 for testing"). `main` still says v1.0.7. `t2` refuses to
  start: "The newest version of T2 is v1.0.7 but the installed version on your system is
  v1.0.8".
- `t2 --version` performs an HTTPS GET and, if `main` has moved on, prints the update nag and
  exits 0 without printing the version. Same for `--show-config` and `--reset-key`.
- GitHub slow: up to 5 s added to every launch. Bounded now, but still on the startup path.

**Fix:** parse flags first; never block startup on this; run the check in a goroutine and
print one line if remote is *newer* than local (`golang.org/x/mod/semver.Compare`); consider
the GitHub releases API instead of parsing Go source off `main`, so the nag names a tag that
exists (`go install …@v1.0.8` fails if you pushed the bump before tagging).

#### B12 — Stale reader and socket leak when the server doesn't hang up

**Where:** `assemblyai.go:70-71` (`Connect` overwrites without closing);
`assemblyai.go:240-243` (no close after `Termination`); `assemblyai.go:190` (`ReadMessage`
with no read deadline and no keepalive).

**Failure scenario:** the `Terminate` write fails, or a NAT silently drops the mapping after a
Wi-Fi roam. The reader blocks in `ReadMessage` forever. The next press dials a new connection;
the old goroutine, file descriptor, and **billed session** live on until TCP gives up or the
3-hour cap. One per press on a bad afternoon.

**Fix:** same as B3: close after `Termination` or timeout; close the old connection in
`Connect`; optionally `SetReadDeadline` while waiting for `Termination`.

#### B13 — `.env` from the *current directory* can silently select another project's key

**Where:** `internal/config/config.go:141-145` (`godotenv.Load()` with no path reads
`./.env`).

**Failure scenario:** `cd ~/work/client-repo && t2`. That repo's `.env` has an
`ASSEMBLYAI_API_KEY` for a client account. Your dictation is billed to the client. Also,
`godotenv.Load` exports *every* variable in that file into `t2`'s environment.

**Fix:** drop `.env` support (env var + config file is enough for one user), or load only
`~/.config/t2/.env`.

#### B14 — Errors are swallowed where they matter most

**Where:** `assemblyai.go:191-201` (all three read-error returns discard the error, including
the server's close code and reason); `:205-207` (JSON decode failures `continue`); `:245-248`
(unknown message types dropped, which is where a server error message would land); `:65-67`
(the dial error discards the HTTP response).

**Failure scenario:** AssemblyAI closes with 3007 (chunk duration violation), 3008 (session
limit), or an auth/credit problem mid-day. You see "❌ No transcription received" and nothing
else. A bad key at startup says "websocket: bad handshake" with no status code.

**Fix:** on abnormal close, print `*websocket.CloseError` code and text; under `-v` log
unknown message types with their body; include `resp.StatusCode` (and body) in the dial error.

### P3

#### B15 — Ctrl+C mid-recording exits without waiting for the listener; `Pa_Terminate` can race `Pa_OpenStream`

**Where:** `daemon.go:85` (`Listen` is never waited on); `daemon.go:88-91`; `daemon.go:94-112`.

Ctrl+C while `OnRelease` is in the 1 s wait or the paste: `Cleanup` closes the socket under
it, terminates PortAudio, `Run` returns, the process exits mid-paste. Ctrl+C while `OnPress`
is inside `OpenDefaultStream`: `Pa_Terminate` runs concurrently with `Pa_OpenStream` in C.
Low impact since you're quitting, but it can print a crash instead of "Shutting down".

**Fix:** stop the hotkey manager, wait for `Listen` to return (a `done` channel), then stop
recorder, client, PortAudio.

#### B16 — Beeps: identical on macOS, ~100 ms each, and the fallback re-runs the thing that just failed

**Where:** `internal/audio/beep.go:15` and `:22`. On darwin, `beeep.Beep` ignores both
arguments and runs `osascript -e beep` synchronously (checked in v0.11.1's `beep_darwin.go`).
`:18` and `:25` fall back to running `osascript` again.

**Measured:** 93–105 ms per `osascript` spawn. That's ~200 ms of blocking per recording and
~200 ms per accidental Ctrl+Shift chord, plus two identical sounds where the comment promises
"a different tone for stop".

**Fix:** `exec.Command("afplay", "/System/Library/Sounds/Tink.aiff").Start()` (not `Run`) for
start and a different file for stop, or `NSSound` via the cgo you already have. Delete the
fallback.

#### B17 — `--reset-key` says you'll be prompted; you won't be if the key comes from the environment

**Where:** `main.go:87`; `config.go:136-145`. The env var and `.env` are consulted before the
prompt, so after a reset the prompt never appears if either is set.

#### B18 — The non-darwin branch is unreachable

**Where:** `simple.go:133-139`. The cgo preamble includes CoreGraphics and Carbon headers, so
this file cannot compile anywhere else; the `runtime.GOOS` branch never runs. §5.7's "fail
loudly at startup" is moot because the build already fails. For a clean failure, add
`//go:build darwin` here and a `simple_other.go` whose `Start()` returns an error.

---

## 3. Design smells I'd change anyway

#### S1 — `Client` is a connection manager pretending to be one connection

`wsConn` + `terminated` + the reader's identity check + the health counters are all
compensation for one object being reused across connections. One press is one session, so
model it that way:

```go
type Session struct {
    conn    *websocket.Conn
    done    chan struct{}          // closed when the reader exits
    mu      sync.Mutex
    finals  map[int]string
    partial map[int]string
    term    chan struct{}          // closed on Termination
    err     error                  // why the reader exited, if abnormal
}

func Dial(ctx context.Context, key string) (*Session, error)   // returns after Begin
func (s *Session) Send(pcm []byte) error
func (s *Session) Terminate(ctx context.Context) (Transcript, error) // Terminate, wait, Close
```

The daemon holds `current *Session`. `wsMutex` stops doing three jobs (§6): connection
identity is the value, writes are serialized by a single sender goroutine, and there are no
health counters. B4 and B12 disappear by construction.

#### S2 — The connection-health heuristic is provably dead (§5.5)

`Connect` resets `connectionHealth` to 100 and `failedSessions` to 0 on every press
(`assemblyai.go:76-77`). `OnRelease` can move them at most one step (`daemon.go:202`, `:207`),
so `failedSessions` oscillates 0 → 1 → 0 and `connectionHealth` never drops below 85.
`ConnectionNeedsRefresh` (`assemblyai.go:253-274`) can therefore only return true after three
consecutive *dial* failures, and its only effect then is a 100 ms sleep before the next dial
(`daemon.go:123-126`). Delete `connectionHealth`, `failedSessions`, `lastConnectionTime`,
`ConnectionNeedsRefresh`, `ReportSessionSuccess`, `ReportSessionFailure`, `connectionCallback`
and the no-op `handleConnection` (`daemon.go:220-223`).

#### S3 — The in-recording silence machinery optimizes for a billing model AssemblyAI doesn't use

`speechState`, `silenceChunks`, `prolongedSilence` and `shouldSendAudio`
(`recorder.go:45-47`, `:96-98`, `:225-259`) withhold audio during initial silence to "avoid
unnecessary API calls". Billing is per session-second, not per audio-second (B3), so
withholding audio saves nothing and only makes the stream discontinuous. Keep the *end*
gate (max RMS, `daemon.go:173`): it saves the round-trip wait and avoids pasting hallucinated
noise words. Delete the send-suppression.

#### S4 — The listen goroutine does everything, synchronously

Dial, two ~100 ms beeps, the 150 ms sleep, the 1 s termination wait, the 200 ms clipboard
sleep and the `osascript` paste all run on the goroutine that has to notice the next press.
That is the mechanism behind B1's third scenario. Shape I'd use: the listen goroutine only
starts/stops capture and hands a finished recording to a worker (`chan *recording`, buffer
of a few); the worker does terminate/wait/consume/paste. Beeps via `Start()` not `Run()`.

#### S5 — Layers talk to each other through error strings

`recorder.go:276-278` inspects WebSocket error text; `assemblyai.go:105-107` inspects
`net` error text; the lists differ and both miss `broken pipe`. Sentinel errors from
`SendAudio` and `errors.Is` in exactly one place (B7).

#### S6 — The partial-fallback heuristic is a category error

`end_of_turn_confidence` (`assemblyai.go:223`, used at `processor.go:36`) is the
probability that the *turn has ended*, not transcript quality. `len(transcript) > len(best)`
is a proxy for "latest" that fails across turns (probe 2). Per-turn latest partial (B2's fix)
removes the heuristic entirely.

#### S7 — Capture loop details

- `time.Sleep(10ms)` at `recorder.go:290`: `Read()` blocks until a buffer is full; the sleep
  only adds latency and eats overflow headroom. Remove.
- Open the stream with `[]int16` (`recorder.go:104`, `:108`); the binding supports it and it
  deletes the `>> 16` conversion at `:212`. Two allocations per chunk (`:207-208`) then become
  one `binary.LittleEndian` copy, or zero with `unsafe.Slice`.
- The chunk in hand when `stopChan` closes is discarded (`:249-253`), and whatever sits unread
  in PortAudio's ring buffer goes with the stream close. Roughly 64–130 ms of tail is never
  sent, so releasing right on the last word can clip its final consonant. Flush everything
  captured before `Terminate`.
- The deferred `recover()` at `:161-166` shadows the receiver and, more importantly, swallows
  a panic in the callback while leaving `recording == true` (same shape as B6). Record the
  reason; don't silently recover.

#### S8 — Accidental-trigger suppression without changing the chord (§5.6)

Two cheap checks, same API family and permission footprint as the existing
`CGEventSourceFlagsState` call:

1. **Modifier exclusivity.** Reject a press edge if `kCGEventFlagMaskCommand` or
   `kCGEventFlagMaskAlternate` is also set. Today Cmd+Ctrl+Shift+4 (screenshot to clipboard)
   starts a recording *and* then races the screenshot for the clipboard.
2. **Non-modifier abort.** Each poll tick during a hold, scan
   `CGEventSourceKeyState(kCGEventSourceStateHIDSystemState, k)` for keycodes 0–127. If any
   key is down, cancel the hold silently: Ctrl+Shift+T, Ctrl+Shift+Tab and Ctrl+Shift+arrow
   never beep, never dial, never bill. Dictation is defined as "Ctrl+Shift alone, with nothing
   else pressed for the duration".

With (2) the 800 ms filter can shrink to ~250 ms or go away, which also fixes B1's first
scenario from the other side.

#### S9 — Keep the whole recording in memory; write a WAV on any failure

Under 2 MB for a minute of audio. On any failure (dial, mid-stream drop, no final, paste
failure) write `~/.config/t2/last-recording.wav` and print the path. Every other finding in
this review then degrades to "annoying" instead of "lost". It also enables an optional
fallback to the pre-recorded (async) API for the buffered audio when streaming fails.

#### S10 — A `-v` flag (§5.8)

What I'd log per recording under `-v`: `Begin.id` and `Begin.configuration` (model, that
is your bill); press→capture-start latency; dial→`Begin` latency (replaces the 150 ms
guess); chunks sent, overflows, dropped; `Terminate`→`Termination` latency (replaces the 1 s
guess, see §5.3); `audio_duration_seconds` vs `session_duration_seconds` (cost); max RMS;
whether the pasted text came from a final, a partial, or a truncated capture; and every
abnormal close code. Without this you're diagnosing by attaching a harness, as you found.

#### S11 — Version check design

Parsing Go source off `main` and comparing with `!=` is fragile (B11). Async, notify-only,
semver comparison, and a source that names tags.

#### S12 — Small things

- Every paste gets a trailing space (`processor.go:108`, `:110`). Fine for prose, wrong for a
  search box or a code editor. At least make it deliberate in a comment or a flag.
- `map[string]any` decoding (`assemblyai.go:204`) → a typed `turnMessage` struct with
  `json` tags; catches schema drift at compile time and documents the fields you rely on.
- `WaitForTermination()` returns a bidirectional `chan bool` (`processor.go:85`); make it
  `<-chan struct{}`.
- `log.Printf` (stderr, timestamps) and `fmt.Println` (stdout, emoji) are mixed. Pick one
  writer; `log.SetFlags(0)` if you keep `log`.
- `handleShowConfig` prints the file including the API key to the terminal (`main.go:78`).
  Mask it.

---

## 4. Pushback on the recorded decisions

### 4.1 The §4 fixes

**4.1 Dead WebSocket / reconnect every press.** Right direction, incomplete. (a) The invariant
"every press reconnects" is false on both skip paths (B3). (b) `Connect` doesn't close the
connection it replaces, and nothing closes after `Termination` (B12). (c) The identity check
is airtight for the *pointer*: the only writers of `wsConn` are `Connect`, `Close`,
`SendAudio` (current connection only) and the reader's guarded defer. It is not airtight for
the *callbacks*, which is the actual §5.2 hole (B4). (d) The consequence you accepted, a
reconnect per press, is fine; the consequence you didn't see is that under session billing
the connection must also *end* per press.

**4.2 Hotkey edges.** Correct fix. With one poller emitting strictly alternating edges
starting with `true`, press/release cannot desynchronize, and the "stray release" and
"duplicate press" branches in `Listen`/`waitForRelease` are unreachable in production (only
the tests inject them). Keep them; they're cheap. What's missing is a timestamp on the edge
(B1). Holds shorter than a poll tick are invisible and a release+press inside one tick merges
two holds; both are fine.

**4.3 Version gate.** The fix is right. The gate itself is still wrong (B11).

**4.4 Double paste.** (a) No retry: agreed, and the reasoning in the comment is correct.
(b) Turn-keyed finals: right, but the fix covered finals only. The partial path still clobbers
the aggregate (B2), and the tests never exercise "final, then partial, then consume".

**4.5 Dead code.** Agreed. More to remove: S2 in full, `SpeechState`/`prolongedSilence` (S3),
the `Begin` case and empty `else` in `handleResponses` (`assemblyai.go:212`, `:247-248`).

### 4.2 The §5 known gaps, re-ranked

My order, most to least important, with verdicts:

| Rank | Gap | Verdict |
|---|---|---|
| 1 | 5.2 contamination | **Confirmed, and worse** (stale `Termination`, B4). |
| 2 | 5.4 reconnect latency | **Reframed**: the problem isn't the dial, it's that capture waits for the dial (B1). |
| 3 | 5.3 termination timeout | **Answered by the protocol**: wait for `Termination`; timeout is a safety net, not a UX knob. |
| 4 | 5.1 backpressure | **Real but over-ranked**; the acute bug is overflow handling (B6). |
| 5 | 5.5 health heuristic | **Dead, with proof** (S2). |
| 6 | 5.8 lossy errors | **Agreed**; concrete list in S10 and B14. |
| 7 | 5.6 hotkey | **Keep the chord; add two checks** (S8). |
| 8 | 5.10 startup network | **Agreed, plus two more bugs** (B11). |
| 9 | 5.11 sync beeps | **Agreed, measured** (B16). |
| 10 | 5.12 clipboard | **Reasoning is right; keep it.** |
| 11 | 5.9 context | **Fine as is.** |
| 12 | 5.7 non-macOS | **Moot** (B18). |

**5.1 Backpressure.** The kernel absorbs about 4 s of audio at 32 KB/s (`net.inet.tcp.sendspace`
is 131072 on this machine), so a write only blocks once the network has been stalled for
seconds, and then the 5 s deadline fails it. The acute issue is what happens *next*: the
overflow that the stall causes kills the capture (B6). Shape: capture goroutine never blocks
(`select { case ch <- chunk: default: dropped++ }`, or better, append to the in-memory
recording of S9 and let the sender read by offset); a sender goroutine coalesces queued
chunks into frames ≤ 960 ms (docs: 50–1000 ms per frame, and "the stream should not exceed
real-time speed", so catch-up bursts should be modest). When the backlog exceeds ~10 s the
network is dead: say so, keep capturing locally, write the WAV. Dropping audio is never the
right answer for dictation.

**5.2** See B4. Per-session state, not a generation counter, but a counter checked under the
mutex is an acceptable minimal patch.

**5.3 The 1 s timeout.** The docs say the wait is mandatory and name the signal: "It is
critical to continue reading from the WebSocket until a 'Termination' message is received to
ensure all final transcripts and revisions are processed," and "Terminate finalizes the open
turn." So the design (wait for `Termination`) is already right; the timeout is only there for
a dead server and should be generous, 3–5 s, not tuned for feel. With B2 fixed, a timeout
costs formatting, never content; with S9 it costs nothing. Measure `Terminate`→`Termination`
under `-v` for a week and set the ceiling at a few times the p99; I'd expect a few hundred
milliseconds. `ForceEndpoint` is not needed for push-to-talk since `Terminate` finalizes.

**5.4 Reconnect on every press.** Keep-warm: **no**, idle time is billed. Pre-dial during
idle: **no**, same reason (an idle pre-dialed session is billed from `Begin`). The 150 ms
sleep: replace it with waiting for `Begin`, which is a real signal that the session exists;
measure how long it takes. Best: start capture immediately, dial concurrently, flush the
buffer on `Begin` (B1). Then the dial isn't faster, it's invisible.

**5.5** Dead, delete (S2).

**5.6** S8.

**5.7** Moot, the file can't compile elsewhere (B18).

**5.8** S10, B14.

**5.9** Not worth threading through. Use `DialContext` with a timeout for the one call that
can hang (B10) and keep `stopChan`.

**5.10** Agreed, and B11.

**5.11** ~100 ms each, measured (B16). Async, and exclude the first ~200 ms from the RMS gate
in case the mic hears the start sound (B9).

**5.12** Your reasoning is correct: you can observe that the target app *changed* the
pasteboard (`changeCount`) but never that it *read* it, so any restore races the paste. The
only non-clobbering alternative is typing the text as keystrokes
(`CGEventKeyboardSetUnicodeString`), which is slow and lossy for long text. Keep the clipboard
design. One cheap addition: also print the transcript in the terminal, so it survives even if
the clipboard is overwritten before you notice a failed paste.

---

## 5. The §6 close reads

**`assemblyai.go`.** Ownership of `wsConn` is consistent for the pointer (see 4.1(c)). The
`terminated` flag is set by `Terminate` and `Close`, cleared by `Connect`, read by
`IsConnected`; all under `wsMutex`; correct. `wsMutex` guarding writes is fine at this size:
gorilla requires serialized writes and there is one writer goroutine plus `Terminate`/`Close`
on the listen goroutine. The mutex is not held across callbacks (good, no deadlock with the
processor). What's wrong is not the locking, it's the object model (S1): the pointer is
protected, the callbacks aren't (B4), and nothing ever closes the socket (B3, B12). No read
deadline (B12), no dial timeout (B10), errors discarded (B14).

**`recorder.go`.** `Stop`'s ordering is correct under a concurrent `Cleanup`: the second
`Stop` sees `!r.recording` and returns, so `close(stopChan)` can't double-fire, and the
stream is only stopped/closed after `streamWg.Wait()`, so the loop can never touch a closed
stream. Two caveats: `Stop` can block for a full write deadline (5 s) when the loop is inside
`SendAudio`, which is why the stop beep can arrive late on a bad network; and `Cleanup`'s
`audio.Terminate()` is not ordered against a concurrent `Start()` (B15). The bigger problems
are the loop's exit paths leaving `recording == true` with no recorded reason (B6, B8, S7),
and reading `stopChan` outside the mutex, which is safe today only because `Start` can't run
while the loop is alive.

**`hotkeys/simple.go`.** Cannot desynchronize with one poller (4.2). Blocking rather than
dropping is the right call, and the buffer of 64 is generous (edges arrive at most once per
tick, ten per second). Missing: timestamps (B1), modifier exclusivity and non-modifier abort
(S8). Polling at 100 ms is fine for a modifier chord; an event tap would give exact edge
times for free but needs the same permissions and more code, not worth it.

**`daemon.go` `OnRelease`.** Step by step:

| Step | Line | If it fails today | Should |
|---|---|---|---|
| `IsRecording()` | 154 | silent return (hides B8) | say why nothing was recording |
| `recorder.Stop()` | 161 | may block ≤ 5 s; the loop's exit reason is lost | return the reason |
| stop beep | 162 | ~100 ms blocking | async |
| quick-press gate | 165 | wrong clock (B1); no `Terminate`, session leaks (B3) | edge timestamps; always terminate |
| RMS gate | 173 | absolute threshold (B9); no `Terminate` (B3) | relative; always terminate |
| `Terminate()` | 182 | error ignored; nil conn returns nil and you still wait 1 s | on error skip the wait, close, report |
| wait ≤ 1 s | 185 | stale signal (B4); on timeout the final is lost and, via B2, earlier sentences too | per-session signal; 3–5 s safety net |
| consume | 191 | B2 | compose per turn |
| reset | 194 | fine | goes away with per-session state |
| paste | 197 | error printed, text on clipboard: good | also print the text |
| ✅ / ❌ | 200–207 | ✅ for partial or truncated results; health counters dead | print the source: final / partial / truncated |

**`processor.go`.** B2 and S6. Also `finalTurnOrder` is arrival order, not numeric order
(`processor.go:48`); over one ordered socket that's the same thing in practice, but sorting
keys at consume time costs nothing and removes the assumption.

---

## 6. Before trusting it daily

In the order I'd do them; the first four are one afternoon each.

1. **Session policy** (B3, B12): no startup session; `Terminate` on every release; close after
   `Termination`/timeout; `inactivity_timeout=30`; log audio vs session seconds. Pin
   `speech_model` and `encoding` (B5).
2. **Per-session state** (B4, S1) and **per-turn composition** (B2, S6). Add the probe tests
   from Appendix A as regressions.
3. **Capture first** (B1): timestamped edges, PortAudio opened before the dial, buffered until
   `Begin`, flush the tail before `Terminate`. Async beeps (B16).
4. **Never report a partial or truncated result as ✅** (B6, B7, B8): overflow is a warning,
   loop exit reasons are returned, sentinel errors, one line that says what you actually got.
5. **WAV on failure** (S9). After this, nothing in this review can lose audio.
6. **`-v`** (S10) and surfaced close codes (B14). Run with it for a week; set the termination
   timeout from the numbers.
7. **Dial timeout** (B10), **flags before the version check and notify-only** (B11), drop
   `.env`-in-CWD (B13), delete the health heuristic (S2) and send-suppression (S3).
8. **Accidental-trigger checks** (S8), relative silence gate (B9).

Tests I'd want with those: "final then partial then consume" and "partials across turns"
(processor); "stale `Termination` does not satisfy the next wait" (session); "abnormal close
code is surfaced" (client); and one daemon-level test against an `httptest` WebSocket server
(you already have the pattern in `client_test.go`) with a fake recorder feeding canned PCM,
asserting the pasted text, that `Terminate` is sent on skip paths, and that the socket is
closed after `Termination`. Put the PortAudio stream behind a two-method interface so the
recorder's overflow and exit-reason paths are testable without a microphone.

Also, before day one: a startup check with `AXIsProcessTrustedWithOptions` (prompt option
set) so a missing Accessibility grant is reported once at launch instead of as a silent
non-paste, and a note in the README that Microphone permission is granted to the *terminal*
app.

---

## 7. If it's going to live for years

- The `Session` value (S1) plus typed messages (S12) is the whole architecture this needs; no
  interfaces for other providers.
- An `NSEvent` global monitor or `CGEventTap` for `flagsChanged` instead of polling, once
  you want sub-tick edge times or want to drop the poll goroutine.
- A config file section for hotkey modifiers, silence gate, termination timeout, model,
  `keyterms_prompt` (names, jargon: documented, up to 100 terms) and `max_turn_silence`
  (default 1536 ms today; raising it for dictation means fewer turn splits and fewer chances
  to hit multi-turn edge cases).
- Batch-API fallback from the buffered WAV when streaming fails.
- A build tag and stub so the non-macOS build fails with one sentence rather than a page of
  cgo errors (B18).

---

## Appendix A — Evidence

### Probe tests (run against the working tree, then deleted)

```go
// Probe 1: final for turn 0, then partial for turn 1, then consume.
p := NewProcessor()
p.ProcessTranscript("First sentence.", 0, true, 0.9)
p.ProcessTranscript("Second sentence", 1, false, 0.1)
got, isFinal := p.ConsumeTranscriptWithFallback()
// got="Second sentence " isFinal=true          <- "First sentence." lost, labelled final

// Probe 2: partials only, across two turns.
p = NewProcessor()
p.ProcessTranscript("Hello there my good friend", 0, false, 0.2)
p.ProcessTranscript("Yes", 1, false, 0.1)
got, isFinal = p.ConsumeTranscriptWithFallback()
// got="Hello there my good friend " isFinal=false   <- turn 1 lost

// Probe 3: a Termination that lands after Reset satisfies the next wait.
p = NewProcessor()
p.Reset()
p.SignalTermination()
select { case <-p.WaitForTermination(): /* returned immediately */ default: }
// returned immediately
```

### Measurements (this machine, 2026-09-09)

| What | Result |
|---|---|
| `osascript -e 'return 1'` spawn (same cost as `beeep.Beep`'s `osascript -e beep`) | 105, 94, 93 ms |
| `curl` to `https://streaming.assemblyai.com/v3/ws`, DNS+TCP+TLS | 91, 136, 64 ms |
| `net.inet.tcp.sendspace` | 131072 bytes (≈ 4 s of 16 kHz PCM16) |

### Library facts (module cache)

- `gordonklaus/portaudio@v0.0.0-20230709114228`: `Stream.Read()` returns
  `newError(C.Pa_ReadStream(...))`; `newError` returns `Error(err)` for every status other
  than `paNoError`, so `InputOverflowed` is a non-nil error with a valid buffer.
  `portaudio.h`: "PaInputOverflowed if input data was discarded by PortAudio after the
  previous call and before this call."
- `gen2brain/beeep@v0.11.1` `beep_darwin.go`: `Beep(freq, duration)` ignores both arguments
  and runs `exec.Command("osascript", "-e", "beep").Run()`.
- `gorilla/websocket@v1.5.3`: `DefaultDialer = &Dialer{Proxy: http.ProxyFromEnvironment,
  HandshakeTimeout: 45 * time.Second}`.

### AssemblyAI docs (fetched 2026-09-09)

- Billing: "Universal Streaming is billed based on the total duration a WebSocket connection
  remains open, rather than the amount of audio processed. Idle time is billed at the same
  rate as active audio streaming."
  (`assemblyai.com/docs/faq/how-does-universal-streaming-session-based-pricing-work`)
- Session cap: 3-hour maximum; sessions that hit it (close code 3008) "are billed for the
  full duration". Other close codes: 3007 audio chunk duration violation, 3006 invalid message
  type or malformed JSON. (`assemblyai.com/docs/streaming/common-session-errors-and-closures`)
- Pricing page: Universal-3.5 Pro Realtime $0.45/hr; Universal-Streaming English and
  Multilingual $0.15/hr. "Close connections immediately when calls end."
  (`assemblyai.com/pricing`)
- API reference: `speech_model` default `universal-3-5-pro`; `encoding` default `pcm_s16le`;
  `format_turns` default `false`; `inactivity_timeout` optional, 5–3600 s, "no timeout if not
  set", reset by `KeepAlive` or audio; `max_turn_silence` default 1536 ms;
  `end_of_turn_confidence_threshold` default 0.4; `keyterms_prompt` up to 100 terms; audio
  frames "between 50ms and 1000ms in length".
  (`assemblyai.com/docs/api-reference/streaming-api/streaming-api`)
- Terminate semantics: "It is critical to continue reading from the WebSocket until a
  'Termination' message is received to ensure all final transcripts and revisions are
  processed." `Termination` carries `audio_duration_seconds` and `session_duration_seconds`.
  `Begin` carries `id`, `expires_at`, and a `configuration` echo. `ForceEndpoint` exists but
  is unnecessary here since "Terminate finalizes the open turn."
  (`assemblyai.com/docs/streaming/message-sequence`,
  `assemblyai.com/docs/speech-to-text/universal-streaming`)
