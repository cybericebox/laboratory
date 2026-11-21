//go:build linux

package vpn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
)

// The nftables forwarding of the VPN pod (VPN_FIREWALL=nft). The pod forwards between the WireGuard interface and the lab
// interfaces WITHOUT connection tracking: the traffic of the pairs is marked notrack in a raw hook, every permitted pair is an element
// of a hash/interval set, and a packet is matched in each direction by one lookup of (lab interface, source, destination), however many
// pairs there are. Access is symmetric and stateless (a pair is allowed in both directions at once; a reverse shell from the lab to the
// participant is allowed), a revoked pair stops with the next packet because its element is gone, and nothing has to be cleaned out of
// a conntrack table.
//
// Accounting is the counters of the set elements, kept apart from the permission:
//
//	allow_fwd / allow_rev   permission: elements (lab interface . client . lab prefix) in one direction and (lab interface . lab prefix . client)
//	                        in the other; replaced in ONE transaction by every plan
//	acct_fwd / acct_rev     the same keys with a counter each (packets, bytes), created with the pair and deleted only after the pair was retired
//	new_fwd / new_rev       the same keys with a counter of the first packets of TCP connections (SYN without ACK): the attempts of the
//	                        participant, and of the lab in the reverse direction. A UDP or ICMP "attempt" is not counted (no state to tell it).
//
// A packet is counted only when the permission set let it pass; once a pair leaves the permission sets its counters stand still, so a retired
// pair is read exactly and only then deleted (the same two steps as the iptables relation chains: close, read, delete). Every element of
// the accounting sets carries the comment "cibacct:<binding>:<epoch>", the identity that survives a restart of the VPN process.
// NFTCommand is the nft binary (a stand-in in the tests).
type NFTCommand interface {
	// Apply runs a batch (one transaction).
	Apply(ctx context.Context, script []byte) error
	// List returns the JSON of the whole table, or an empty answer when the table does not exist.
	List(ctx context.Context) ([]byte, error)
}

type nativeNFT struct{}

func (nativeNFT) Apply(ctx context.Context, script []byte) error {
	c := exec.CommandContext(ctx, "nft", "-f", "-")
	c.Stdin = bytes.NewReader(script)
	if b, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("apply VPN nft batch: %w: %s", err, b)
	}
	return nil
}

func (nativeNFT) List(ctx context.Context) ([]byte, error) {
	c := exec.CommandContext(ctx, "nft", "-j", "list", "table", "ip", nftTable)
	b, err := c.CombinedOutput()
	if err != nil {
		if strings.Contains(string(b), "No such file or directory") {
			return nil, nil
		}
		return nil, fmt.Errorf("list VPN nft table: %w: %s", err, b)
	}
	return b, nil
}

// UseNFT switches the forwarding of the pod to nftables.
func (m *IPTablesManager) UseNFT(c NFTCommand) { m.nft = c }

func (m *IPTablesManager) nftOn() bool { return m.nft != nil }

// nftSetupForward installs the nft gate and then lets the iptables FORWARD chain trust it. The order never leaves a window: FORWARD is DROP
// from the first moment (ProtectInput sets it and this keeps it), the nft gate is installed closed (the permission sets are emptied; the
// accounting sets survive, as the relation chains did, so the counters of a surviving network namespace are not lost), and only then
// FORWARD gets its one rule: accept what the gate marked.
func (m *IPTablesManager) nftSetupForward() error {
	if err := m.ipt.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
		return err
	}
	if err := m.ipt.ClearChain("filter", "FORWARD"); err != nil {
		return err
	}
	if err := m.nft.Apply(context.Background(), []byte(nftBaseScript(m.wgIface))); err != nil {
		return err
	}
	return m.ipt.AppendUnique("filter", "FORWARD", "-m", "mark", "--mark", nftAcceptMark+"/"+nftAcceptMark, "-j", "ACCEPT")
}

type nftState struct {
	snapshot flowacct.CounterSnapshot
	epochs   map[string]string
	// elements per binding for the deletion of a retired one: set name -> keys
	keys map[string]map[string][]string
}

type nftElem struct {
	Val     json.RawMessage `json:"val"`
	Comment string          `json:"comment"`
	Counter struct {
		Packets uint64 `json:"packets"`
		Bytes   uint64 `json:"bytes"`
	} `json:"counter"`
}

var nftComment = regexp.MustCompile(`^cibacct:([0-9a-f]{16}):([0-9a-f]{16})$`)

