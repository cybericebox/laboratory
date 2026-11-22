// Package crdcheck lets the agent and the operator refuse to start against CRDs that are older than they are. helm does not update the
// crds/ directory, so a release upgraded without `kubectl apply` leaves the API server pruning the new fields; for the enrollment epoch that
// means a token burnt for nothing. The check reads the published OpenAPI schema of the laboratory group (no extra permission: discovery is
// open to every authenticated client) and looks for the fields the binary needs.
package crdcheck

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/client-go/discovery"
)

// Required are the schema fields that came with this version (a JSON property name each, and the object it belongs to for the message).
var Required = []struct{ Field, On string }{
	{"certificateEpoch", "Tenant.status"},
}

// consoleCablesRequired is required only while consoleCables.enabled is on (Device.spec.consolePorts comes with the ConsoleCable CRD in
// the same crds/ directory): only then does the operator watch ConsoleCables. With the switch off an upgrade may roll the operator out
// before the CRDs, and the labs keep running (a missing ConsoleCable kind is no cable).
var consoleCablesRequired = struct{ Field, On string }{"consolePorts", "Device.spec"}

// Option adds what a switch of the chart needs to the check.
type Option func(*[]struct{ Field, On string })

// ConsoleCables requires the console cable CRDs when on is true (the chart's consoleCables.enabled).
func ConsoleCables(on bool) Option {
	return func(req *[]struct{ Field, On string }) {
		if on {
			*req = append(*req, consoleCablesRequired)
		}
	}
}

const groupVersionPath = "apis/laboratory.cybericebox.com/v1alpha1"

// Fetch returns the OpenAPI v3 document of the laboratory group.
type Fetch func(context.Context) ([]byte, error)

// FromDiscovery reads the document through the discovery client.
func FromDiscovery(d discovery.DiscoveryInterface) Fetch {
	return func(context.Context) ([]byte, error) {
		paths, err := d.OpenAPIV3().Paths()
		if err != nil {
			return nil, err
		}
		gv, ok := paths[groupVersionPath]
		if !ok {
			return nil, fmt.Errorf("the API server publishes no schema for %s: the CRDs are not installed", groupVersionPath)
		}
		return gv.Schema("application/json")
	}
}

// Verify returns an error that names every missing field and says what to do.
func Verify(ctx context.Context, fetch Fetch, opts ...Option) error {
	doc, err := fetch(ctx)
	if err != nil {
		return err
	}
	req := append([]struct{ Field, On string }(nil), Required...)
	for _, o := range opts {
		o(&req)
	}
	var missing []string
	for _, r := range req {
		if !bytes.Contains(doc, []byte(`"`+r.Field+`"`)) {
			missing = append(missing, r.On+"."+r.Field)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the installed CRDs are older than this release (no %s): apply them first with `kubectl apply --server-side -f charts/laboratory/crds/` (helm upgrade does not)", strings.Join(missing, ", "))
	}
	return nil
}

// Wait retries Verify for up to timeout (the schema of a CRD applied a moment ago takes a few seconds to be published).
func Wait(ctx context.Context, fetch Fetch, timeout time.Duration, log func(error), opts ...Option) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		err := Verify(ctx, fetch, opts...)
		if err == nil {
			return nil
		}
		log(err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(5 * time.Second):
		}
	}
}
