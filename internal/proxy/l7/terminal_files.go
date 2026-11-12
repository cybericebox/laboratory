package l7

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// File transfer of the console. The files are moved by the probe inside the device (see the device access service); the proxy only
// streams. A transfer belongs to an open console: it needs the token the page got over the console's WebSocket, which no other page
// knows, and it spends the console's budget (MaxFile per file, MaxSession in all).

// transferTimeout is the longest one transfer may take.
const transferTimeout = 10 * time.Minute

type fileError struct {
	Error string `json:"error"`
}

// fileFailure answers a transfer that failed with a short code the page turns into its own words.
func fileFailure(w http.ResponseWriter, code string) {
	status := http.StatusBadGateway
	switch code {
	case wire.CodeNotFound:
		status = http.StatusNotFound
	case wire.CodeDenied:
		status = http.StatusForbidden
	case wire.CodeNotRegular, wire.CodeBadRequest:
		status = http.StatusUnprocessableEntity
	case wire.CodeTooBig:
		status = http.StatusRequestEntityTooLarge
	case wire.CodeLimit, wire.CodeBusy:
		status = http.StatusTooManyRequests
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(fileError{Error: code})
}

// terminalFile handles /files: PUT uploads a file to the device, GET downloads one.
func (h *Handler) terminalFile(w http.ResponseWriter, r *http.Request, claims jwtClaims, client string, group AccessGroup) {
	t := h.terminal
	q := r.URL.Query()
	sess := t.session(q.Get("session"))
	// The token is the capability; the session behind it must also be the caller's own, so a token read by someone else is useless.
	if sess == nil || sess.group != claims.GroupID || sess.client != client || sess.tenant != claims.Tenant || sess.namespace != group.Namespace {
		fail(w, r, http.StatusNotFound, pageGone, "not found")
		return
	}
	switch r.Method {
	case http.MethodPut:
		if !t.sameOrigin(r) {
			fail(w, r, http.StatusForbidden, pageGone, "forbidden")
			return
		}
	case http.MethodGet:
		// A download is a navigation, which has no Origin; the fetch metadata tells a request of this site from another one.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			fail(w, r, http.StatusForbidden, pageGone, "forbidden")
			return
		}
	default:
		fail(w, r, http.StatusMethodNotAllowed, pageGone, "method not allowed")
		return
	}
	filePath := q.Get("path")
	if !validDevicePath(filePath) {
		fileFailure(w, wire.CodeBadRequest)
		return
	}
	// Access is checked again: a lock that closed the console must also stop a transfer that was about to start.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	allowed := h.access.Allowed(ctx, group.Namespace, client, sess.lab)
	cancel()
	if !allowed {
		fail(w, r, http.StatusNotFound, pageGone, "not found")
		return
	}
	if !sess.transferring.CompareAndSwap(false, true) {
		fileFailure(w, wire.CodeBusy)
		return
	}
	defer sess.transferring.Store(false)

	tctx, tcancel := context.WithTimeout(r.Context(), transferTimeout)
	defer tcancel()
	entry := &liveEntry{group: sess.group, client: sess.client, lab: sess.lab, tenant: sess.tenant, deadline: h.liveDeadline(claims.Abs), cancel: tcancel}
	if !h.live.tryAdd(entry, LiveCaps{}) {
		fileFailure(w, wire.CodeBusy)
		return
	}
	defer h.live.remove(entry)

	if r.Method == http.MethodPut {
		h.uploadFile(tctx, w, r, sess, filePath)
		return
	}
	h.downloadFile(tctx, w, r, sess, filePath)
}

