package l7

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cybericebox/laboratory/internal/names"
)

// The gRPC access of a device (EDA web-direct, programmatic level; docs/lab-access-model.md). The host <device>-<code>g.<base> is the
// management gRPC endpoint of the device, for a client that is not a browser: the brain that drives an AI opponent or the grader.
//
//   - the client speaks gRPC (HTTP/2 over TLS, ALPN h2) and the proxy speaks h2c to the device, so a call, a server stream and a bidi
//     stream pass as they are, with their trailers;
//   - there is no cookie and no handoff. Every call carries "authorization: Bearer <token>", a platform-signed EdDSA token checked like a
//     handoff link (see grpcClaims) but valid for the run (exp - iat at most DefaultGRPCTokenMaxLifetime). exp ends the stream too;
//   - the same live checks as for the web: the group still belongs to the tenant, the lab runs, the client exists, the policy allows it,
//     repeated every live-check interval (CheckLive), and the key that signed the token must still be registered;
//   - no read or write deadline of the server applies, a stream is bounded by the token alone;
//   - every refusal is a gRPC status (a trailers-only answer, application/grpc), never an HTML page.
//
// What the device sees is a plain gRPC server on its port: no token, no cookie, no address of the client.

// gRPC status codes the proxy answers with (google.golang.org/grpc/codes), spelled here so that the proxy needs no gRPC library.
const (
	grpcCanceled         = 1
	grpcPermissionDenied = 7
	grpcNotFound         = 5
	grpcResourceExhaust  = 8
	grpcUnavailable      = 14
	grpcUnauthenticated  = 16
	grpcInternalError    = 13
	grpcContentTypeBase  = "application/grpc"
	grpcPingIdle         = 30 * time.Second
	grpcPingTimeout      = 10 * time.Second
	grpcUpstreamDialWait = 5 * time.Second
)

// newGRPCTransport is the transport to the device: HTTP/2 over plain TCP (h2c with prior knowledge), the way a runner serves gRPC in
// its pod. An idle connection is probed with a ping, so a device that went away is noticed, and no other protocol is tried.
func newGRPCTransport() *http.Transport {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Transport{
		Protocols:           &p,
		HTTP2:               &http.HTTP2Config{SendPingTimeout: grpcPingIdle, PingTimeout: grpcPingTimeout},
		DialContext:         (&net.Dialer{Timeout: grpcUpstreamDialWait}).DialContext,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               nil,
	}
}

// WithGRPCTokenMaxLifetime sets the longest exp - iat of a gRPC access token (zero: DefaultGRPCTokenMaxLifetime).
func (h *Handler) WithGRPCTokenMaxLifetime(d time.Duration) *Handler {
	h.grpcTokenMax = d
	return h
}

// WithGRPCTransport replaces the transport to the device (tests).
func (h *Handler) WithGRPCTransport(t http.RoundTripper) *Handler {
	h.grpcTransport = t
	return h
}

// isGRPCRequest says the request is a gRPC call: HTTP/2 and a content type application/grpc (+proto, +json...).
func isGRPCRequest(r *http.Request) bool {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return r.ProtoMajor == 2 && (ct == grpcContentTypeBase || strings.HasPrefix(ct, grpcContentTypeBase+"+") || strings.HasPrefix(ct, grpcContentTypeBase+";"))
}

