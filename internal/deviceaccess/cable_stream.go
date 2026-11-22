package deviceaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// The cable stream: the line owner on the node of the console end dials the device access service of the node of the serial end. It is a
// WebSocket on the pod network between two device access pods (the Cilium policy of the chart admits nothing else on that port), never the
// lab network. The first message is a signed CableOpen (cable_auth.go); the acceptor serves it only for a line its own node-agent gave it
// with the serial-end role and the same cable, lab and ends, attaches the serial port and answers with a signed CableReply. Then Data
// frames both ways and a Close frame with a reason.

// streamSide is the cable stream as one side of a relay.
type streamSide struct {
	conn    Conn
	ws      *websocket.Conn
	timeout time.Duration
	// rctx is the context of the reads: cancelling it closes the WebSocket, which is what close does after the close frame.
	rctx   context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func newStreamSide(conn Conn, ws *websocket.Conn, timeout time.Duration) *streamSide {
	ctx, cancel := context.WithCancel(context.Background())
	return &streamSide{conn: conn, ws: ws, timeout: timeout, rctx: ctx, cancel: cancel}
}

func (s *streamSide) read() ([]byte, error) {
	for {
		text, msg, err := s.conn.Read(s.rctx)
		if err != nil {
			return nil, err
		}
		if text {
			return nil, errors.New("a text message inside the cable stream")
		}
		typ, payload, err := wire.Parse(msg)
		if err != nil {
			return nil, err
		}
		switch typ {
		case wire.TypeData:
			if len(payload) == 0 {
				continue
			}
			return payload, nil
		case wire.TypeClose:
			return nil, errors.New("the peer closed the line: " + string(payload))
		default:
			return nil, errors.New("an unexpected frame in the cable stream")
		}
	}
}

func (s *streamSide) write(p []byte) error {
	for len(p) > 0 {
		chunk := p[:min(len(p), wire.MaxData)]
		ctx, cancel := context.WithTimeout(s.rctx, s.timeout)
		err := s.conn.WriteBinary(ctx, wire.Data(chunk))
		cancel()
		if err != nil {
			return err
		}
		p = p[len(chunk):]
	}
	return nil
}

func (s *streamSide) close(reason string) {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		_ = s.conn.WriteBinary(ctx, wire.Close(reason))
		cancel()
		if s.ws != nil {
			_ = s.ws.Close(websocket.StatusNormalClosure, "")
		}
		s.cancel()
	})
}

// cableOpenOf is the open request of a line.
func cableOpenOf(l wire.LineSpec) wire.CableOpen {
	return wire.CableOpen{Namespace: l.Namespace, Name: l.Name, UID: l.UID, Lab: l.Lab, LabUID: l.LabUID,
		Serial: wire.PortRef{Device: l.Serial.Device, Port: l.Serial.Port}, Console: wire.PortRef{Device: l.Console.Device, Port: l.Console.Port}}
}

// dialPeer opens the cable stream of an owner line to the service of the serial end's node. On a failure it returns the reason of the
// line: PeerUnreachable when the peer cannot be reached or does not prove it holds the key, WaitingForDevice or PortDown when the peer
// says so about the serial end.
func (s *Service) dialPeer(ctx context.Context, l wire.LineSpec) (side, string, error) {
	key, err := s.cable.key.Get()
	if err != nil {
		return nil, wire.LinePeerUnreachable, err
	}
	open, err := signOpen(key, cableOpenOf(l), s.cable.now())
	if err != nil {
		return nil, wire.LinePeerUnreachable, err
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, "ws://"+l.Peer+wire.CablePath, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
		HTTPClient:      &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	if err != nil {
		return nil, wire.LinePeerUnreachable, err
	}
	ws.SetReadLimit(wire.MaxFrame)
	conn := wsConn{ws}
	fail := func(reason string, err error) (side, string, error) {
		_ = ws.CloseNow()
		return nil, reason, err
	}
	body, _ := json.Marshal(open)
	if err := conn.WriteText(dctx, body); err != nil {
		return fail(wire.LinePeerUnreachable, err)
	}
	text, msg, err := conn.Read(dctx)
	if err != nil || !text || len(msg) > wire.MaxOpen {
		return fail(wire.LinePeerUnreachable, errors.Join(err, errors.New("no reply to the cable request")))
	}
	var reply wire.CableReply
	dec := json.NewDecoder(bytes.NewReader(msg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reply); err != nil {
		return fail(wire.LinePeerUnreachable, err)
	}
	if err := checkReply(key, reply, open.Nonce); err != nil {
		return fail(wire.LinePeerUnreachable, err)
	}
	if !reply.OK {
		switch reply.Error {
		case wire.CodeNotFound:
			return fail(wire.LineWaitingForDevice, errors.New("the peer has no such line or no serial device yet"))
		case wire.CodePortDown:
			return fail(wire.LinePortDown, errors.New("the peer could not attach the serial port"))
		}
		return fail(wire.LinePeerUnreachable, errors.New("the peer refused the cable stream: "+reply.Error))
	}
	return newStreamSide(conn, ws, s.lim.WriteTimeout), "", nil
}

// CableHandler is the listener of the cable stream (on its own port, chart consoleCables.port): the stream endpoint only.
func (s *Service) CableHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(wire.CablePath, func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		c.SetReadLimit(wire.MaxFrame)
		defer func() { _ = c.CloseNow() }()
		s.serveCable(r.Context(), wsConn{c}, c)
	})
	return mux
}

