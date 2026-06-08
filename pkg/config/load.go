package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net"
	"reflect"

	"github.com/caarlos0/env/v11"
)

// ParserFunc is an alias for the env package's parser function type.
type ParserFunc = env.ParserFunc

// Load parses environment variables into cfg using `env` struct tags.
//
// Built-in parsers:
//   - *net.IPNet — parsed via net.ParseCIDR
//   - ed25519.PublicKey — decoded from standard base64
//
// Pass extra to register additional type parsers.
func Load[T any](cfg *T, extra ...map[reflect.Type]env.ParserFunc) error {
	m := map[reflect.Type]env.ParserFunc{
		reflect.TypeOf(net.IPNet{}): func(v string) (interface{}, error) {
			_, ipNet, err := net.ParseCIDR(v)
			if err != nil {
				return nil, err
			}
			return *ipNet, nil
		},
		reflect.TypeOf(ed25519.PublicKey(nil)): func(v string) (interface{}, error) {
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return nil, fmt.Errorf("decode base64: %w", err)
			}
			if len(b) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("must be %d bytes, got %d", ed25519.PublicKeySize, len(b))
			}
			return ed25519.PublicKey(b), nil
		},
	}
	for _, e := range extra {
		for k, v := range e {
			m[k] = v
		}
	}
	return env.ParseWithOptions(cfg, env.Options{FuncMap: m})
}
