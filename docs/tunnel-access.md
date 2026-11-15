# Tunnel access to a device service (the gRPC kind)

A lab device can publish a TCP service (by convention the runner on port 9099) to a program that is not a browser: an AI opponent, a
grader. The program reaches it through the lab proxy as a **byte tunnel over a WebSocket**. The proxy never reads the bytes, so whatever the
program and the service speak (gRPC, TLS, mutual TLS, their own tokens) works end to end. The proxy authenticates only the right to open the
tunnel; the service authenticates its caller itself.

This is the access kind `grpc` of the device's `access[]` list (EDA web-direct, programmatic level, see `docs/lab-access-model.md`). Its host is
`<device>-<code>g.<labs domain>`; the access entry of the lab (`LabAccessEntry`, protocol `grpc`) carries it as `url`. The device itself is
reached on its Service port; a NetworkPolicy admits only the proxy pods on that port.

## Embedding it in Go

```go
import "github.com/cybericebox/laboratory/pkg/labtunnel"

conn, err := grpc.NewClient("passthrough:///"+host+":443", // host = the host of the grpc access entry
    grpc.WithContextDialer(labtunnel.Dialer(getHandoff)),
    grpc.WithTransportCredentials(yourOwnTransportCredentials)) // for example mTLS to the device service
```

`getHandoff(ctx)` returns a handoff link for that host (below). `labtunnel.Dialer`:

- exchanges the first handoff for a tunnel token and keeps it per host;
- refreshes the token before it ends (in its last quarter), without a new handoff;
- when the tunnel drops (a proxy replica restarted, a network blip), gRPC dials again and the dialer reconnects, possibly to another replica,
  with the token it still holds;
- asks for a new handoff only when the token cannot be refreshed (past `max_until`, or refused) or when the proxy answers 401 to it;
- returns a `net.Conn` with working deadlines (`os.ErrDeadlineExceeded`, `Timeout()`, `Close`);
- leaves your `TransportCredentials` alone: the TLS of the device service runs inside the tunnel.

Options: `WithTLSConfig` (how the proxy's certificate is checked; the system roots by default), `WithHTTPClient`, `WithEndpoint` (the base URL of the
proxy when it is not `https://<host>:443`, for a test or a private route; the Host header stays the device host).

## How to get the handoff

The platform (the backend that owns the tenant access key) signs it, like the 60 second link of a web device:

| claim | value |
|---|---|
| header `alg`, `kid` | `EdDSA`, the id of the tenant's access key |
| `iss` | the tenant |
| `aud` | `laboratory-proxy` |
| `sub` | a name for the caller (a run id); it is used for the traffic ledger only, it need not be a lab client |
| `group_id` | the lab group of the device |
| `host` | the **full** host name of the device: `<device>-<code>g.<labs domain>` |
| `use` | `tunnel` |
| `sess` | the end of the whole access, unix time (the tokens never go past it, nor past the proxy's session maximum) |
| `iat`, `nbf`, `exp` | `exp - iat` at most 60 seconds |

A handoff is single-purpose: without `use: "tunnel"` the exchange refuses it, and with it the web and console hosts refuse it. There is no
replay cache (60 seconds, one host).

## The protocol, for other languages

All requests go to the device host on the proxy. A request that carries an `Origin` header is refused (403) on all three paths, so a browser
cannot use them. JSON errors look like `{"error": "unauthorized"}`.

1. **Exchange.** `POST https://<host>/_access/exchange`, body `{"handoff": "<jwt>"}`. 200 answers
   `{"token": "...", "expires_at": "<RFC 3339>", "max_until": "<RFC 3339>"}`. The token is opaque to the client. 401: the link is not good
   (expired, another host, another use, a removed key, not the tenant's group). 404: the device has no gRPC kind or its lab is not running.
2. **Refresh.** `POST https://<host>/_access/refresh`, body `{"token": "..."}`, same answer with a new token. It never goes past `max_until`.
   401 when the token is expired, unknown or past `max_until` (ask for a new handoff); 404 as above.
3. **Tunnel.** `GET wss://<host>/_tunnel` with `Sec-WebSocket-Protocol: cib.tunnel.v1, <token>` (exactly these two, in this order, no
   `Origin`). The answer selects `cib.tunnel.v1` only. After the upgrade every **binary** message is a piece of the byte stream towards the
   device, and every binary message from the proxy is a piece of the stream back; messages have no meaning of their own (split and join as
   you like, up to 1 MiB each). Closing the WebSocket closes the TCP connection to the device and the other way round. 401: bad token or
   subprotocols; 404: access lost; 429: too many open tunnels; 502: the device does not answer on its port.

Tokens are valid for one hour at a time and live until `max_until`; refresh when less than a quarter of their life is left. A token is valid
on one host only.

## What cuts a tunnel, and what to do

| event | what the client sees | what to do |
|---|---|---|
| a proxy replica restarts or is stopped | close status 1001 "service restarting", or a dropped connection; streams in flight are cut | dial again; the same token works on any replica; calls in flight are retried by the application |
| the lab stops, is deleted, the device loses its gRPC kind, the group changes owner, the tenant key is removed | close status 1008 "access ended", within the live-check interval (10 s by default); new exchanges and refreshes answer 404 or 401 | stop; start again only when the platform gives a new handoff |
| the tunnel reaches the proxy's maximum lifetime (`proxy.l7.liveMaxLifetime`, 12 h) or `max_until` | close status 1008 | reconnect (refresh) or ask for a handoff |
| the token expired | 401 on the next dial or refresh | ask for a new handoff |
| the device service is down | 502 on the upgrade | retry with backoff |

The proxy meters the bytes like web traffic and applies the participant's bandwidth share; it never logs a token.
