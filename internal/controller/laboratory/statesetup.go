package laboratory

import (
	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/cybericebox/laboratory/internal/operator"
	"github.com/cybericebox/laboratory/internal/snapshot"
)

// SetupState turns the operator's state persistence configuration into the
// policy the Lab controller copies onto new devices and the snapshot registry
// client (nil when persistence is off or no registry is configured).
func SetupState(c operator.StateConfig) (StatePolicy, *snapshot.Registry, error) {
	maxBytes, err := c.MaxSnapshotBytes()
	if err != nil {
		return StatePolicy{}, nil, err
	}
	pol := StatePolicy{
		Enabled:          c.Enabled,
		Debounce:         c.Debounce,
		ExcludePaths:     c.ExcludePaths,
		MaxSnapshotBytes: maxBytes,
		MaxLayers:        c.MaxLayers,
	}
	if !c.Enabled || c.RegistryAddr == "" {
		return pol, nil, nil
	}
	reg := &snapshot.Registry{Host: c.RegistryAddr}
	if c.RegistryUser != "" {
		reg.Auth = &authn.Basic{Username: c.RegistryUser, Password: c.RegistryPassword}
	}
	return pol, reg, nil
}
