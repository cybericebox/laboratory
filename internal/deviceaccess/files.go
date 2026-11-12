package deviceaccess

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"path"
	"strings"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/probe"
)

// File transfer. The file is read or written by the probe inside the device, started through the same exec as the console and run
// as the same user, so the participant can reach exactly what their shell reaches and the node's file system is never involved. The
// probe refuses anything that is not a regular file. The service streams: nothing is kept whole in memory, and a slow peer is
// backpressure on the pipe, not a buffer.

// errorFor maps the exit code of the probe to the short code a caller sees. The probe's stderr is never read: it is the device's text.
func errorFor(code uint32) string {
	switch code {
	case probe.ExitNotFound:
		return "not-found"
	case probe.ExitNotRegular:
		return "not-regular"
	case probe.ExitDenied:
		return "denied"
	}
	return "io"
}

func (s *Service) serveFile(ctx context.Context, conn Conn, req wire.OpenRequest) {
	clean := path.Clean(req.Path)
	if req.Path == "" || !strings.HasPrefix(req.Path, "/") || strings.ContainsRune(req.Path, 0) || clean != req.Path && clean+"/" != req.Path {
		s.refuse(ctx, conn, req, "bad-request", errors.New("the path must be absolute and clean"))
		return
	}
	if req.Kind == wire.KindPut && (req.Size < 0 || req.Size > s.lim.MaxFile) {
		s.refuse(ctx, conn, req, "too-big", errors.New("the upload is larger than the limit"))
		return
	}
	if _, err := CheckSettings(nil, req.User); err != nil {
		s.refuse(ctx, conn, req, "bad-request", err)
		return
	}
	if !s.counter.acquireTransfer(s.lim, clientKey(req)) {
		s.refuse(ctx, conn, req, "limit", errors.New("too many file transfers"))
		return
	}
	defer s.counter.releaseTransfer(clientKey(req))

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
	cmd := probe.CommandGet
	if req.Kind == wire.KindPut {
		cmd = probe.CommandPut
	}
	proc, err := ProcessFor(spec.Process, ExecArgs{Args: []string{names.ProbeBinary, cmd, req.Path}, User: user})
	if err != nil {
		s.refuse(ctx, conn, req, "unavailable", err)
		return
	}
	st := s.newStream(req, s.lim.TransferTimeout)
	if !s.track(st) {
		s.refuse(ctx, conn, req, "unavailable", errors.New("shutting down"))
		return
	}
	defer s.untrack(st)

	tctx, cancel := context.WithTimeout(ctx, s.lim.TransferTimeout)
	defer cancel()
	if req.Kind == wire.KindPut {
		s.put(tctx, conn, st, dev, proc)
	} else {
		s.get(tctx, conn, st, dev, proc)
	}
}

func (s *Service) reply(conn Conn, r wire.OpenReply) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.lim.WriteTimeout)
	defer cancel()
	return conn.WriteText(ctx, encodeReply(r))
}

func (s *Service) cleanup(ex Exec, req wire.OpenRequest) {
	cctx, cancel := context.WithTimeout(context.Background(), s.lim.Cleanup)
	defer cancel()
	if err := ex.Close(cctx); err != nil {
		s.log.Error(err, "clean up the file exec", "lab", req.Lab, "device", req.Device)
	}
}