// validDevicePath: absolute, clean, bounded, printable.
func validDevicePath(p string) bool {
	if p == "" || len(p) > 4096 || !strings.HasPrefix(p, "/") || p == "/" {
		return false
	}
	if c := path.Clean(p); c != p {
		return false
	}
	for _, c := range p {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// reserve takes n bytes from the budget of the console; it reports false when they are not there.
func (s *consoleSession) reserve(n int64) bool {
	for {
		cur := s.budget.Load()
		if n > cur {
			return false
		}
		if s.budget.CompareAndSwap(cur, cur-n) {
			return true
		}
	}
}

func (s *consoleSession) refund(n int64) { s.budget.Add(n) }

func (h *Handler) openTransfer(ctx context.Context, sess *consoleSession, kind, filePath string, size int64) (*websocket.Conn, wire.OpenReply, error) {
	svc, err := h.terminal.dial(ctx, sess.base)
	if err != nil {
		return nil, wire.OpenReply{}, err
	}
	open, _ := json.Marshal(wire.OpenRequest{Kind: kind, Namespace: sess.namespace, Lab: sess.lab, Device: sess.dev, User: sess.user,
		Path: filePath, Size: size, Client: sess.client, Group: sess.group})
	wctx, cancel := context.WithTimeout(ctx, terminalWrite)
	err = svc.Write(wctx, websocket.MessageText, open)
	cancel()
	if err != nil {
		_ = svc.CloseNow()
		return nil, wire.OpenReply{}, err
	}
	reply, err := readReply(ctx, svc, 20*time.Second)
	if err != nil {
		_ = svc.CloseNow()
		return nil, wire.OpenReply{}, err
	}
	return svc, reply, nil
}

func readReply(ctx context.Context, svc *websocket.Conn, wait time.Duration) (wire.OpenReply, error) {
	rctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	typ, msg, err := svc.Read(rctx)
	var reply wire.OpenReply
	if err != nil || typ != websocket.MessageText || json.Unmarshal(msg, &reply) != nil {
		return reply, errors.New("no answer from the service")
	}
	return reply, nil
}

// uploadFile streams the request body to the device. The length must be announced and within the limits; the service checks that
// exactly that many bytes arrive.
func (h *Handler) uploadFile(ctx context.Context, w http.ResponseWriter, r *http.Request, sess *consoleSession, filePath string) {
	t := h.terminal
	size := r.ContentLength
	if size < 0 || size > t.cfg.MaxFile {
		fileFailure(w, wire.CodeTooBig)
		return
	}
	if !sess.reserve(size) {
		fileFailure(w, wire.CodeTooBig)
		return
	}
	ok := false
	defer func() {
		if !ok {
			sess.refund(size)
		}
	}()
	svc, reply, err := h.openTransfer(ctx, sess, wire.KindPut, filePath, size)
	if err != nil {
		fileFailure(w, wire.CodeUnavailable)
		return
	}
	defer func() { _ = svc.CloseNow() }()
	if !reply.OK {
		fileFailure(w, reply.Error)
		return
	}
	rc := http.NewResponseController(w)
	buf := make([]byte, wire.MaxData)
	var sent int64
	for sent < size {
		_ = rc.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := io.ReadFull(io.LimitReader(r.Body, size-sent), buf)
		if n > 0 {
			wctx, cancel := context.WithTimeout(ctx, terminalWrite)
			werr := svc.Write(wctx, websocket.MessageBinary, wire.Data(buf[:n]))
			cancel()
			if werr != nil {
				fileFailure(w, wire.CodeUnavailable)
				return
			}
			sent += int64(n)
			sess.touch()
		}
		if err != nil {
			break
		}
	}
	if sent != size {
		_ = svc.Write(ctx, websocket.MessageBinary, wire.Close(wire.ReasonError))
		fileFailure(w, wire.CodeIncomplete)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, terminalWrite)
	werr := svc.Write(cctx, websocket.MessageBinary, wire.Close(wire.ReasonExit))
	cancel()
	if werr != nil {
		fileFailure(w, wire.CodeUnavailable)
		return
	}
	final, err := readReply(ctx, svc, 60*time.Second)
	if err != nil {
		fileFailure(w, wire.CodeUnavailable)
		return
	}
	if !final.OK {
		fileFailure(w, final.Error)
		return
	}
	ok = true
	w.WriteHeader(http.StatusNoContent)
}

// downloadFile streams a file of the device to the browser as an attachment. The headers are fixed: it is bytes to save, never a page
// to show, whatever the file is called or contains.
func (h *Handler) downloadFile(ctx context.Context, w http.ResponseWriter, r *http.Request, sess *consoleSession, filePath string) {
	t := h.terminal
	svc, reply, err := h.openTransfer(ctx, sess, wire.KindGet, filePath, 0)
	if err != nil {
		fileFailure(w, wire.CodeUnavailable)
		return
	}
	defer func() { _ = svc.CloseNow() }()
	if !reply.OK {
		fileFailure(w, reply.Error)
		return
	}
	if reply.Size < 0 || reply.Size > t.cfg.MaxFile || !sess.reserve(reply.Size) {
		_ = svc.Write(ctx, websocket.MessageBinary, wire.Close(wire.ReasonTooBig))
		fileFailure(w, wire.CodeTooBig)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("Content-Disposition", `attachment; filename="`+downloadName(filePath)+`"`)
	hd.Set("Content-Length", strconv.FormatInt(reply.Size, 10))
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	hd.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	var got int64
	for {
		rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		typ, msg, err := svc.Read(rctx)
		cancel()
		if err != nil || typ != websocket.MessageBinary {
			sess.refund(reply.Size - got)
			panic(http.ErrAbortHandler)
		}
		ft, payload, perr := wire.Parse(msg)
		if perr != nil {
			sess.refund(reply.Size - got)
			panic(http.ErrAbortHandler)
		}
		switch ft {
		case wire.TypeData:
			got += int64(len(payload))
			if got > reply.Size {
				panic(http.ErrAbortHandler)
			}
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := w.Write(payload); err != nil {
				_ = svc.Write(ctx, websocket.MessageBinary, wire.Close(wire.ReasonClient))
				return
			}
			sess.touch()
		case wire.TypeClose:
			// A download that did not arrive whole is cut off, so the browser reports a failed download and does not keep a short file
			// as if it were complete.
			if string(payload) != wire.ReasonExit || got != reply.Size {
				panic(http.ErrAbortHandler)
			}
			return
		default:
			panic(http.ErrAbortHandler)
		}
	}
}

// downloadName is the file name offered to the browser: the last element of the path, reduced to letters, digits and a few safe
// punctuation marks, so it can never break out of the header or suggest a path or an executable extension trick.
func downloadName(p string) string {
	base := path.Base(p)
	if base == "/" || base == "." || base == ".." {
		return "download"
	}
	var b strings.Builder
	for _, c := range base {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 100 {
			break
		}
	}
	name := strings.TrimLeft(b.String(), ".")
	if name == "" {
		return "download"
	}
	return name
}
