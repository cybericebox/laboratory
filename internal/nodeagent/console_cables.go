package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/consolespec"
	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/portkeeper"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

// The console cables of this node (docs/internal-docs/laboratory/specs/console-ports-design.md, 6.5). The node-agent drives the lines;
// the device access service of the node moves their bytes. Every pass the node-agent puts the whole set of lines of its node on
// cable.sock (wire.LinesRequest):
//
//   - a cable whose console end runs on this node (Device.status.nodeName) is owned here: the service attaches the console end and the
//     serial end too when it runs here, else dials the device access pod of the serial end's node (its pod IP, read from the pod cache);
//   - a cable whose serial end only runs here is given with the serial role, so the service accepts the owner's stream for it and for
//     nothing else.
//
// The owner's node-agent writes ConsoleCable.status (both nodes, both attaches, lineUp, the Ready condition with the reasons
// WaitingForDevice, PortDown, PeerUnreachable); the operator sums them into the Lab status. A cable whose console end moved to another
// node is written down once by the old owner (unless the new one has already written it or the operator has moved the stamp), so a
// status never claims a line that nobody runs. A node-agent writes only cables whose annotation names.AnnotationConsoleNode, which the
// operator stamps from the console device, names its node (an admission policy enforces it). The lines go in the order of the cables'
// creation; a line the service refuses because the node is full gets the reason NodeLineLimit. Nothing here waits for a line: a device is
// Ready without its lines.

// Codes of the annotation names.AnnotationConsoleRelay. The management agent reports console cables only when every lab node says ready.
const (
	CodeRelayUnreachable = "CONSOLE_RELAY_UNREACHABLE"
	CodeRelayOld         = "CONSOLE_RELAY_OUTDATED"
	CodeRelayDisabled    = "CONSOLE_RELAY_DISABLED"
)

// RelayCodeOf is the code of a value of names.AnnotationConsoleRelay that is not ready: one of the relay codes, CodeRelayDisabled for
// "disabled", and CodeNodeUnconfirmed for anything else (no value, an older node-agent). "" for ready.
func RelayCodeOf(v string, present bool) string {
	switch {
	case !present:
		return CodeNodeUnconfirmed
	case v == PreflightReady:
		return ""
	case v == PreflightDisabled:
		return CodeRelayDisabled
	}
	if r, ok := strings.CutPrefix(v, preflightUnavailable); ok {
		code, _, _ := strings.Cut(r, ": ")
		if code == CodeRelayUnreachable || code == CodeRelayOld {
			return code
		}
	}
	return CodeNodeUnconfirmed
}

// PatchNodeAnnotation puts one annotation on the node now (a merge patch of that key only).
func PatchNodeAnnotation(ctx context.Context, c client.Client, node, key, value string) error {
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{key: value}}})
	return c.Patch(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}}, client.RawPatch(types.MergePatchType, body))
}

// consoleCableReady is the reason of the Ready condition of a line that is up.
const consoleCableReady = "LineUp"

// deviceAccessApp is the app label of the device access pods (the chart's laboratory.deviceAccess.selectorLabels).
const deviceAccessApp = "laboratory-device-access"

// CableClient is the client side of cable.sock.
type CableClient interface {
	PutLines(ctx context.Context, req wire.LinesRequest) (wire.LinesReply, error)
}

// unixCableClient speaks HTTP over the unix socket.
type unixCableClient struct{ c *http.Client }

