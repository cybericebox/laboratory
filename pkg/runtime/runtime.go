// Package runtime configures Go runtime parameters for containerised deployments.
// Import with a blank identifier to activate:
//
//	import _ "github.com/cybericebox/laboratory/pkg/runtime"
package runtime

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	_ "go.uber.org/automaxprocs" // sets GOMAXPROCS from cgroup CPU quota
)

// memoryLimitShare is the part of the container's memory limit the Go runtime is asked to stay under (a soft limit: the collector works
// harder as the heap nears it), so that a burst of requests is garbage collected before the kernel kills the container.
const memoryLimitShare = 0.8

func init() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return // set by hand
	}
	if limit := containerMemoryLimit(os.ReadFile); limit > 0 {
		debug.SetMemoryLimit(int64(float64(limit) * memoryLimitShare))
	}
}

// containerMemoryLimit is the memory limit of the container's cgroup in bytes, 0 when there is none (cgroup v2, then v1).
func containerMemoryLimit(read func(string) ([]byte, error)) int64 {
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := read(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "" || s == "max" {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 || n >= 1<<60 { // cgroup v1 reports "no limit" as a huge number
			return 0
		}
		return n
	}
	return 0
}
