package runtime

import (
	"errors"
	"testing"
)

func TestContainerMemoryLimit(t *testing.T) {
	fs := func(files map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("not found")
		}
	}
	for name, c := range map[string]struct {
		files map[string]string
		want  int64
	}{
		"cgroup v2":          {map[string]string{"/sys/fs/cgroup/memory.max": "67108864\n"}, 64 << 20},
		"v2 without a limit": {map[string]string{"/sys/fs/cgroup/memory.max": "max\n"}, 0},
		"cgroup v1":          {map[string]string{"/sys/fs/cgroup/memory/memory.limit_in_bytes": "134217728"}, 128 << 20},
		"v1 without a limit": {map[string]string{"/sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712"}, 0},
		"nothing":            {nil, 0},
		"garbage":            {map[string]string{"/sys/fs/cgroup/memory.max": "lots"}, 0},
	} {
		if got := containerMemoryLimit(fs(c.files)); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
}