// nftConcatKey renders the "val" of an element ({"concat": ["lab1", "10.8.0.5", {"prefix": {...}}]}) as nft writes a key.
func nftConcatKey(raw json.RawMessage) (string, error) {
	var v struct {
		Concat []json.RawMessage `json:"concat"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Concat) == 0 {
		return "", fmt.Errorf("unexpected nft element key")
	}
	parts := make([]string, 0, len(v.Concat))
	for i, p := range v.Concat {
		var s string
		if json.Unmarshal(p, &s) == nil {
			if i == 0 {
				s = strconv.Quote(s)
			}
			parts = append(parts, s)
			continue
		}
		var pre struct {
			Prefix struct {
				Addr string `json:"addr"`
				Len  int    `json:"len"`
			} `json:"prefix"`
		}
		if json.Unmarshal(p, &pre) != nil || pre.Prefix.Addr == "" {
			return "", fmt.Errorf("unexpected nft element key part")
		}
		parts = append(parts, fmt.Sprintf("%s/%d", pre.Prefix.Addr, pre.Prefix.Len))
	}
	return strings.Join(parts, " . "), nil
}

// parseNFTCounters reads the accounting sets of the table in the shape the iptables backend returns: one row per binding with the
// totals of both directions and the counts of the attempts, the epoch of every binding, and the element keys to delete when it is retired.
func parseNFTCounters(list []byte, bindings map[string]ForwardRule, tolerateMissing ...bool) (nftState, error) {
	tolerate := len(tolerateMissing) > 0 && tolerateMissing[0]
	st := nftState{snapshot: flowacct.CounterSnapshot{At: time.Now()}, epochs: map[string]string{}, keys: map[string]map[string][]string{}}
	rows := map[string]*flowacct.PairCounters{}
	seen := map[string]map[string]bool{}
	if len(bytes.TrimSpace(list)) != 0 {
		var doc struct {
			Nftables []struct {
				Set *struct {
					Name string            `json:"name"`
					Elem []json.RawMessage `json:"elem"`
				} `json:"set"`
			} `json:"nftables"`
		}
		if err := json.Unmarshal(list, &doc); err != nil {
			return st, fmt.Errorf("parse nft list: %w", err)
		}
		for _, item := range doc.Nftables {
			if item.Set == nil {
				continue
			}
			for _, rawElem := range item.Set.Elem {
				if err := st.fold(item.Set.Name, rawElem, bindings, rows, seen); err != nil {
					return st, err
				}
			}
		}
	}
	for id, kinds := range seen {
		if len(kinds) != 4 {
			return st, fmt.Errorf("incomplete VPN counter binding %s", id)
		}
	}
	for id := range bindings {
		if len(seen[id]) != 4 {
			if !tolerate {
				return st, fmt.Errorf("missing VPN counter binding %s", id)
			}
			st.snapshot.Partial = true
		}
	}
	for _, row := range rows {
		st.snapshot.Rows = append(st.snapshot.Rows, *row)
	}
	sort.Slice(st.snapshot.Rows, func(i, j int) bool { return st.snapshot.Rows[i].BindingID < st.snapshot.Rows[j].BindingID })
	return st, nil
}

// fold reads one element of an accounting set into the state.
func (st *nftState) fold(set string, rawElem json.RawMessage, bindings map[string]ForwardRule, rows map[string]*flowacct.PairCounters, seen map[string]map[string]bool) error {
	var dir, kind string
	for _, s := range accountingSets {
		if s.name == set {
			dir, kind = s.direction, s.kind
		}
	}
	if dir == "" {
		return nil // a permission set or a set of the notrack hook
	}
	var wrapper struct {
		Elem nftElem `json:"elem"`
	}
	if err := json.Unmarshal(rawElem, &wrapper); err != nil || len(wrapper.Elem.Val) == 0 {
		return fmt.Errorf("malformed owned VPN counter")
	}
	e := wrapper.Elem
	match := nftComment.FindStringSubmatch(e.Comment)
	if match == nil {
		return fmt.Errorf("malformed owned VPN counter")
	}
	id, epoch := match[1], match[2]
	if old := st.epochs[id]; old != "" && old != epoch {
		return fmt.Errorf("mixed VPN counter epochs")
	}
	st.epochs[id] = epoch
	key, err := nftConcatKey(e.Val)
	if err != nil {
		return err
	}
	if st.keys[id] == nil {
		st.keys[id] = map[string][]string{}
	}
	st.keys[id][set] = append(st.keys[id][set], key)
	if seen[id] == nil {
		seen[id] = map[string]bool{}
	}
	k := dir + kind
	if seen[id][k] {
		return fmt.Errorf("duplicate owned VPN counter")
	}
	seen[id][k] = true
	binding, ok := bindings[id]
	if !ok {
		st.snapshot.Partial = true
		return nil
	}
	row := rows[id]
	if row == nil {
		row = &flowacct.PairCounters{Key: flowacct.Key{Subject: binding.ClientName, Lab: binding.LabName}, BindingID: id, Epoch: epoch, ClientCIDR: binding.ClientCIDR}
		rows[id] = row
	}
	switch k {
	case "FT":
		row.PacketsOut, row.BytesOut = e.Counter.Packets, e.Counter.Bytes
	case "RT":
		row.PacketsIn, row.BytesIn = e.Counter.Packets, e.Counter.Bytes
	case "FN":
		row.Attempts = e.Counter.Packets
	case "RN":
		row.LabInitiatedAttempts = e.Counter.Packets
	}
	return nil
}

func (m *IPTablesManager) nftRead(ctx context.Context, bindings map[string]ForwardRule, tolerate ...bool) (nftState, error) {
	list, err := m.nft.List(ctx)
	if err != nil {
		return nftState{}, err
	}
	return parseNFTCounters(list, bindings, tolerate...)
}

// nftApplyForwardPlan is ApplyForwardPlan for the nft backend: the permission sets are replaced in one transaction (the new pairs are
// accounted from the same moment), then the pairs that left the plan are read and deleted from the accounting sets.
func (m *IPTablesManager) nftApplyForwardPlan(ctx context.Context, plan ForwardPlan) (ApplyResult, error) {
	m.forwardMu.Lock()
	defer m.forwardMu.Unlock()
	if m.quiesced {
		return ApplyResult{}, fmt.Errorf("VPN forwarding is quiesced for shutdown")
	}
	if m.forwardReady && !m.retirePending && slices.Equal(m.forwardPlan.Allows, plan.Allows) {
		return ApplyResult{}, nil
	}
	if err := validateForwardPlan(plan); err != nil {
		return ApplyResult{}, err
	}
	state, err := m.nftRead(ctx, m.bindings, true)
	if err != nil {
		return ApplyResult{}, err
	}
	wanted := map[string]ForwardRule{}
	for _, r := range plan.Allows {
		wanted[r.BindingID] = r
	}
	drops, err := m.nftDropCounters(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	script, err := nftPlanScript(plan, func(id string) bool { return state.epochs[id] != "" }, func() (string, error) {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		return hex.EncodeToString(random[:]), nil
	})
	if err != nil {
		return ApplyResult{}, err
	}
	if err := m.nft.Apply(ctx, []byte(script)); err != nil {
		return ApplyResult{}, err
	}
	m.foldDrops(drops) // the set was emptied and filled again: its counters start again
	m.forwardPlan = ForwardPlan{Allows: slices.Clone(plan.Allows), Decisions: slices.Clone(plan.Decisions)}
	m.forwardReady = true
	// The gate is closed for the pairs that left the plan: their counters stand still. Keep them until a fresh read and the report write
	// have succeeded; otherwise retry.
	m.retirePending = true
	for id, r := range wanted {
		if m.bindings == nil {
			m.bindings = map[string]ForwardRule{}
		}
		m.bindings[id] = r
	}
	return m.nftRetire(ctx, wanted)
}

func (m *IPTablesManager) nftRetire(ctx context.Context, wanted map[string]ForwardRule) (ApplyResult, error) {
	final, err := m.nftRead(ctx, m.bindings, true)
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
	sort.Strings(ids)
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
		for _, id := range ids {
			for set, keys := range final.keys[id] {
				for _, key := range keys {
					fmt.Fprintf(&cleanup, "delete element ip %s %s { %s }\n", nftTable, set, key)
				}
			}
		}
		if err := m.nft.Apply(ctx, []byte(cleanup.String())); err != nil {
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

func (m *IPTablesManager) nftQuiesce(ctx context.Context) error {
	m.forwardMu.Lock()
	defer m.forwardMu.Unlock()
	m.quiesced = true
	return m.nft.Apply(ctx, []byte(fmt.Sprintf("flush set ip %s allow_fwd\nflush set ip %s allow_rev\nflush set ip %s reply_rev\n", nftTable, nftTable, nftTable)))
}

func (m *IPTablesManager) nftCleanup() {
	// FORWARD stops trusting the gate before the gate goes: the policy is DROP and nothing is accepted
	_ = m.ipt.ClearChain("filter", "FORWARD")
	_ = m.ipt.ChangePolicy("filter", "FORWARD", "DROP")
	_ = m.nft.Apply(context.Background(), []byte(fmt.Sprintf("add table ip %s\ndelete table ip %s\n", nftTable, nftTable)))
	m.forwardMu.Lock()
	m.forwardReady = false
	m.bindings = nil
	m.forwardMu.Unlock()
}

// nftDropCounters reads the packet counts of the elements of blocked_labs, by lab interface.
func (m *IPTablesManager) nftDropCounters(ctx context.Context) (map[string]uint64, error) {
	list, err := m.nft.List(ctx)
	if err != nil {
		return nil, err
	}
	return parseNFTDrops(list)
}

func parseNFTDrops(list []byte) (map[string]uint64, error) {
	out := map[string]uint64{}
	if len(bytes.TrimSpace(list)) == 0 {
		return out, nil
	}
	var doc struct {
		Nftables []struct {
			Set *struct {
				Name string            `json:"name"`
				Elem []json.RawMessage `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(list, &doc); err != nil {
		return nil, fmt.Errorf("parse nft list: %w", err)
	}
	for _, item := range doc.Nftables {
		if item.Set == nil || item.Set.Name != "blocked_labs" {
			continue
		}
		for _, raw := range item.Set.Elem {
			var w struct {
				Elem nftElem `json:"elem"`
			}
			var iface string
			if json.Unmarshal(raw, &w) != nil || json.Unmarshal(w.Elem.Val, &iface) != nil || iface == "" {
				continue
			}
			out[iface] += w.Elem.Counter.Packets
		}
	}
	return out, nil
}
