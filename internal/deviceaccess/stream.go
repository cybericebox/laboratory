package deviceaccess

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/probe"
)

// Direction is which way bytes of a stream go.
type Direction int

const (
	// ToDevice is what the participant types (and the content of an upload).
	ToDevice Direction = iota
	// FromDevice is what the device prints (and the content of a download).
	FromDevice
)

func (d Direction) String() string {
	if d == ToDevice {
		return "in"
	}
	return "out"
}

// StreamInfo says whose bytes a Tap sees.
type StreamInfo struct {
	Kind, Namespace, Lab, Device, Client string
}

// Tap sees every byte of a stream. It is the one place where recording a console can be attached later; nothing implements it
// today, and the bytes must not be kept, logged or copied anywhere until the owner decides on recording.
type Tap func(info StreamInfo, dir Direction, p []byte)

// stream is the state of one open console or file transfer.
type stream struct {
	svc   *Service
	req   wire.OpenRequest
	start time.Time

	in, out atomic.Int64

	// idle is reset by every byte that crosses the stream (see pass).
	idleMu  sync.Mutex
	idle    *time.Timer
	idleFor time.Duration

	stopOnce sync.Once
	stopped  chan string
}

func (s *Service) newStream(req wire.OpenRequest, idle time.Duration) *stream {
	st := &stream{svc: s, req: req, start: time.Now(), stopped: make(chan string, 1), idleFor: idle}
	if idle > 0 {
		st.idle = time.NewTimer(idle)
	}
	return st
}

// stop ends the stream for a reason. The first call wins; it never blocks.
func (st *stream) stop(reason string) {
	st.stopOnce.Do(func() { st.stopped <- reason })
}

func (st *stream) idleC() <-chan time.Time {
	if st.idle == nil {
		return nil
	}
	return st.idle.C
}

// pass is the one function the bytes of a stream go through, in both directions, for a console and for a file. It counts them, keeps
// the stream from going idle and shows them to the Tap. Recording, if the owner ever decides to build it, attaches here and nowhere
// else; until then the bytes are not stored, logged or copied.
func (st *stream) pass(dir Direction, p []byte) {
	if len(p) == 0 {
		return
	}
	if dir == ToDevice {
		st.in.Add(int64(len(p)))
	} else {
		st.out.Add(int64(len(p)))
	}
	if st.idle != nil {
		st.idleMu.Lock()
		st.idle.Reset(st.idleFor)
		st.idleMu.Unlock()
	}
	if tap := st.svc.tap; tap != nil {
		tap(StreamInfo{Kind: st.req.Kind, Namespace: st.req.Namespace, Lab: st.req.Lab, Device: st.req.Device, Client: st.req.Client}, dir, p)
	}
}

// finish records the end of the stream: the metric and the one log line. The line holds metadata only, never content and never an
// address.
func (st *stream) finish(reason string) {
	d := time.Since(st.start)
	st.svc.m.sessions.WithLabelValues(st.req.Kind, reason).Inc()
	st.svc.m.bytes.WithLabelValues(st.req.Kind, "in").Add(float64(st.in.Load()))
	st.svc.m.bytes.WithLabelValues(st.req.Kind, "out").Add(float64(st.out.Load()))
	st.svc.log.Info("stream end", "kind", st.req.Kind, "namespace", st.req.Namespace, "lab", st.req.Lab, "device", st.req.Device,
		"client", st.req.Client, "group", st.req.Group, "reason", reason, "duration", d.Round(time.Millisecond).String(),
		"bytesIn", st.in.Load(), "bytesOut", st.out.Load())
}

func (st *stream) begin() {
	st.svc.m.active.WithLabelValues(st.req.Kind).Inc()
	st.svc.log.Info("stream start", "kind", st.req.Kind, "namespace", st.req.Namespace, "lab", st.req.Lab, "device", st.req.Device,
		"client", st.req.Client, "group", st.req.Group)
}

func (st *stream) end(reason string) {
	st.svc.m.active.WithLabelValues(st.req.Kind).Dec()
	st.finish(reason)
}

// outWriter is where the device's output goes: every write is a frame towards the proxy, bounded by the write deadline. It is the
// backpressure: while the proxy does not take bytes the runtime's copy loop is blocked here and the FIFO fills.
type outWriter struct {
	st   *stream
	conn Conn
}

