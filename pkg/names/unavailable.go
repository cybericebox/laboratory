package names

import (
	"fmt"
	"strconv"
	"strings"
)

// The reasons GetFeatures gives for a feature the cluster is configured for but its nodes do not confirm
// (StatePersistenceFeature.unavailable_reason, LifecycleFeature.unavailable_reason): a fixed code and a count, "<CODE>:<n>/<m>", with n
// the lab nodes that do not confirm and m the lab nodes asked. Never a node name, a path or an error text. The codes are the allow-list
// of a caller.
const (
	ReasonContainerdRootMissing = "CONTAINERD_ROOT_MISSING"
	ReasonSnapshotterMissing    = "SNAPSHOTTER_MISSING"
	ReasonRuntimeSocket         = "RUNTIME_SOCKET_UNAVAILABLE"
	ReasonSnapshotRegistry      = "SNAPSHOT_REGISTRY_UNREACHABLE"
	ReasonCgroupV2Missing       = "CGROUP_V2_MISSING"
	ReasonPersistenceDisabled   = "PERSISTENCE_DISABLED"
	ReasonObservationDisabled   = "OBSERVATION_DISABLED"
	ReasonNodeUnconfirmed       = "NODE_UNCONFIRMED"
)

// UnavailableReasonCodes is the list of the codes.
var UnavailableReasonCodes = []string{
	ReasonContainerdRootMissing, ReasonSnapshotterMissing, ReasonRuntimeSocket, ReasonSnapshotRegistry, ReasonCgroupV2Missing,
	ReasonPersistenceDisabled, ReasonObservationDisabled, ReasonNodeUnconfirmed,
}

// ParseUnavailableReason reads "<CODE>:<n>/<m>"; an error for anything that is not a known code with 0 <= n <= m, m > 0.
func ParseUnavailableReason(s string) (code string, unconfirmed, asked int, err error) {
	code, counts, ok := strings.Cut(s, ":")
	known := false
	for _, c := range UnavailableReasonCodes {
		known = known || c == code
	}
	if !ok || !known {
		return "", 0, 0, fmt.Errorf("unavailable reason %q is not <CODE>:<n>/<m> with a known code", s)
	}
	a, b, ok := strings.Cut(counts, "/")
	if !ok {
		return "", 0, 0, fmt.Errorf("unavailable reason %q has no n/m", s)
	}
	n, err1 := strconv.Atoi(a)
	m, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || n < 0 || m <= 0 || n > m {
		return "", 0, 0, fmt.Errorf("unavailable reason %q has bad counts", s)
	}
	return code, n, m, nil
}
