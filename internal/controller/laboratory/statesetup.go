package laboratory

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/operator"
	"github.com/cybericebox/laboratory/internal/snapshot"
)

// SetupState turns the operator's state persistence configuration into the
// policy the Lab controller copies onto new devices and the snapshot registry
// client (nil when persistence is off or no registry is configured).
func SetupState(c operator.StateConfig) (StatePolicy, *snapshot.Registry, error) {
	maxBytes, err := c.WriteQuotaBytes()
	if err != nil {
		return StatePolicy{}, nil, err
	}
	maxFile, err := c.MaxFileBytes()
	if err != nil {
		return StatePolicy{}, nil, err
	}
	tenantQuota, err := c.TenantQuotaBytes()
	if err != nil {
		return StatePolicy{}, nil, err
	}
	pol := StatePolicy{
		MaxFileBytes:    maxFile,
		MaxEntries:      int32(c.MaxEntries),
		TenantQuota:     tenantQuota,
		Enabled:         c.Enabled,
		Debounce:        c.Debounce,
		ExcludePaths:    c.ExcludePaths,
		WriteQuotaBytes: maxBytes,
		MaxLayers:       c.MaxLayers,
	}
	if !c.Enabled || c.RegistryAddr == "" {
		return pol, nil, nil
	}
	if c.RegistryCAFile == "" && !c.RegistryInsecureDev {
		return StatePolicy{}, nil, fmt.Errorf("STATE_REGISTRY_CA_FILE is required: the operator's token never travels in the clear (STATE_REGISTRY_INSECURE_DEV is the development setup, with no token)")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if c.RegistryCAFile != "" {
		pem, err := os.ReadFile(c.RegistryCAFile)
		if err != nil {
			return StatePolicy{}, nil, fmt.Errorf("STATE_REGISTRY_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return StatePolicy{}, nil, fmt.Errorf("STATE_REGISTRY_CA_FILE: no certificate in %s", c.RegistryCAFile)
		}
		host, _, herr := net.SplitHostPort(c.RegistryAddr)
		if herr != nil {
			host = c.RegistryAddr
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12}
	}
	// The operator is known to the gate by its own token, on every request; it has no registry account.
	if c.RegistryCAFile == "" { // development: plain http, no token
		return pol, &snapshot.Registry{Host: c.RegistryAddr, Transport: tr}, nil
	}
	return pol, &snapshot.Registry{Host: c.RegistryAddr, Transport: imagecache.NewTokenFile(c.RegistryTokenFile).RoundTripper(tr)}, nil
}
