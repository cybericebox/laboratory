// Package client is a thin, ready-to-use gRPC client for the LabManager agent.
// It handles connection setup and mutual TLS so a caller (e.g. the control
// plane) only imports this package and calls the typed LabManager RPCs.
//
// Ported from the legacy agent's client wrapper, adapted to the LabManager
// contract and to mTLS (the LabManager agent authenticates callers by client
// certificate + CN allowlist, not a JWT token).
package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"github.com/cybericebox/laboratory/pkg/tlsreload"
)

type (
	// Config describes how to reach a LabManager agent.
	Config struct {
		// Endpoint is the agent address, host:port. Dial the agent by its
		// certificate hostname (the agent's domain): gRPC then uses that as the
		// TLS SNI and verifies it against the server certificate SAN, so no
		// separate server-name setting is needed.
		Endpoint string
		TLS      TLS
	}

	// TLS configures the transport. When Enabled, the connection is mutual
	// TLS: the client presents CertFile/KeyFile (whose CN must be in the
	// agent's allowlist) and verifies the server against CAFile.
	TLS struct {
		Enabled  bool
		CertFile string // client certificate (PEM) presented for mTLS
		KeyFile  string // client private key (PEM)
		CAFile   string // CA (PEM) that signed the agent's server certificate
	}

	// Client is the typed LabManager client plus connection ownership.
	Client interface {
		protobuf.LabManagerClient
		Close() error
	}

	labManagerClient struct {
		protobuf.LabManagerClient
		conn *grpc.ClientConn
	}
)

// transportCredentials builds the client transport credentials: mutual TLS when
// enabled (client keypair + server CA), or insecure for local/dev. The verified
// server name comes from the dial target authority (Endpoint host).
func transportCredentials(conf TLS) (credentials.TransportCredentials, error) {
	if !conf.Enabled {
		return insecure.NewCredentials(), nil
	}

	// The client certificate is renewed in place by cert-manager: re-read it
	// from the files when they change instead of pinning the first one.
	files, err := tlsreload.New(conf.CertFile, conf.KeyFile, "")
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}

	ca, err := os.ReadFile(conf.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read server CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("append server CA")
	}

	tc := &tls.Config{
		GetClientCertificate: files.GetClientCertificate,
		RootCAs:              pool,
		MinVersion:           tls.VersionTLS12,
	}
	return credentials.NewTLS(tc), nil
}

// NewConnection dials the LabManager agent and returns a typed client. The
// caller owns the connection and must Close it. grpc.NewClient is lazy — the
// actual connection is established on the first RPC.
func NewConnection(config Config) (Client, error) {
	creds, err := transportCredentials(config.TLS)
	if err != nil {
		return nil, fmt.Errorf("build transport credentials: %w", err)
	}

	conn, err := grpc.NewClient(config.Endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial agent %q: %w", config.Endpoint, err)
	}

	return &labManagerClient{
		LabManagerClient: protobuf.NewLabManagerClient(conn),
		conn:             conn,
	}, nil
}

func (c *labManagerClient) Close() error {
	return c.conn.Close()
}
