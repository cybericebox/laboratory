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
const nftTable = "cicevpn"

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

var accountingSets = []struct{ name, direction, kind string }{
	{"acct_fwd", "F", "T"}, {"acct_rev", "R", "T"}, {"new_fwd", "F", "N"}, {"new_rev", "R", "N"},
}

func (m *IPTablesManager) nftBaseScript() string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table ip %s\n", nftTable)
	for _, set := range []string{"allow_fwd", "allow_rev"} {
		fmt.Fprintf(&b, "add set ip %s %s { type ifname . ipv4_addr . ipv4_addr; flags interval; }\nflush set ip %s %s\n", nftTable, set, nftTable, set)
	}
	for _, s := range accountingSets {
		fmt.Fprintf(&b, "add set ip %s %s { type ifname . ipv4_addr . ipv4_addr; flags interval; counter; }\n", nftTable, s.name)
	}
	for _, set := range []string{"labnets", "clients"} {
		fmt.Fprintf(&b, "add set ip %s %s { type ipv4_addr; flags interval; }\nflush set ip %s %s\n", nftTable, set, nftTable, set)
	}
	for _, chain := range []string{"count_fwd", "count_rev"} {
		fmt.Fprintf(&b, "add chain ip %s %s\nflush chain ip %s %s\n", nftTable, chain, nftTable, chain)
	}
	fmt.Fprintf(&b, "add rule ip %s count_fwd tcp flags & (syn | ack) == syn oifname . ip saddr . ip daddr @new_fwd\n", nftTable)
	fmt.Fprintf(&b, "add rule ip %s count_fwd oifname . ip saddr . ip daddr @acct_fwd accept\n", nftTable)
	fmt.Fprintf(&b, "add rule ip %s count_rev tcp flags & (syn | ack) == syn iifname . ip saddr . ip daddr @new_rev\n", nftTable)
	fmt.Fprintf(&b, "add rule ip %s count_rev iifname . ip saddr . ip daddr @acct_rev accept\n", nftTable)
	// raw hook: the traffic between the participants and the labs is not tracked; what is addressed to the pod itself (its status page, its ping) is
	fmt.Fprintf(&b, "add chain ip %s prerouting { type filter hook prerouting priority -300; policy accept; }\nflush chain ip %s prerouting\n", nftTable, nftTable)
	fmt.Fprintf(&b, "add rule ip %s prerouting iifname %q ip daddr @labnets notrack\n", nftTable, m.wgIface)
	fmt.Fprintf(&b, "add rule ip %s prerouting iifname \"lab*\" ip daddr @clients notrack\n", nftTable)
	// the gate: closed by policy; participants never reach each other through the pod
	fmt.Fprintf(&b, "add chain ip %s forward { type filter hook forward priority 0; policy drop; }\nflush chain ip %s forward\n", nftTable, nftTable)
	fmt.Fprintf(&b, "add rule ip %s forward iifname %q oifname %q drop\n", nftTable, m.wgIface, m.wgIface)
	fmt.Fprintf(&b, "add rule ip %s forward iifname %q oifname . ip saddr . ip daddr @allow_fwd jump count_fwd\n", nftTable, m.wgIface)
	fmt.Fprintf(&b, "add rule ip %s forward oifname %q iifname . ip saddr . ip daddr @allow_rev jump count_rev\n", nftTable, m.wgIface)
	return b.String()
}

// nftSetupForward closes the gate (the permission sets are emptied) and installs the rules; the accounting sets survive, as the relation
// chains did, so the counters of a surviving network namespace are not lost. Nothing is forwarded by the iptables FORWARD chain any more:
// it accepts, and the nft gate decides.
func (m *IPTablesManager) nftSetupForward() error {
	if err := m.nft.Apply(context.Background(), []byte(m.nftBaseScript())); err != nil {
		return err
	}
	if err := m.ipt.ClearChain("filter", "FORWARD"); err != nil {
		return err
	}
	return m.ipt.ChangePolicy("filter", "FORWARD", "ACCEPT")
}