// grpcFail answers a call with a gRPC status and nothing else: HTTP 200 with the status in the headers (a trailers-only response).
func grpcFail(w http.ResponseWriter, code int, message string) {
	hdr := w.Header()
	hdr.Set("Content-Type", grpcContentTypeBase)
	hdr.Set("Grpc-Status", strconv.Itoa(code))
	hdr.Set("Grpc-Message", grpcMessage(message))
	hdr.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// grpcMessage percent-encodes a message for the grpc-message header: printable ASCII except '%' as it is, the rest as %XX.
func grpcMessage(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x20 && c <= 0x7e && c != '%' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// serveGRPC serves a call to the gRPC access of a device. task is the first label of the host (<device>-<code>g).
func (h *Handler) serveGRPC(w http.ResponseWriter, r *http.Request, task string) {
	if !isGRPCRequest(r) {
		// Not a gRPC client (a browser, a plain HTTP/1.1 client): there is nothing here for it, and no page to show.
		http.Error(w, "gRPC over HTTP/2 only", http.StatusUnsupportedMediaType)
		return
	}
	if h.access == nil {
		grpcFail(w, grpcNotFound, "not found")
		return
	}
	// The whole call is judged by the token and by the live checks, not by the server's read and write deadlines.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	raw, ok := bearerToken(r.Header.Get("Authorization"))
	var claims grpcClaims
	var kid string
	var err error
	if ok {
		claims, kid, err = verifyGRPCToken(raw, h.keys, h.groupTenant, task, h.now, h.grpcTokenMax)
	}
	if !ok || err != nil {
		// A client that keeps sending a bad token is slowed down; the calls of a good token are never counted.
		if !h.authLimit.allow("grpc|" + task) {
			w.Header().Set("Retry-After", "5")
			grpcFail(w, grpcResourceExhaust, "too many requests")
			return
		}
		grpcFail(w, grpcUnauthenticated, "unauthenticated")
		return
	}

	ctx := r.Context()
	group, err := h.access.Group(ctx, claims.GroupID)
	if err != nil || group.Tenant != claims.Issuer {
		grpcFail(w, grpcNotFound, "not found")
		return
	}
	route, err := h.access.GRPCRoute(ctx, task, group.Namespace)
	if err != nil {
		grpcFail(w, grpcNotFound, "not found")
		return
	}
	client := claims.Subject
	if !h.access.Allowed(ctx, group.Namespace, client, route.Lab) {
		grpcFail(w, grpcNotFound, "not found")
		return
	}
	if h.prepare != nil {
		prepareCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = h.prepare(prepareCtx, group.Namespace)
		cancel()
		if err != nil {
			grpcFail(w, grpcUnavailable, "service unavailable")
			return
		}
	}
	target, err := url.Parse(route.URL)
	if err != nil {
		grpcFail(w, grpcInternalError, "internal error")
		return
	}

	// The device never sees the token, a cookie or an address of the client.
	r = r.Clone(ctx)
	r.Header.Del("Authorization")
	r.Header.Del("Cookie")
	stripClientAddress(r.Header)

	// Track the call: CheckLive cancels it when the access is lost, and exp of the token is its last moment.
	clientCtx := r.Context()
	callCtx, cancel := context.WithCancel(clientCtx)
	defer cancel()
	r = r.WithContext(callCtx)
	issuer := claims.Issuer
	key, _ := h.keys(issuer, kid)
	entry := &liveEntry{group: claims.GroupID, client: client, lab: route.Lab, tenant: issuer, deadline: claims.ExpiresAt.Time, cancel: cancel,
		valid: func() bool {
			k, ok := h.keys(issuer, kid)
			return ok && k.Equal(key)
		}}
	if !h.live.tryAdd(entry, h.caps) {
		w.Header().Set("Retry-After", "5")
		grpcFail(w, grpcResourceExhaust, "too many requests")
		return
	}
	defer h.live.remove(entry)

	transport := h.grpcTransport
	if transport == nil {
		transport = grpcUpstreamTransport
	}
	if h.accessLog != nil {
		rec := &accessRecorder{ResponseWriter: w}
		w = rec
		transport = &timedTransport{base: transport, rec: rec}
		defer h.accessLog.emit(rec, r, route.Lab, clientCtx)
	}

	// The transport to the device writes the request body in the goroutine that also waits for the end of the call, and a read of the
	// body of an idle bidi stream does not end by itself when the call is cancelled: closing the body does, and the call with it.
	if body := r.Body; body != nil && body != http.NoBody {
		defer context.AfterFunc(callCtx, func() { _ = body.Close() })()
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	// Responses of a stream go out as they come (the reverse proxy flushes at once when the length is unknown); trailers pass with them.
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		switch {
		case errors.Is(err, context.Canceled) && clientCtx.Err() == nil:
			grpcFail(w, grpcCanceled, "access ended")
		case errors.Is(err, context.Canceled):
			grpcFail(w, grpcCanceled, "canceled")
		default:
			grpcFail(w, grpcUnavailable, "device unavailable")
		}
	}
	// A call cut by the live check has already sent its headers and is left half-way: the reverse proxy aborts the response, which the
	// client would read as a reset. The status is put into the trailers instead, so the client learns why its stream ended.
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if v != http.ErrAbortHandler || callCtx.Err() == nil || clientCtx.Err() != nil {
			panic(v)
		}
		code, message := grpcPermissionDenied, "access ended"
		if !h.now().Before(entry.deadline) {
			code, message = grpcUnauthenticated, "token expired"
		}
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", strconv.Itoa(code))
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", grpcMessage(message))
	}()
	proxy.ServeHTTP(w, r)
}

// grpcUpstreamTransport is the shared transport to the devices.
var grpcUpstreamTransport = newGRPCTransport()

// GRPCRoute is the route of the gRPC access Service named task in a group namespace: h2c to the port of the Service.
func (a *AccessReader) GRPCRoute(ctx context.Context, task, namespace string) (AccessRoute, error) {
	var svc corev1.Service
	if err := a.Reader.Get(ctx, types.NamespacedName{Name: task, Namespace: namespace}, &svc); err != nil {
		return AccessRoute{}, err
	}
	if !svc.DeletionTimestamp.IsZero() || svc.Labels[names.LabelLab] == "" || svc.Labels[names.LabelDevice] == "" ||
		svc.Labels[names.LabelAccessKind] != names.AccessKindGRPC || len(svc.Spec.Ports) == 0 {
		return AccessRoute{}, fmt.Errorf("service is not a gRPC access of a lab device")
	}
	declared := svc.Spec.Ports[0]
	if declared.Port < 1 || declared.Port > 65535 || declared.Protocol != "" && declared.Protocol != corev1.ProtocolTCP {
		return AccessRoute{}, fmt.Errorf("service has an invalid gRPC port")
	}
	return AccessRoute{fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", task, namespace, declared.Port), svc.Labels[names.LabelLab]}, nil
}
