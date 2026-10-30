package client

import "fmt"

// The API contract of the management agent, "major.minor", is what a caller needs to be compatible with an agent; the
// image tag says nothing about it (a patch release of the laboratory that touches no call must not stop the daemon).
//
//   - A minor is bumped for every additive change of agent.proto (a field, a message, an RPC, an error reason).
//   - The major is bumped for a change that breaks an existing caller.
//   - A caller built against contract A.B works with an agent of contract C.D when C == A and D >= B (see Compatible).
//
// pkg/agent/protobuf/contract.lock holds the version and a fingerprint of agent.proto's descriptor; TestContractLock
// fails when the proto changed without a new version, which forces the bump to be made deliberately. The agent reports
// the version in GetFeatures (contract_version) and the agent image carries it as the label
// com.cybericebox.laboratory.api-contract.
const (
	ContractMajor = 1
	ContractMinor = 3
)

// ContractVersion is the contract of this build of the SDK.
var ContractVersion = fmt.Sprintf("%d.%d", ContractMajor, ContractMinor)

// Compatible says whether a caller built against contract `built` can use an agent that reports contract `agent`.
// An empty `agent` (an agent older than the field) is compatible only with a caller built against 1.0.
func Compatible(built, agent string) (bool, error) {
	bMaj, bMin, err := parseContract(built)
	if err != nil {
		return false, fmt.Errorf("built contract: %w", err)
	}
	if agent == "" {
		return bMaj == 1 && bMin == 0, nil
	}
	aMaj, aMin, err := parseContract(agent)
	if err != nil {
		return false, fmt.Errorf("agent contract: %w", err)
	}
	return aMaj == bMaj && aMin >= bMin, nil
}

func parseContract(v string) (major, minor int, err error) {
	if _, err = fmt.Sscanf(v, "%d.%d", &major, &minor); err != nil || major < 0 || minor < 0 {
		return 0, 0, fmt.Errorf("%q is not major.minor", v)
	}
	return major, minor, nil
}