func (w *outWriter) Write(p []byte) (int, error) {
	done := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > wire.MaxData {
			chunk = chunk[:wire.MaxData]
		}
		ctx, cancel := context.WithTimeout(context.Background(), w.st.svc.lim.WriteTimeout)
		err := w.conn.WriteBinary(ctx, wire.Data(chunk))
		cancel()
		if err != nil {
			w.st.stop(wire.ReasonStalled)
			return done, err
		}
		w.st.pass(FromDevice, chunk)
		done += len(chunk)
		p = p[len(chunk):]
	}
	return done, nil
}

// pipeWriter writes into a pipe the runtime reads (the standard input of the exec) and gives up when the reader does not take the
// bytes in time, so a device that never reads cannot hold the stream open.
type pipeWriter struct {
	r       *io.PipeReader
	w       *io.PipeWriter
	timeout time.Duration
}

func newPipe(timeout time.Duration) (*io.PipeReader, *pipeWriter) {
	r, w := io.Pipe()
	return r, &pipeWriter{r: r, w: w, timeout: timeout}
}

var errStalled = errors.New("the device does not read its input")

func (p *pipeWriter) Write(b []byte) error {
	t := time.AfterFunc(p.timeout, func() { _ = p.r.CloseWithError(errStalled) })
	defer t.Stop()
	_, err := p.w.Write(b)
	return err
}

func (p *pipeWriter) Close() { _ = p.w.Close() }

func (p *pipeWriter) Abort() {
	_ = p.r.CloseWithError(io.ErrClosedPipe)
	_ = p.w.CloseWithError(io.ErrClosedPipe)
}

// serveConsole runs a console: a TTY exec of the terminal command of the device, with resize, until the participant leaves, the
// shell exits, the stream is idle or too old, or the proxy goes away.
func (s *Service) serveConsole(ctx context.Context, conn Conn, req wire.OpenRequest) {
	command, err := CheckSettings(req.Command, req.User)
	if err != nil {
		s.refuse(ctx, conn, req, "bad-request", err)
		return
	}
	cols, rows := req.Cols, req.Rows
	if cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}
	if cols < wire.MinSize || cols > wire.MaxSize || rows < wire.MinSize || rows > wire.MaxSize {
		s.refuse(ctx, conn, req, "bad-request", errors.New("terminal size out of range"))
		return
	}
	if !s.counter.acquireConsole(s.lim, clientKey(req), labKey(req)) {
		s.refuse(ctx, conn, req, "limit", errors.New("too many consoles"))
		return
	}
	defer s.counter.releaseConsole(clientKey(req), labKey(req))

	dev, code, err := s.resolve(ctx, req)
	if err != nil {
		s.refuse(ctx, conn, req, code, err)
		return
	}
	spec, err := s.rt.Spec(ctx, dev.ContainerID)
	if err != nil || spec.Process == nil {
		s.refuse(ctx, conn, req, "unavailable", errors.Join(err, errors.New("no container spec")))
		return
	}
	user, err := s.lookupUser(ctx, dev, spec.Process, req.User)
	if err != nil {
		s.refuse(ctx, conn, req, "bad-request", err)
		return
	}
	proc, err := ProcessFor(spec.Process, ExecArgs{Args: command, Terminal: true, Cols: cols, Rows: rows, User: user})
	if err != nil {
		s.refuse(ctx, conn, req, "unavailable", err)
		return
	}

	st := s.newStream(req, s.lim.Idle)
	if !s.track(st) {
		s.refuse(ctx, conn, req, "unavailable", errors.New("shutting down"))
		return
	}
	defer s.untrack(st)
	stdin, stdinW := newPipe(s.lim.WriteTimeout)
	ex, err := s.rt.Start(ctx, dev.ContainerID, proc, IO{Stdin: stdin, Stdout: &outWriter{st: st, conn: conn}, Terminal: true})
	if err != nil {
		s.refuse(ctx, conn, req, "unavailable", err)
		return
	}
	// Whatever happens from here on, the exec is killed and deleted and its FIFOs removed.
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), s.lim.Cleanup)
		defer cancel()
		if err := ex.Close(cctx); err != nil {
			s.log.Error(err, "clean up the console exec", "lab", req.Lab, "device", req.Device)
		}
	}()
	st.begin()
	wctx, cancel := context.WithTimeout(ctx, s.lim.WriteTimeout)
	err = conn.WriteText(wctx, encodeReply(wire.OpenReply{OK: true}))
	cancel()
	if err != nil {
		stdinW.Abort()
		st.end(wire.ReasonError)
		return
	}

	reason := st.runConsole(ctx, conn, ex, stdinW)
	stdinW.Abort()
	st.closeFrame(conn, reason)
	st.end(reason)
}

