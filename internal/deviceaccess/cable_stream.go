package deviceaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// The cable stream: the line owner on the node of the console end dials the device access service of the node of the serial end. It is a
// WebSocket on the pod network between two device access pods (the network policy of the chart admits nothing else on that port), never
// the lab network. The first message is a CableOpen signed with the dialer's node key (cable_auth.go); the acceptor serves it only for a
// line its own node-agent gave it with the serial-end role, the same cable, lab and ends, and the dialer's node as the console end's node,
// attaches the serial port and answers with a signed CableReply that adds its own nonce. Then every message is a sealed frame
// (cable_session.go): Data frames both ways and a Close frame with a reason.

// cableReadLimit bounds a message of the cable stream: a frame and what sealing adds.
const cableReadLimit = wire.MaxFrame + sealOverhead

// Bounds of the handshakes that are not authenticated yet: at most maxPendingCable at once and maxPendingPerSource from one address, and the
// first message within cableFirstMessage.
const (
	maxPendingCable     = 64
	maxPendingPerSource = 4
)

var cableFirstMessage = 3 * time.Second

// streamSide is the sealed cable stream as one side of a relay.
type streamSide struct {
	sc      *sealedConn
	ws      *websocket.Conn
	timeout time.Duration
	// rctx is the context of the reads: cancelling it closes the WebSocket, which is what close does after the close frame.
	rctx   context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func newStreamSide(sc *sealedConn, ws *websocket.Conn, timeout time.Duration) *streamSide {
	ctx, cancel := context.WithCancel(context.Background())
	return &streamSide{sc: sc, ws: ws, timeout: timeout, rctx: ctx, cancel: cancel}
}

func (s *streamSide) read() ([]byte, error) {
	for {
		msg, err := s.sc.readFrame(s.rctx)
		if err != nil {
			return nil, err
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
		err := s.sc.writeFrame(ctx, wire.Data(chunk))
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
		_ = s.sc.writeFrame(ctx, wire.Close(reason))
		cancel()
		// No close handshake: a peer that stops reading must not hold the line for the handshake's timeout.
		s.cancel()
		if s.ws != nil {
			_ = s.ws.CloseNow()
		}
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
	keys, err := s.cable.key.Keys()
	if err != nil {
		return nil, wire.LinePeerUnreachable, err
	}
	if s.node == "" {
		return nil, wire.LinePeerUnreachable, errors.New("the service does not know its node name")
	}
	open, key, err := signOpen(keys[0], s.node, cableOpenOf(l), s.cable.now())
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
	ws.SetReadLimit(cableReadLimit)
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
	sc, err := newSealedConn(conn, key, open.Nonce, reply.Accept, open.UID, true)
	if err != nil {
		return fail(wire.LinePeerUnreachable, err)
	}
	return newStreamSide(sc, ws, s.lim.WriteTimeout), "", nil
}

// pendingCable bounds the handshakes not authenticated yet.
type pendingCable struct {
	mu       sync.Mutex
	total    int
	bySource map[string]int
}

func (p *pendingCable) acquire(source string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.total >= maxPendingCable || p.bySource[source] >= maxPendingPerSource {
		return false
	}
	p.total++
	p.bySource[source]++
	return true
}

func (p *pendingCable) release(source string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total--
	if p.bySource[source]--; p.bySource[source] <= 0 {
		delete(p.bySource, source)
	}
}

func sourceOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// CableHandler is the listener of the cable stream (on its own port, chart consoleCables.port): the stream endpoint only.
func (s *Service) CableHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(wire.CablePath, func(w http.ResponseWriter, r *http.Request) {
		source := sourceOf(r)
		if !s.pending.acquire(source) {
			s.m.refused.WithLabelValues("cable-" + wire.CodeLimit).Inc()
			http.Error(w, wire.CodeLimit, http.StatusServiceUnavailable)
			return
		}
		held := true
		release := func() {
			if held {
				held = false
				s.pending.release(source)
			}
		}
		defer release()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		c.SetReadLimit(cableReadLimit)
		defer func() { _ = c.CloseNow() }()
		s.serveCable(r.Context(), wsConn{c}, c, source, release)
	})
	return mux
}

// serveCable serves one cable stream from another node. handshaken is called once the handshake is over (authenticated or refused).
func (s *Service) serveCable(ctx context.Context, conn Conn, ws *websocket.Conn, source string, handshaken func()) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	// answer sends a reply. key is the node key that authenticated the request: only then is a reply signed, so an unauthenticated caller
	// never gets a MAC over anything; the nonce is echoed only when it has the shape of one.
	answer := func(key []byte, code, nonce, accept string) {
		if !validNonce(nonce) {
			nonce = ""
		}
		reply := wire.CableReply{OK: code == "", Error: code, Nonce: nonce, Accept: accept}
		if key != nil {
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
	rctx, cancel := context.WithTimeout(ctx, cableFirstMessage)
	text, msg, err := conn.Read(rctx)
	cancel()
	if err != nil || !text || len(msg) > wire.MaxOpen {
		handshaken()
		return
	}
	var req wire.CableOpen
	dec := json.NewDecoder(bytes.NewReader(msg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		answer(nil, wire.CodeBadRequest, "", "")
		handshaken()
		return
	}
	// Authenticated before anything else is done with it; nothing of a failed request is logged but the code.
	key, err := s.cable.check(req, source)
	if err != nil {
		s.log.Info("cable stream refused", "code", wire.CodeUnauthorized)
		answer(nil, wire.CodeUnauthorized, req.Nonce, "")
		handshaken()
		return
	}
	handshaken()
	l := s.lines.accepting(req)
	if l == nil {
		s.log.Info("cable stream refused: no such line on this node, or not from the node of its console end", "namespace", req.Namespace, "cable", req.Name, "dialer", req.Node)
		answer(key, wire.CodeNotFound, req.Nonce, "")
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
		answer(key, code, req.Nonce, "")
		return
	}
	accept, err := newNonce()
	if err != nil {
		serial.close("")
		return
	}
	sc, err := newSealedConn(conn, key, req.Nonce, accept, req.UID, false)
	if err != nil {
		serial.close("")
		return
	}
	answer(key, "", req.Nonce, accept)
	l.setState(func(st *wire.LineState) { st.SerialAttached, st.Up, st.Reason = true, true, "" })
	s.log.Info("console line up", "role", wire.RoleSerial, "namespace", spec.Namespace, "cable", spec.Name)
	stream := newStreamSide(sc, ws, s.lim.WriteTimeout)
	// a = the console side (the stream from the owner), b = the serial port here
	s.relay(sess.ctx, stream, serial, func(int) string { return wire.ReasonExit })
	l.setState(func(st *wire.LineState) { st.SerialAttached, st.Up, st.Reason = false, false, wire.LinePortDown })
	s.log.Info("console line down", "role", wire.RoleSerial, "namespace", spec.Namespace, "cable", spec.Name)
}
