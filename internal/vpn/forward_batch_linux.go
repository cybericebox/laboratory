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
	if m.quiesced {
		return ApplyResult{}, fmt.Errorf("VPN forwarding is quiesced for shutdown")
	}
	if m.forwardReady && !m.retirePending && slices.Equal(m.forwardPlan.Allows, plan.Allows) {
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
	parsed, err := parseKernelCounters(saved, m.bindings, true)
	if err != nil {
		return ApplyResult{}, err
	}
	wanted := map[string]ForwardRule{}
	for _, r := range plan.Allows {
		wanted[r.BindingID] = r
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
	body.WriteString("COMMIT\n")
	if err := m.commands.Restore(ctx, []byte(body.String())); err != nil {
		return ApplyResult{}, err
	}
	m.forwardPlan = ForwardPlan{Allows: slices.Clone(plan.Allows), Decisions: slices.Clone(plan.Decisions)}
	m.forwardReady = true
	// The gate is now closed for retired pairs. Keep their detached chains
	// until a fresh read and the report write have succeeded; otherwise retry.
	m.retirePending = true
	for id, r := range wanted {
		if m.bindings == nil {
			m.bindings = map[string]ForwardRule{}
		}
		m.bindings[id] = r
	}
	finalSaved, err := m.commands.Save(ctx)
	if err != nil {
		return ApplyResult{Changed: true}, err
	}
	final, err := parseKernelCounters(finalSaved, m.bindings, true)
	if err != nil {
		return ApplyResult{Changed: true}, err
	}
	retired := flowacct.CounterSnapshot{At: final.snapshot.At, Partial: final.snapshot.Partial}
	ids := []string{}
	for id := range final.epochs {
		if _, ok := wanted[id]; !ok {
			ids = append(ids, id)
		}
	}
	for _, row := range final.snapshot.Rows {
		if _, ok := wanted[row.BindingID]; !ok {
			retired.Rows = append(retired.Rows, row)
		}
	}
	if (len(ids) > 0 || retired.Partial) && m.BeforeRetire != nil {
		if err := m.BeforeRetire(retired); err != nil {
			return ApplyResult{Changed: true}, fmt.Errorf("capture retired traffic: %w", err)
		}
	}
	if len(ids) > 0 {
		var cleanup strings.Builder
		cleanup.WriteString("*filter\n")
		for _, id := range ids {
			for _, d := range []string{"F", "R"} {
				chain := relationChain(id, d)
				fmt.Fprintf(&cleanup, "-F %s\n-X %s\n", chain, chain)
			}
		}
		cleanup.WriteString("COMMIT\n")
		if err := m.commands.Restore(ctx, []byte(cleanup.String())); err != nil {
			return ApplyResult{Changed: true}, err
		}
		if m.AfterRetire != nil {
			m.AfterRetire(ids)
		}
	}
	forgot := []string{}
	for id := range m.bindings {
		if _, ok := wanted[id]; !ok {
			forgot = append(forgot, id)
		}
	}
	if len(forgot) > 0 && m.AfterRetire != nil {
		m.AfterRetire(forgot)
	}
	m.bindings = wanted
	m.retirePending = false
	return ApplyResult{Changed: true}, nil
}
