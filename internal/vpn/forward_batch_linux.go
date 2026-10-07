//go:build linux

package vpn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
)

type RuleCommand interface {
	Save(context.Context) ([]byte, error)
	Restore(context.Context, []byte) error
}

type nativeRuleCommand struct{}

func (nativeRuleCommand) Save(ctx context.Context) ([]byte, error) {
	c := exec.CommandContext(ctx, "iptables-save", "-c", "-t", "filter")
	b, err := c.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("save VPN counters: %w: %s", err, b)
	}
	return b, nil
}
func (nativeRuleCommand) Restore(ctx context.Context, body []byte) error {
	c := exec.CommandContext(ctx, "iptables-restore", "--noflush", "--counters", "--wait", "5")
	c.Stdin = bytes.NewReader(body)
	b, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("apply VPN batch: %w: %s", err, b)
	}
	return nil
}

type ApplyResult struct{ Changed bool }

const relationChainPrefix = "CICER_"

func relationChain(id, direction string) string { return relationChainPrefix + id + "_" + direction }
func counterComment(id, epoch, direction, kind string) string {
	return "cibacct:" + id + ":" + epoch + ":" + direction + ":" + kind
}

func (m *IPTablesManager) ApplyForwardPlan(ctx context.Context, plan ForwardPlan) (ApplyResult, error) {
	m.forwardMu.Lock()
	defer m.forwardMu.Unlock()
	if m.forwardReady && slices.Equal(m.forwardPlan.Allows, plan.Allows) {
		return ApplyResult{}, nil
	}
	for _, r := range plan.Allows {
		if _, err := LabInterfaceIndex(r.LabInterface); err != nil {
			return ApplyResult{}, err
		}
		if len(r.BindingID) != 16 {
			return ApplyResult{}, fmt.Errorf("invalid binding identity")
		}
		if _, err := hex.DecodeString(r.BindingID); err != nil {
			return ApplyResult{}, err
		}
		if src, ok := parsePrefix(r.ClientCIDR); !ok || !src.Addr().Is4() || src.Bits() != 32 {
			return ApplyResult{}, fmt.Errorf("invalid client address")
		}
		if dst, ok := parsePrefix(r.LabCIDR); !ok || !dst.Addr().Is4() {
			return ApplyResult{}, fmt.Errorf("invalid lab prefix")
		}
	}
	saved, err := m.commands.Save(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	parsed, err := parseKernelCounters(saved, m.bindings)
	if err != nil {
		return ApplyResult{}, err
	}
	wanted := map[string]ForwardRule{}
	for _, r := range plan.Allows {
		wanted[r.BindingID] = r
	}
	var retired flowacct.CounterSnapshot
	retired.At = parsed.snapshot.At
	for _, row := range parsed.snapshot.Rows {
		if _, ok := wanted[row.BindingID]; !ok {
			retired.Rows = append(retired.Rows, row)
		}
	}
	if len(retired.Rows) > 0 && m.BeforeRetire != nil {
		if err := m.BeforeRetire(retired); err != nil {
			return ApplyResult{}, fmt.Errorf("capture retired traffic: %w", err)
		}
	}
	var body strings.Builder
	body.WriteString("*filter\n:" + accessChain + " - [0:0]\n-F " + accessChain + "\n")
	for _, r := range plan.Allows {
		epoch := parsed.epochs[r.BindingID]
		if epoch == "" {
			var random [8]byte
			if _, err := rand.Read(random[:]); err != nil {
				return ApplyResult{}, err
			}
			epoch = hex.EncodeToString(random[:])
			index, _ := LabInterfaceIndex(r.LabInterface)
			mark := FlowCountedMark | uint32(index)<<8
			for _, direction := range []string{"F", "R"} {
				chain := relationChain(r.BindingID, direction)
				fmt.Fprintf(&body, ":%s - [0:0]\n", chain)
				fmt.Fprintf(&body, "-A %s -m conntrack --ctstate NEW --ctdir ORIGINAL ! --ctstatus CONFIRMED -m connmark ! --mark 0x%x/0x%x -m comment --comment %s -j CONNMARK --set-xmark 0x%x/0x%x\n", chain, FlowCountedMark, FlowCountedMark, counterComment(r.BindingID, epoch, direction, "N"), mark, FlowCountedMark|LabIdentityMask)
				fmt.Fprintf(&body, "-A %s -m comment --comment %s -j ACCEPT\n", chain, counterComment(r.BindingID, epoch, direction, "T"))
			}
		}
		fmt.Fprintf(&body, "-A %s -i %s -o %s -s %s -d %s -j %s\n", accessChain, m.wgIface, r.LabInterface, r.ClientCIDR, r.LabCIDR, relationChain(r.BindingID, "F"))
		fmt.Fprintf(&body, "-A %s -i %s -o %s -d %s -j %s\n", accessChain, r.LabInterface, m.wgIface, r.ClientCIDR, relationChain(r.BindingID, "R"))
	}
	body.WriteString("-A " + accessChain + " -j DROP\n")
	for id := range parsed.epochs {
		if _, ok := wanted[id]; ok {
			continue
		}
		for _, d := range []string{"F", "R"} {
			c := relationChain(id, d)
			fmt.Fprintf(&body, "-F %s\n-X %s\n", c, c)
		}
	}
	body.WriteString("COMMIT\n")
	if err := m.commands.Restore(ctx, []byte(body.String())); err != nil {
		return ApplyResult{}, err
	}
	m.forwardPlan = ForwardPlan{Allows: slices.Clone(plan.Allows), Decisions: slices.Clone(plan.Decisions)}
	m.forwardReady = true
	m.bindings = wanted
	return ApplyResult{Changed: true}, nil
}
