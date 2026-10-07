//go:build linux

package vpn

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
)

var savedCounterLine = regexp.MustCompile(`^\[(\d+):(\d+)\] -A (\S+) .*--comment "?cibacct:([0-9a-f]{16}):([0-9a-f]{16}):([FR]):([NT])"?(?:\s|$)`)

type parsedKernelCounters struct {
	snapshot flowacct.CounterSnapshot
	epochs   map[string]string
}

func parseKernelCounters(saved []byte, bindings map[string]ForwardRule, tolerateMissing ...bool) (parsedKernelCounters, error) {
	result := parsedKernelCounters{snapshot: flowacct.CounterSnapshot{At: time.Now()}, epochs: map[string]string{}}
	rows := map[string]*flowacct.PairCounters{}
	seen := map[string]map[string]bool{}
	for _, line := range strings.Split(string(saved), "\n") {
		if !strings.Contains(line, "cibacct:") {
			continue
		}
		match := savedCounterLine.FindStringSubmatch(line)
		if len(match) != 8 {
			return result, fmt.Errorf("malformed owned VPN counter")
		}
		packets, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil {
			return result, err
		}
		bytes, err := strconv.ParseUint(match[2], 10, 64)
		if err != nil {
			return result, err
		}
		chain, id, epoch, direction, kind := match[3], match[4], match[5], match[6], match[7]
		if chain != relationChain(id, direction) {
			return result, fmt.Errorf("VPN counter chain identity mismatch")
		}
		if old := result.epochs[id]; old != "" && old != epoch {
			return result, fmt.Errorf("mixed VPN counter epochs")
		}
		result.epochs[id] = epoch
		if seen[id] == nil {
			seen[id] = map[string]bool{}
		}
		key := direction + kind
		if seen[id][key] {
			return result, fmt.Errorf("duplicate owned VPN counter")
		}
		seen[id][key] = true
		binding, ok := bindings[id]
		if !ok {
			result.snapshot.Partial = true
			continue
		}
		row := rows[id]
		if row == nil {
			row = &flowacct.PairCounters{Key: flowacct.Key{Subject: binding.ClientName, Lab: binding.LabName}, BindingID: id, Epoch: epoch, ClientCIDR: binding.ClientCIDR}
			rows[id] = row
		}
		switch key {
		case "FT":
			row.PacketsOut = packets
			row.BytesOut = bytes
		case "RT":
			row.PacketsIn = packets
			row.BytesIn = bytes
		case "FN":
			row.Attempts = packets
		case "RN":
			row.LabInitiatedAttempts = packets
		}
	}
	for id, kinds := range seen {
		if len(kinds) != 4 {
			return result, fmt.Errorf("incomplete VPN counter binding %s", id)
		}
	}
	for id := range bindings {
		if len(seen[id]) != 4 {
			if len(tolerateMissing) > 0 && tolerateMissing[0] {
				result.snapshot.Partial = true
				continue
			}
			return result, fmt.Errorf("missing VPN counter binding %s", id)
		}
	}
	for _, row := range rows {
		result.snapshot.Rows = append(result.snapshot.Rows, *row)
	}
	sort.Slice(result.snapshot.Rows, func(i, j int) bool { return result.snapshot.Rows[i].BindingID < result.snapshot.Rows[j].BindingID })
	return result, nil
}

func (m *IPTablesManager) ReadPairCounters(ctx context.Context) (flowacct.CounterSnapshot, error) {
	m.forwardMu.Lock()
	defer m.forwardMu.Unlock()
	saved, err := m.commands.Save(ctx)
	if err != nil {
		return flowacct.CounterSnapshot{}, err
	}
	result, err := parseKernelCounters(saved, m.bindings)
	return result.snapshot, err
}

// RestoreCounterBindings attaches private persisted identities to surviving
// native chains before the first policy reconcile. Endpoints are not needed
// to read an already identified cumulative counter.
func (m *IPTablesManager) RestoreCounterBindings(rows []flowacct.PairCounters) {
	m.forwardMu.Lock()
	defer m.forwardMu.Unlock()
	m.bindings = map[string]ForwardRule{}
	for _, row := range rows {
		m.bindings[row.BindingID] = ForwardRule{ClientName: row.Subject, LabName: row.Lab, BindingID: row.BindingID}
	}
}

// WithPairCounters holds the same lock as retirement until its snapshot has
// been folded, so an older sample cannot arrive after a binding was forgotten.
func (m *IPTablesManager) WithPairCounters(ctx context.Context, observe func(flowacct.CounterSnapshot) error) error {
	m.forwardMu.Lock()
	defer m.forwardMu.Unlock()
	saved, err := m.commands.Save(ctx)
	if err != nil {
		return err
	}
	parsed, err := parseKernelCounters(saved, m.bindings)
	if err != nil {
		return err
	}
	return observe(parsed.snapshot)
}
func (m *IPTablesManager) KnownBindingIDs() map[string]bool {
	m.forwardMu.Lock()
	defer m.forwardMu.Unlock()
	ids := map[string]bool{}
	for id := range m.bindings {
		ids[id] = true
	}
	return ids
}
func (m *IPTablesManager) Quiesce(ctx context.Context) error {
	m.forwardMu.Lock()
	defer m.forwardMu.Unlock()
	m.quiesced = true
	return m.commands.Restore(ctx, []byte("*filter\n-F "+accessChain+"\n-A "+accessChain+" -j DROP\nCOMMIT\n"))
}