// put receives frames from the proxy and feeds them to the probe. The upload ends with a close frame; the answer is a final text
// message. The size announced in the open request is exact: fewer or more bytes is an error.
func (s *Service) put(ctx context.Context, conn Conn, st *stream, dev Device, proc *specs.Process) {
	stdin, stdinW := newPipe(s.lim.WriteTimeout)
	ex, err := s.rt.Start(ctx, dev.ContainerID, proc, IO{Stdin: stdin, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		s.refuse(ctx, conn, st.req, "unavailable", err)
		return
	}
	defer s.cleanup(ex, st.req)
	st.begin()
	if err := s.reply(conn, wire.OpenReply{OK: true}); err != nil {
		stdinW.Abort()
		st.end(wire.ReasonError)
		return
	}

	reason, code := wire.ReasonExit, ""
	var got int64
recv:
	for {
		// The proxy streams continuously; a gap longer than the write deadline is a stalled upload.
		rctx, cancel := context.WithTimeout(ctx, s.lim.WriteTimeout)
		text, msg, err := conn.Read(rctx)
		cancel()
		if err != nil {
			reason, code = wire.ReasonClient, "io"
			if ctx.Err() != nil {
				reason = wire.ReasonLifetime
			}
			break
		}
		typ, payload, perr := wire.Parse(msg)
		if text || perr != nil {
			reason, code = wire.ReasonError, "bad-request"
			break
		}
		switch typ {
		case wire.TypeData:
			got += int64(len(payload))
			if got > st.req.Size {
				reason, code = wire.ReasonTooBig, "too-big"
				break recv
			}
			st.pass(ToDevice, payload)
			if err := stdinW.Write(payload); err != nil {
				reason, code = wire.ReasonStalled, "io"
				break recv
			}
		case wire.TypeClose:
			break recv
		default:
			reason, code = wire.ReasonError, "bad-request"
			break recv
		}
	}
	if code == "" && got != st.req.Size {
		reason, code = wire.ReasonError, "incomplete"
	}
	if code != "" {
		stdinW.Abort()
		_ = s.reply(conn, wire.OpenReply{Error: code})
		st.end(reason)
		return
	}
	stdinW.Close()
	select {
	case es := <-ex.Wait():
		if es.Err != nil || es.Code != 0 {
			reason = wire.ReasonError
			code = errorFor(es.Code)
		}
	case <-time.After(s.lim.WriteTimeout):
		reason, code = wire.ReasonStalled, "io"
	case <-ctx.Done():
		reason, code = wire.ReasonLifetime, "io"
	}
	_ = s.reply(conn, wire.OpenReply{OK: code == "", Error: code})
	st.end(reason)
}

// getWriter receives the standard output of the probe's get: eight bytes of length, then the content. It hands the length to the
// service, waits until the service has answered the open request, then sends the content to the proxy as frames, cut at the length.
type getWriter struct {
	st   *stream
	conn Conn
	max  int64

	head   [8]byte
	have   int
	size   int64
	sent   int64
	header chan int64
	gate   chan struct{}
	ctx    context.Context
	bad    bool
}

func (w *getWriter) Write(p []byte) (int, error) {
	total := len(p)
	if w.have < len(w.head) {
		n := copy(w.head[w.have:], p)
		w.have += n
		p = p[n:]
		if w.have < len(w.head) {
			return total, nil
		}
		w.size = int64(binary.BigEndian.Uint64(w.head[:]))
		if w.size < 0 || w.size > w.max {
			w.bad = true
		}
		w.header <- w.size
		select {
		case <-w.gate:
		case <-w.ctx.Done():
			return total - len(p), w.ctx.Err()
		}
		if w.bad {
			return total - len(p), errors.New("the file is larger than the limit")
		}
	}
	if w.bad {
		return total, errors.New("the file is larger than the limit")
	}
	for len(p) > 0 && w.sent < w.size {
		chunk := p
		if len(chunk) > wire.MaxData {
			chunk = chunk[:wire.MaxData]
		}
		if rest := w.size - w.sent; int64(len(chunk)) > rest {
			chunk = chunk[:rest]
		}
		ctx, cancel := context.WithTimeout(w.ctx, w.st.svc.lim.WriteTimeout)
		err := w.conn.WriteBinary(ctx, wire.Data(chunk))
		cancel()
		if err != nil {
			w.st.stop(wire.ReasonStalled)
			return total - len(p), err
		}
		w.st.pass(FromDevice, chunk)
		w.sent += int64(len(chunk))
		p = p[len(chunk):]
	}
	// Anything past the announced length (the file grew) is dropped, not sent.
	return total, nil
}

// get streams a file to the proxy: the open reply carries its size, then data frames, then a close frame whose reason says whether
// the file arrived whole.
func (s *Service) get(ctx context.Context, conn Conn, st *stream, dev Device, proc *specs.Process) {
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &getWriter{st: st, conn: conn, max: s.lim.MaxFile, header: make(chan int64, 1), gate: make(chan struct{}), ctx: gctx}
	ex, err := s.rt.Start(gctx, dev.ContainerID, proc, IO{Stdout: w, Stderr: io.Discard})
	if err != nil {
		s.refuse(ctx, conn, st.req, "unavailable", err)
		return
	}
	defer s.cleanup(ex, st.req)
	st.begin()

	var size int64
	select {
	case size = <-w.header:
	case es := <-ex.Wait():
		select {
		case size = <-w.header:
			// A small file: the probe is done and its length was announced before the runtime reported the exit.
		default:
			// The probe ended before it announced a length: it could not open the file.
			_ = s.reply(conn, wire.OpenReply{Error: errorFor(es.Code)})
			st.end(wire.ReasonError)
			return
		}
	case <-ctx.Done():
		_ = s.reply(conn, wire.OpenReply{Error: "io"})
		st.end(wire.ReasonLifetime)
		return
	}
	if w.bad {
		close(w.gate)
		_ = s.reply(conn, wire.OpenReply{Error: "too-big"})
		st.end(wire.ReasonTooBig)
		return
	}
	if err := s.reply(conn, wire.OpenReply{OK: true, Size: size}); err != nil {
		close(w.gate)
		st.end(wire.ReasonError)
		return
	}
	close(w.gate)

	// The proxy only ever tells us to stop: a close frame or a dropped connection.
	go func() {
		for {
			_, msg, err := conn.Read(gctx)
			if err != nil {
				st.stop(wire.ReasonClient)
				return
			}
			if typ, _, perr := wire.Parse(msg); perr != nil || typ == wire.TypeClose {
				st.stop(wire.ReasonClient)
				return
			}
		}
	}()

	reason := wire.ReasonExit
	select {
	case es := <-ex.Wait():
		if es.Err != nil || es.Code != 0 || w.sent != size {
			reason = wire.ReasonError
		}
	case reason = <-st.stopped:
	case <-st.idleC():
		reason = wire.ReasonStalled
	case <-ctx.Done():
		reason = wire.ReasonLifetime
	}
	cancel()
	st.closeFrame(conn, reason)
	st.end(reason)
}
