//go:build linux

package vpn

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/nstest"
)

type recordingRuleCommand struct {
	saved      []byte
	restores   [][]byte
	restoreErr error
}

func (r *recordingRuleCommand) Save(context.Context) ([]byte, error) { return r.saved, nil }
func (r *recordingRuleCommand) Restore(_ context.Context, body []byte) error {
	r.restores = append(r.restores, append([]byte(nil), body...))
	return r.restoreErr
}

func TestForwardBatchAppliesOnlyChangedOwnedRules(t *testing.T) {
	commands := &recordingRuleCommand{saved: []byte("*filter\n:FORWARD DROP [0:0]\n:UNRELATED - [0:0]\n-A UNRELATED -j ACCEPT\nCOMMIT\n")}
	m := &IPTablesManager{wgIface: "wg0", commands: commands}
	plan, err := CompileForwardPlan([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}}, map[string]LabAccessSnapshot{"a": {VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1"}}, []AccessPolicyRule{{Action: AccessAllow}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.ApplyForwardPlan(context.Background(), plan)
	if err != nil || !result.Changed || len(commands.restores) != 1 {
		t.Fatalf("first application: %+v %v", result, err)
	}
	body := string(commands.restores[0])
	if strings.Contains(body, "-F UNRELATED") || strings.Contains(body, "-X UNRELATED") || strings.Contains(body, "-F FORWARD") {
		t.Fatalf("batch affected unrelated state:\n%s", body)
	}
	if !strings.Contains(body, "-i wg0 -o lab1 -s 10.8.0.2/32 -d 10.8.1.0/24") || !strings.Contains(body, "-i lab1 -o wg0 -d 10.8.0.2/32") {
		t.Fatalf("missing symmetric physical bindings:\n%s", body)
	}
	result, err = m.ApplyForwardPlan(context.Background(), plan)
	if err != nil || result.Changed || len(commands.restores) != 1 {
		t.Fatalf("unchanged plan was reapplied: %+v %v restores=%d", result, err, len(commands.restores))
	}
}

// The switch of a lab reaches the kernel in one batch: the default blocks lab-started connections, turning the switch on or off is a
// new application of the plan, and neither touches the FORWARD chain.
func TestForwardBatchCarriesTheLabInitiatedSwitch(t *testing.T) {
	commands := &recordingRuleCommand{saved: []byte("*filter\n:FORWARD DROP [0:0]\nCOMMIT\n")}
	m := &IPTablesManager{wgIface: "wg0", commands: commands}
	apply := func(labInitiated bool) string {
		t.Helper()
		plan, err := CompileForwardPlan([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}},
			map[string]LabAccessSnapshot{"a": {VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1", LabInitiated: labInitiated}}, []AccessPolicyRule{{Action: AccessAllow}})
		if err != nil {
			t.Fatal(err)
		}
		before := len(commands.restores)
		if _, err := m.ApplyForwardPlan(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		if len(commands.restores) != before+1 {
			t.Fatalf("restores %d -> %d, want one batch", before, len(commands.restores))
		}
		return string(commands.restores[len(commands.restores)-1])
	}
	blocked := apply(false)
	if !strings.Contains(blocked, "-i lab1 -o wg0 -d 10.8.0.2/32 -m conntrack --ctstate ESTABLISHED,RELATED --ctdir REPLY -j ") {
		t.Fatalf("the default does not restrict the lab to replies:\n%s", blocked)
	}
	open := apply(true)
	if strings.Contains(open, "--ctdir REPLY") || !strings.Contains(open, "-i lab1 -o wg0 -d 10.8.0.2/32 -j ") {
		t.Fatalf("switch on, the lab side is still restricted:\n%s", open)
	}
	if again := apply(false); !strings.Contains(again, "--ctdir REPLY") {
		t.Fatalf("switch off again, the restriction is missing:\n%s", again)
	}
	for _, body := range []string{blocked, open} {
		if strings.Contains(body, "-F FORWARD") || strings.Contains(body, "-P FORWARD") {
			t.Fatalf("a switch change touched the FORWARD chain:\n%s", body)
		}
	}
}

func TestForwardBatchFailureDoesNotSeedAppliedCache(t *testing.T) {
	commands := &recordingRuleCommand{saved: []byte("*filter\nCOMMIT\n"), restoreErr: errors.New("restore rejected")}
	m := &IPTablesManager{wgIface: "wg0", commands: commands}
	if _, err := m.ApplyForwardPlan(context.Background(), ForwardPlan{}); err == nil {
		t.Fatal("restore failure hidden")
	}
	commands.restoreErr = nil
	result, err := m.ApplyForwardPlan(context.Background(), ForwardPlan{})
	if err != nil || !result.Changed || len(commands.restores) != 2 {
		t.Fatalf("failed first application suppressed retry: %+v %v", result, err)
	}
}

func BenchmarkUnchangedForwardPlan(b *testing.B) {
	m := &IPTablesManager{forwardReady: true, commands: &recordingRuleCommand{}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := m.ApplyForwardPlan(context.Background(), ForwardPlan{}); err != nil {
			b.Fatal(err)
		}
	}
	if len(m.commands.(*recordingRuleCommand).restores) != 0 {
		b.Fatal("unchanged plan invoked restore")
	}
}

type recordingNativeRules struct {
	nativeRuleCommand
	restores [][]byte
}

func (r *recordingNativeRules) Restore(ctx context.Context, body []byte) error {
	r.restores = append(r.restores, append([]byte(nil), body...))
	return r.nativeRuleCommand.Restore(ctx, body)
}
func TestNetnsStartupGateUsesOneClosedBatch(t *testing.T) {
	nstest.Require(t)
	m, err := NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	commands := &recordingNativeRules{}
	m.commands = commands
	if err := m.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Cleanup)
	if len(commands.restores) != 1 {
		t.Fatalf("startup used %d closed batches, want one", len(commands.restores))
	}
	body := string(commands.restores[0])
	if !strings.Contains(body, "-A "+accessChain+" -j DROP") || !strings.Contains(body, "-F FORWARD") {
		t.Fatal("startup can expose an empty return gate")
	}
}