// NewCableClient returns the client of the cable.sock at path.
func NewCableClient(path string) CableClient {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}, MaxIdleConns: 1, IdleConnTimeout: 30 * time.Second}
	return unixCableClient{c: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

func (u unixCableClient) PutLines(ctx context.Context, req wire.LinesRequest) (wire.LinesReply, error) {
	body, _ := json.Marshal(req)
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://cable"+wire.LinesPath, bytes.NewReader(body))
	if err != nil {
		return wire.LinesReply{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := u.c.Do(r)
	if err != nil {
		return wire.LinesReply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var reply wire.LinesReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&reply); err != nil {
		return wire.LinesReply{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return reply, fmt.Errorf("cable.sock answered %d %s", resp.StatusCode, reply.Error)
	}
	return reply, nil
}

// ConsoleCableSync is the loop of the console cables of this node.
type ConsoleCableSync struct {
	Client   client.Client
	NodeName string
	Cable    CableClient
	// DeviceAccessNamespace and CablePort say where the device access pods of the other nodes listen for the cable stream.
	DeviceAccessNamespace string
	CablePort             int
	// Interval is the period of a pass; zero means one second.
	Interval time.Duration
	// Relay, when set, is told the state of the relay of this node: the value of the annotation names.AnnotationConsoleRelay.
	Relay func(value string)
	// RelayKey, when set, is told the public key of the device access process of this node: the value of the annotation
	// names.AnnotationConsoleRelayKey, which the peers verify its signatures with.
	RelayKey func(value string)

	mu sync.Mutex
	// owned are the cables whose status this node-agent wrote as their owner.
	owned map[types.NamespacedName]bool
}

// NeedLeaderElection: every node-agent drives the lines of its own node.
func (s *ConsoleCableSync) NeedLeaderElection() bool { return false }

// Start runs the passes until ctx ends.
func (s *ConsoleCableSync) Start(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastErr string
	for {
		if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				ctrl.Log.WithName("console-cables").Error(err, "console cables pass")
			}
		} else {
			lastErr = ""
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// cableView is what one pass knows of one cable.
type cableView struct {
	cable                   *laboratoryv1alpha1.ConsoleCable
	serialNode, consoleNode string
	line                    *wire.LineSpec
	// reason is the state of an owned cable that is not given to the service (the serial end has no node or no device access pod).
	reason string
}

// Sync is one pass.
func (s *ConsoleCableSync) Sync(ctx context.Context) error {
	var cables laboratoryv1alpha1.ConsoleCableList
	if err := s.Client.List(ctx, &cables); err != nil {
		return err
	}
	peers, err := s.deviceAccessPods(ctx)
	if err != nil {
		return err
	}
	keys, err := s.relayKeys(ctx)
	if err != nil {
		return err
	}
	devices := map[string]map[string]*laboratoryv1alpha1.Device{} // namespace/lab -> device name -> Device
	var views []cableView
	for i := range cables.Items {
		c := &cables.Items[i]
		if !c.DeletionTimestamp.IsZero() {
			continue
		}
		key := c.Namespace + "/" + c.Spec.LabRef
		if _, ok := devices[key]; !ok {
			m, err := s.labDevices(ctx, c.Namespace, c.Spec.LabRef)
			if err != nil {
				return err
			}
			devices[key] = m
		}
		views = append(views, s.view(c, devices[key], peers, keys))
	}
	req := wire.LinesRequest{Lines: []wire.LineSpec{}}
	for _, v := range views {
		if v.line != nil {
			req.Lines = append(req.Lines, *v.line)
		}
	}
	// the order the service keeps lines in when the node is full: the oldest cable first, then namespace and name
	sort.SliceStable(req.Lines, func(i, j int) bool {
		a, b := req.Lines[i], req.Lines[j]
		if a.Created != b.Created {
			return a.Created < b.Created
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	reply, err := s.Cable.PutLines(ctx, req)
	if err != nil {
		s.relay(Unavailable(CodeRelayUnreachable, "the device access service of this node does not answer on cable.sock"))
		return fmt.Errorf("cable.sock: %w", err)
	}
	if reply.RelayKey != "" && s.RelayKey != nil {
		s.RelayKey(reply.RelayKey)
	}
	if reply.Relay < wire.RelayVersion {
		s.relay(Unavailable(CodeRelayOld, "the device access service of this node speaks relay version "+strconv.Itoa(reply.Relay)))
	} else {
		s.relay(PreflightReady)
	}
	states := map[types.NamespacedName]wire.LineState{}
	for _, st := range reply.Lines {
		states[types.NamespacedName{Namespace: st.Namespace, Name: st.Name}] = st
	}
	return s.writeStatuses(ctx, views, states)
}

func (s *ConsoleCableSync) relay(v string) {
	if s.Relay != nil {
		s.Relay(v)
	}
}

// labDevices are the Devices of a lab by their name in the lab.
func (s *ConsoleCableSync) labDevices(ctx context.Context, namespace, lab string) (map[string]*laboratoryv1alpha1.Device, error) {
	var list laboratoryv1alpha1.DeviceList
	if err := s.Client.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{names.LabelLab: lab}); err != nil {
		return nil, err
	}
	out := make(map[string]*laboratoryv1alpha1.Device, len(list.Items))
	for i := range list.Items {
		d := &list.Items[i]
		if d.Spec.LabRef == lab && d.DeletionTimestamp.IsZero() {
			out[d.Spec.Name] = d
		}
	}
	return out, nil
}

// deviceAccessPods are the addresses of the cable stream of the ready device access pods, by node.
func (s *ConsoleCableSync) deviceAccessPods(ctx context.Context) (map[string]string, error) {
	var pods corev1.PodList
	if err := s.Client.List(ctx, &pods, client.InNamespace(s.DeviceAccessNamespace), client.MatchingLabels{"app": deviceAccessApp}); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" || p.Status.PodIP == "" || p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning || !podReady(p) {
			continue
		}
		out[p.Spec.NodeName] = net.JoinHostPort(p.Status.PodIP, strconv.Itoa(s.CablePort))
	}
	return out, nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// relayKeys are the public keys the node-agents published on their Nodes (names.AnnotationConsoleRelayKey), by node.
func (s *ConsoleCableSync) relayKeys(ctx context.Context) (map[string]string, error) {
	var nodes corev1.NodeList
	if err := s.Client.List(ctx, &nodes); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range nodes.Items {
		if k := nodes.Items[i].Annotations[names.AnnotationConsoleRelayKey]; k != "" {
			out[nodes.Items[i].Name] = k
		}
	}
	return out, nil
}

// labUIDOf is the UID of the Lab that owns the cable.
func labUIDOf(c *laboratoryv1alpha1.ConsoleCable) string {
	for _, o := range c.OwnerReferences {
		if o.Kind == "Lab" && o.Name == c.Spec.LabRef {
			return string(o.UID)
		}
	}
	return ""
}

func lineEnd(d *laboratoryv1alpha1.Device, port string) wire.LineEnd {
	return wire.LineEnd{Device: d.Spec.Name, Port: port, Ports: portkeeper.Marshal(consolespec.KeeperSpec(&d.Spec))}
}

// view decides what this node does for a cable.
func (s *ConsoleCableSync) view(c *laboratoryv1alpha1.ConsoleCable, devs map[string]*laboratoryv1alpha1.Device, peers, keys map[string]string) cableView {
	v := cableView{cable: c}
	serial, console := devs[c.Spec.Serial.Device], devs[c.Spec.Console.Device]
	if serial != nil {
		v.serialNode = serial.Status.NodeName
	}
	if console != nil {
		v.consoleNode = console.Status.NodeName
	}
	labUID := labUIDOf(c)
	if serial == nil || console == nil || labUID == "" {
		if v.consoleNode == s.NodeName {
			v.reason = wire.LineWaitingForDevice
		}
		return v
	}
	spec := wire.LineSpec{Namespace: c.Namespace, Name: c.Name, UID: string(c.UID), Lab: c.Spec.LabRef, LabUID: labUID,
		Serial: lineEnd(serial, c.Spec.Serial.Port), Console: lineEnd(console, c.Spec.Console.Port), Created: c.CreationTimestamp.Unix(),
		Tenant: c.Labels[names.LabelTenant]}
	switch {
	case v.consoleNode == s.NodeName:
		spec.Role = wire.RoleOwner
		switch {
		case v.serialNode == s.NodeName:
		case v.serialNode == "":
			v.reason = wire.LineWaitingForDevice
			return v
		case peers[v.serialNode] == "" || keys[v.serialNode] == "":
			// no ready device access pod there, or its node published no key yet
			v.reason = wire.LinePeerUnreachable
			return v
		default:
			spec.Peer, spec.PeerKey = peers[v.serialNode], keys[v.serialNode]
		}
		v.line = &spec
	case v.serialNode == s.NodeName:
		if v.consoleNode == "" || keys[v.consoleNode] == "" {
			return v
		}
		// the acceptor serves the stream only from the node of the console end, signed with the key that node published
		spec.Role, spec.ConsoleNode, spec.ConsoleNodeKey = wire.RoleSerial, v.consoleNode, keys[v.consoleNode]
		v.line = &spec
	}
	return v
}

// writeStatuses writes the status of the cables this node owns, and the last status of the cables it no longer owns.
func (s *ConsoleCableSync) writeStatuses(ctx context.Context, views []cableView, states map[types.NamespacedName]wire.LineState) error {
	s.mu.Lock()
	if s.owned == nil {
		s.owned = map[types.NamespacedName]bool{}
	}
	before := s.owned
	s.mu.Unlock()
	now := map[types.NamespacedName]bool{}
	var errs []error
	for _, v := range views {
		key := types.NamespacedName{Namespace: v.cable.Namespace, Name: v.cable.Name}
		var want laboratoryv1alpha1.ConsoleCableStatus
		// The admission policy lets this node-agent write only a cable the operator stamped with this node: until the stamp follows a
		// moved console end, the cable is left for the next pass (or for the node-agent of the node it names).
		stamped := v.cable.Annotations[names.AnnotationConsoleNode] == s.NodeName
		switch {
		case v.consoleNode == s.NodeName && !stamped:
			now[key] = true
			continue
		case v.consoleNode == s.NodeName:
			now[key] = true
			st, given := states[key]
			reason := v.reason
			if given && st.UID == string(v.cable.UID) {
				reason = st.Reason
				want = desiredStatus(v, st.ConsoleAttached, st.SerialAttached, st.Up, reason)
			} else {
				if reason == "" {
					reason = wire.LineWaitingForDevice
				}
				want = desiredStatus(v, false, false, false, reason)
			}
		case before[key] && v.cable.Status.Console.NodeName == s.NodeName && stamped:
			// the console end left this node and no new owner wrote the cable yet: say the line is down
			want = desiredStatus(v, false, false, false, wire.LineWaitingForDevice)
		default:
			continue
		}
		if err := s.putStatus(ctx, v.cable, want); err != nil {
			errs = append(errs, err)
		}
	}
	s.mu.Lock()
	s.owned = now
	s.mu.Unlock()
	return errors.Join(errs...)
}

func desiredStatus(v cableView, consoleAttached, serialAttached, up bool, reason string) laboratoryv1alpha1.ConsoleCableStatus {
	out := laboratoryv1alpha1.ConsoleCableStatus{
		Serial:     laboratoryv1alpha1.ConsoleCableEndStatus{NodeName: v.serialNode, Attached: serialAttached},
		Console:    laboratoryv1alpha1.ConsoleCableEndStatus{NodeName: v.consoleNode, Attached: consoleAttached},
		LineUp:     up,
		Conditions: append([]metav1.Condition(nil), v.cable.Status.Conditions...),
	}
	cond := metav1.Condition{Type: labstatus.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, ObservedGeneration: v.cable.Generation,
		Message: lineMessage(reason)}
	if up {
		cond.Status, cond.Reason = metav1.ConditionTrue, consoleCableReady
	}
	if cond.Reason == "" {
		cond.Reason = wire.LineWaitingForDevice
	}
	meta.SetStatusCondition(&out.Conditions, cond)
	return out
}

func lineMessage(reason string) string {
	switch reason {
	case wire.LineWaitingForDevice:
		return "an end of the line is not running yet"
	case wire.LinePortDown:
		return "a port of the line cannot be attached now; retrying"
	case wire.LinePeerUnreachable:
		return "the node of the serial end cannot be reached; retrying"
	case wire.LineNodeLineLimit:
		return "the node of the console end runs the most console lines it may; waiting for room"
	}
	return "the line is up"
}

// putStatus writes the status when it differs.
func (s *ConsoleCableSync) putStatus(ctx context.Context, c *laboratoryv1alpha1.ConsoleCable, want laboratoryv1alpha1.ConsoleCableStatus) error {
	if equality.Semantic.DeepEqual(c.Status, want) {
		return nil
	}
	cp := c.DeepCopy()
	cp.Status = want
	if err := s.Client.Status().Update(ctx, cp); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}
