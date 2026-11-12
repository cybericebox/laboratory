package deviceaccess

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

const (
	maxName = 253
	maxPath = 4096
	maxID   = 256
)

// decodeOpen parses and bounds an open request. Unknown fields are an error: a caller cannot slip in a field the service ignores
// today and honours tomorrow.
func decodeOpen(msg []byte) (wire.OpenRequest, error) {
	var r wire.OpenRequest
	dec := json.NewDecoder(bytes.NewReader(msg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("open request: %w", err)
	}
	for _, f := range []struct {
		name string
		v    string
		max  int
	}{{"namespace", r.Namespace, maxName}, {"lab", r.Lab, maxName}, {"device", r.Device, maxName}, {"path", r.Path, maxPath},
		{"client", r.Client, maxID}, {"group", r.Group, maxID}, {"user", r.User, 64}} {
		if len(f.v) > f.max {
			return r, fmt.Errorf("open request: %s is too long", f.name)
		}
	}
	if r.Namespace == "" || r.Lab == "" || r.Device == "" {
		return r, errors.New("open request: namespace, lab and device are required")
	}
	if r.Size < 0 {
		return r, errors.New("open request: negative size")
	}
	return r, nil
}

func encodeReply(r wire.OpenReply) []byte {
	b, _ := json.Marshal(r)
	return b
}