// runConsole is the loop of a console. It returns the reason the stream ended.
func (st *stream) runConsole(ctx context.Context, conn Conn, ex Exec, stdin *pipeWriter) string {
	life := ctx
	if l := st.svc.lim.Lifetime; l > 0 {
		var cancel context.CancelFunc
		life, cancel = context.WithTimeout(ctx, l)
		defer cancel()
	}
	rctx, stopReader := context.WithCancel(life)
	defer stopReader()
	readerDone := make(chan string, 1)
	go func() { readerDone <- st.readClient(rctx, conn, ex, stdin) }()

	var reason string
	select {
	case reason = <-st.stopped:
	case reason = <-readerDone:
	case <-ex.Wait():
		reason = wire.ReasonExit
	case <-st.idleC():
		reason = wire.ReasonIdle
	case <-life.Done():
		reason = wire.ReasonShutdown
		if errors.Is(life.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			reason = wire.ReasonLifetime
		} else if ctx.Err() != nil {
			reason = wire.ReasonClient
		}
	}
	stopReader()
	return reason
}

// readClient reads the frames of the proxy until the stream ends. It returns the reason it stopped.
func (st *stream) readClient(ctx context.Context, conn Conn, ex Exec, stdin *pipeWriter) string {
	for {
		text, msg, err := conn.Read(ctx)
		if err != nil {
			return wire.ReasonClient
		}
		if text {
			return wire.ReasonError
		}
		typ, payload, err := wire.Parse(msg)
		if err != nil {
			return wire.ReasonError
		}
		switch typ {
		case wire.TypeData:
			st.pass(ToDevice, payload)
			if err := stdin.Write(payload); err != nil {
				return wire.ReasonStalled
			}
		case wire.TypeResize:
			cols, rows, _ := wire.Size(payload)
			rctx, cancel := context.WithTimeout(ctx, st.svc.lim.WriteTimeout)
			err := ex.Resize(rctx, cols, rows)
			cancel()
			if err != nil {
				st.svc.log.V(1).Info("resize failed", "error", err.Error())
			}
		case wire.TypeClose:
			return wire.ReasonClient
		default:
			return wire.ReasonError
		}
	}
}

// closeFrame tells the proxy why the stream ended, if it still listens.
func (st *stream) closeFrame(conn Conn, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), st.svc.lim.WriteTimeout)
	defer cancel()
	_ = conn.WriteBinary(ctx, wire.Close(reason))
}

// lookupUser resolves the user a console runs as inside the device, with the probe (the service has no view of the device's file
// system). Empty means the user of the container.
func (s *Service) lookupUser(ctx context.Context, dev Device, base *specs.Process, name string) (*ExecUser, error) {
	if name == "" {
		return nil, nil
	}
	proc, err := ProcessFor(base, ExecArgs{Args: []string{names.ProbeBinary, probe.CommandResolveUser, name}})
	if err != nil {
		return nil, err
	}
	var out limitedBuffer
	out.max = 4 << 10
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ex, err := s.rt.Start(rctx, dev.ContainerID, proc, IO{Stdout: &out, Stderr: io.Discard})
	if err != nil {
		return nil, err
	}
	defer func() {
		cctx, c := context.WithTimeout(context.Background(), s.lim.Cleanup)
		defer c()
		_ = ex.Close(cctx)
	}()
	select {
	case st := <-ex.Wait():
		if st.Err != nil || st.Code != 0 {
			return nil, errors.New("the user is not known in the device")
		}
	case <-rctx.Done():
		return nil, errors.New("the user lookup timed out")
	}
	return parseUser(out.Bytes())
}
