package deviceaccess

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
)

// limitedBuffer keeps the first max bytes written to it and drops the rest; a write never fails, so the runtime's copy loop is never
// stopped by an output the service does not want.
type limitedBuffer struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// parseUser reads the answer of the probe's resolve-user: one JSON document.
func parseUser(raw []byte) (*ExecUser, error) {
	var u ExecUser
	if err := json.Unmarshal(bytes.TrimSpace(raw), &u); err != nil {
		return nil, errors.New("the device did not answer the user lookup")
	}
	// A path in HOME is the device's own text; keep it to what an environment value can hold.
	if len(u.Home) > 1024 || len(u.Groups) > 256 {
		return nil, errors.New("the user entry is too large")
	}
	return &u, nil
}
