package profiles

import (
	"strconv"
	"strings"
	"testing"
)

// The ping range must fit the 65536-id mapping of a user namespace (hostUsers: false): a bound outside it makes runc refuse the
// sysctl and the pod never starts. The same value works without a user namespace.
func TestPingGroupRangeFitsAUserNamespaceMapping(t *testing.T) {
	parts := strings.Fields(PingGroupRange)
	if len(parts) != 2 {
		t.Fatalf("not a range: %q", PingGroupRange)
	}
	lo, err1 := strconv.ParseInt(parts[0], 10, 64)
	hi, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("%q", PingGroupRange)
	}
	const mapped = 65536 // the default size of a pod's user-namespace mapping
	if lo != 0 || hi < lo || hi >= mapped {
		t.Fatalf("range %d..%d does not fit gids 0..%d of a user namespace", lo, hi, mapped-1)
	}
}
