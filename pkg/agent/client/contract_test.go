package client

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const lockFile = "../protobuf/contract.lock"

func fingerprint(t *testing.T) string {
	t.Helper()
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(protodesc.ToFileDescriptorProto(protobuf.File_pkg_agent_protobuf_agent_proto))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Changing agent.proto without a new ContractVersion fails here: bump ContractMinor (additive) or ContractMajor (breaking),
// the label in the Dockerfile, and write the new "version fingerprint" into pkg/agent/protobuf/contract.lock.
func TestContractLock(t *testing.T) {
	raw, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	want := ContractVersion + " " + fingerprint(t)
	if got := strings.TrimSpace(string(raw)); got != want {
		t.Fatalf("agent.proto or ContractVersion changed without the lock.\nlock: %s\nnow:  %s\nIf the proto change is additive bump ContractMinor, if it breaks a caller bump ContractMajor, then put the line above into %s", got, want, lockFile)
	}
}

func TestDockerfileCarriesTheContract(t *testing.T) {
	raw, err := os.ReadFile("../../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `LABEL com.cybericebox.laboratory.api-contract="`+ContractVersion+`"`) {
		t.Fatalf("the agent image label does not carry contract %s", ContractVersion)
	}
}

func TestCompatible(t *testing.T) {
	for _, c := range []struct {
		built, agent string
		want         bool
	}{
		{"1.1", "1.1", true}, {"1.1", "1.4", true}, {"1.2", "1.1", false}, {"1.1", "2.0", false}, {"1.0", "", true}, {"1.1", "", false},
	} {
		got, err := Compatible(c.built, c.agent)
		if err != nil || got != c.want {
			t.Errorf("Compatible(%q,%q) = %v %v, want %v", c.built, c.agent, got, err, c.want)
		}
	}
	if _, err := Compatible("x", "1.0"); err == nil {
		t.Error("a malformed version must be an error")
	}
}