// nftElementKeys are the element keys of a binding in the permission and accounting sets.
func nftKeys(r ForwardRule) (fwd, rev string) {
	return fmt.Sprintf("%q . %s . %s", r.LabInterface, strings.TrimSuffix(r.ClientCIDR, "/32"), r.LabCIDR),
		fmt.Sprintf("%q . %s . %s", r.LabInterface, r.LabCIDR, strings.TrimSuffix(r.ClientCIDR, "/32"))
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
	st := nftState{snapshot: flowacct.CounterSnapshot{At: time.Now()}, epochs: map[string]string{}, keys: map[string]map[string][]string{}}
	if len(bytes.TrimSpace(list)) == 0 {
		for id := range bindings {
			if len(tolerateMissing) == 0 || !tolerateMissing[0] {
				return st, fmt.Errorf("missing VPN counter binding %s", id)
			}
			st.snapshot.Partial = true
		}
		return st, nil
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
		return st, fmt.Errorf("parse nft list: %w", err)
	}
	rows := map[string]*flowacct.PairCounters{}
	seen := map[string]map[string]bool{}
	for _, item := range doc.Nftables {
		if item.Set == nil {
			continue
		}
		var dir, kind string
		for _, s := range accountingSets {
			if s.name == item.Set.Name {
				dir, kind = s.direction, s.kind
			}
		}
		if dir == "" {
			continue
		}
		for _, rawElem := range item.Set.Elem {
			var wrapper struct {
				Elem nftElem `json:"elem"`
			}
			if err := json.Unmarshal(rawElem, &wrapper); err != nil || len(wrapper.Elem.Val) == 0 {
				return st, fmt.Errorf("malformed owned VPN counter")
			}
			e := wrapper.Elem
			match := nftComment.FindStringSubmatch(e.Comment)
			if match == nil {
				return st, fmt.Errorf("malformed owned VPN counter")
			}
			id, epoch := match[1], match[2]
			if old := st.epochs[id]; old != "" && old != epoch {
				return st, fmt.Errorf("mixed VPN counter epochs")
			}
			st.epochs[id] = epoch
			key, err := nftConcatKey(e.Val)
			if err != nil {
				return st, err
			}
			if st.keys[id] == nil {
				st.keys[id] = map[string][]string{}
			}
			st.keys[id][item.Set.Name] = append(st.keys[id][item.Set.Name], key)
			if seen[id] == nil {
				seen[id] = map[string]bool{}
			}
			k := dir + kind
			if seen[id][k] {
				return st, fmt.Errorf("duplicate owned VPN counter")
			}
			seen[id][k] = true
			binding, ok := bindings[id]
			if !ok {
				st.snapshot.Partial = true
				continue
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
		}
	}
	for id, kinds := range seen {
		if len(kinds) != 4 {
			return st, fmt.Errorf("incomplete VPN counter binding %s", id)
		}
	}
	for id := range bindings {
		if len(seen[id]) != 4 {
			if len(tolerateMissing) > 0 && tolerateMissing[0] {
				st.snapshot.Partial = true
				continue
			}
			return st, fmt.Errorf("missing VPN counter binding %s", id)
		}
	}
	for _, row := range rows {
		st.snapshot.Rows = append(st.snapshot.Rows, *row)
	}
	sort.Slice(st.snapshot.Rows, func(i, j int) bool { return st.snapshot.Rows[i].BindingID < st.snapshot.Rows[j].BindingID })
	return st, nil
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
	var b strings.Builder
	fmt.Fprintf(&b, "flush set ip %s allow_fwd\nflush set ip %s allow_rev\nflush set ip %s labnets\nflush set ip %s clients\n", nftTable, nftTable, nftTable, nftTable)
	labnets, clients := map[string]bool{}, map[string]bool{}
	for _, r := range plan.Allows {
		fwd, rev := nftKeys(r)
		fmt.Fprintf(&b, "add element ip %s allow_fwd { %s }\nadd element ip %s allow_rev { %s }\n", nftTable, fwd, nftTable, rev)
		if !labnets[r.LabCIDR] {
			labnets[r.LabCIDR] = true
			fmt.Fprintf(&b, "add element ip %s labnets { %s }\n", nftTable, r.LabCIDR)
		}
		client := strings.TrimSuffix(r.ClientCIDR, "/32")
		if !clients[client] {
			clients[client] = true
			fmt.Fprintf(&b, "add element ip %s clients { %s }\n", nftTable, client)
		}
		if state.epochs[r.BindingID] == "" {
			var random [8]byte
			if _, err := rand.Read(random[:]); err != nil {
				return ApplyResult{}, err
			}
			comment := "cibacct:" + r.BindingID + ":" + hex.EncodeToString(random[:])
			for _, s := range accountingSets {
				key := fwd
				if s.direction == "R" {
					key = rev
				}
				fmt.Fprintf(&b, "add element ip %s %s { %s counter comment %q }\n", nftTable, s.name, key, comment)
			}
		}
	}
	if err := m.nft.Apply(ctx, []byte(b.String())); err != nil {
		return ApplyResult{}, err
	}
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
	return m.nft.Apply(ctx, []byte(fmt.Sprintf("flush set ip %s allow_fwd\nflush set ip %s allow_rev\n", nftTable, nftTable)))
}

func (m *IPTablesManager) nftCleanup() {
	_ = m.nft.Apply(context.Background(), []byte(fmt.Sprintf("add table ip %s\ndelete table ip %s\n", nftTable, nftTable)))
	m.forwardMu.Lock()
	m.forwardReady = false
	m.bindings = nil
	m.forwardMu.Unlock()
}