// serveCable serves one cable stream from another node.
func (s *Service) serveCable(ctx context.Context, conn Conn, ws *websocket.Conn) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	answer := func(code, nonce string) {
		key, kerr := s.cable.key.Get()
		reply := wire.CableReply{OK: code == "", Error: code, Nonce: nonce}
		if kerr == nil {
			reply = signReply(key, reply)
		}
		b, _ := json.Marshal(reply)
		wctx, cancel := context.WithTimeout(ctx, s.lim.WriteTimeout)
		_ = conn.WriteText(wctx, b)
		cancel()
		if code != "" {
			s.m.refused.WithLabelValues("cable-" + code).Inc()
		}
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	text, msg, err := conn.Read(rctx)
	cancel()
	if err != nil || !text || len(msg) > wire.MaxOpen {
		return
	}
	var req wire.CableOpen
	dec := json.NewDecoder(bytes.NewReader(msg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		answer(wire.CodeBadRequest, "")
		return
	}
	// Authenticated before anything else is done with it; nothing of a failed request is logged but the code.
	if err := s.cable.check(req); err != nil {
		s.log.Info("cable stream refused", "code", wire.CodeUnauthorized)
		answer(wire.CodeUnauthorized, req.Nonce)
		return
	}
	l := s.lines.accepting(req)
	if l == nil {
		s.log.Info("cable stream refused: no such line on this node", "namespace", req.Namespace, "cable", req.Name)
		answer(wire.CodeNotFound, req.Nonce)
		return
	}
	sess := l.beginSession()
	defer l.endSession(sess)
	spec := l.spec
	serial, reason, err := s.attachPort(sess.ctx, endRef{Namespace: spec.Namespace, Lab: spec.Lab, LabUID: spec.LabUID, End: spec.Serial, Serial: true})
	if err != nil {
		l.setState(func(st *wire.LineState) { st.SerialAttached, st.Up, st.Reason = false, false, reason })
		code := wire.CodePortDown
		if reason == wire.LineWaitingForDevice {
			code = wire.CodeNotFound
		}
		s.log.V(1).Info("cable stream: the serial end cannot be attached", "cable", spec.Name, "reason", reason, "error", err.Error())
		answer(code, req.Nonce)
		return
	}
	answer("", req.Nonce)
	l.setState(func(st *wire.LineState) { st.SerialAttached, st.Up, st.Reason = true, true, "" })
	s.log.Info("console line up", "role", wire.RoleSerial, "namespace", spec.Namespace, "cable", spec.Name)
	stream := newStreamSide(conn, ws, s.lim.WriteTimeout)
	// a = the console side (the stream from the owner), b = the serial port here
	s.relay(sess.ctx, stream, serial, func(int) string { return wire.ReasonExit })
	l.setState(func(st *wire.LineState) { st.SerialAttached, st.Up, st.Reason = false, false, wire.LinePortDown })
	s.log.Info("console line down", "role", wire.RoleSerial, "namespace", spec.Namespace, "cable", spec.Name)
}
