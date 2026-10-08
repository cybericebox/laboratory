package l7

import (
	"bufio"
	"context"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"io"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
	"time"
)

type accessCountingReader struct {
	client.Reader
	gets map[string]int
}

func (r *accessCountingReader) Get(ctx context.Context, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.gets[k.Name]++
	return r.Reader.Get(ctx, k, obj, opts...)
}
func TestAccessReaderOneServiceReadAndDeletingClientDenied(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = lab.AddToScheme(scheme)
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", UID: types.UID("u"), Labels: map[string]string{names.LabelTenant: "acme"}}, Status: lab.LabGroupStatus{Namespace: "legacy"}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "legacy", Labels: map[string]string{names.LabelLab: "lab", names.LabelDevice: "web"}}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "https", Port: 443}}}}
	member := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "legacy"}}
	policy := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "legacy"}, Spec: lab.LabGroupAccessPolicySpec{Rules: []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessAllow, ClientNames: []string{"p"}, LabNames: []string{"lab"}}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, svc, member, policy, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "legacy"}}).Build()
	reader := &accessCountingReader{Reader: c, gets: map[string]int{}}
	access := &AccessReader{Reader: reader}
	ctx := context.Background()
	g, err := access.Group(ctx, "g")
	if err != nil || g.Namespace != "legacy" || g.Tenant != "acme" {
		t.Fatal(g, err)
	}
	route, err := access.Route(ctx, "web", g.Namespace)
	if err != nil || route.Lab != "lab" || route.URL != "https://web.legacy.svc.cluster.local:443" || reader.gets["web"] != 1 {
		t.Fatal(route, err, reader.gets)
	}
	if !access.Allowed(ctx, g.Namespace, "p", "lab") {
		t.Fatal("allowed denied")
	}
	member.Finalizers = []string{"keep"}
	_ = c.Update(ctx, member)
	_ = c.Delete(ctx, member)
	if access.Allowed(ctx, g.Namespace, "p", "lab") {
		t.Fatal("deleting client allowed")
	}
	group.Finalizers = []string{"keep"}
	_ = c.Update(ctx, group)
	_ = c.Delete(ctx, group)
	if _, err := access.Group(ctx, "g"); err == nil {
		t.Fatal("deleting group resolved")
	}
}
func TestCompactProxyCacheKeepsRoutingAndDeletion(t *testing.T) {
	now := metav1.NewTime(time.Now())
	g := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", DeletionTimestamp: &now, ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "big"}}}, Status: lab.LabGroupStatus{Namespace: "legacy", Phase: lab.PhaseReady}}
	transformed, err := CompactCacheObject(g)
	if err != nil {
		t.Fatal(err)
	}
	got := transformed.(*lab.LabGroup)
	if got.Status.Namespace != "legacy" || got.Status.Phase != "" || got.DeletionTimestamp == nil || len(got.ManagedFields) != 0 {
		t.Fatal(got)
	}
}
func TestGroupedLiveChecksReuseAuthorization(t *testing.T) {
	h := NewHandler(nil, nil, "", "", nil)
	calls := 0
	h.WithAuthorizer(func(string, string, string) bool { calls++; return true })
	for i := 0; i < 20; i++ {
		h.live.tryAdd(&liveEntry{group: "g", client: "p", lab: "lab", deadline: time.Now().Add(time.Hour), cancel: func() {}}, LiveCaps{})
	}
	if h.CheckLive() != 0 || calls != 1 {
		t.Fatal(calls)
	}
}

func TestAccessHandlerUsesOneGroupAndServiceRead(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = lab.AddToScheme(scheme)
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{names.LabelTenant: "acme"}}, Status: lab.LabGroupStatus{Namespace: "legacy"}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web-abc123", Namespace: "legacy", Labels: map[string]string{names.LabelLab: "lab", names.LabelDevice: "web"}}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}}
	member := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p-u1", Namespace: "legacy"}}
	policy := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "legacy"}, Spec: lab.LabGroupAccessPolicySpec{Rules: []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessAllow, ClientNames: []string{"p-u1"}, LabNames: []string{"lab"}}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, svc, member, policy, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "legacy"}}).Build()
	reader := &accessCountingReader{Reader: c, gets: map[string]int{}}
	h, _, _, cookie := liveFixture(t, http.NotFoundHandler())
	m := NewMeter("b", time.Now())
	h.WithAccounting(m, nil).WithAccessReader(&AccessReader{Reader: reader})
	h.transport = accountingRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("abc")), ContentLength: 3}, nil
	})
	req := httptest.NewRequest("GET", "http://web-abc123.challenges.example.com/", nil)
	req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || reader.gets["g1"] != 1 || reader.gets["web-abc123"] != 1 || reader.gets["p-u1"] != 1 || reader.gets[names.LabGroupAccessPolicyName] != 1 {
		t.Fatal(rec.Code, reader.gets)
	}
	rows, _ := m.Ledger("legacy")
	if len(rows) != 1 || rows[0].BytesIn != 3 {
		t.Fatal(rows)
	}
}

func TestStoppedLabRevokesCachedSessionAndLiveConnection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	target := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "ns"}}
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", Labels: map[string]string{names.LabelTenant: "tenant"}}, Status: lab.LabGroupStatus{Namespace: "ns"}}
	member := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	policy := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "ns"}, Spec: lab.LabGroupAccessPolicySpec{Rules: []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessAllow}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, group, member, policy).Build()
	a := &AccessReader{Reader: c}
	ctx := context.Background()
	if !a.Allowed(ctx, "ns", "p", "lab") {
		t.Fatal("legacy running lab denied")
	}
	h := NewHandler(nil, nil, "", "", nil).WithAccessReader(a)
	closed := false
	h.live.tryAdd(&liveEntry{group: "g", tenant: "tenant", client: "p", lab: "lab", deadline: time.Now().Add(time.Hour), cancel: func() { closed = true }}, LiveCaps{})
	target.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}
	if err := c.Update(ctx, target); err != nil {
		t.Fatal(err)
	}
	if a.Allowed(ctx, "ns", "p", "lab") {
		t.Fatal("stopped lab allowed by old policy")
	}
	if h.CheckLive() != 1 || !closed {
		t.Fatal("existing authenticated connection survived desired stop")
	}
	compact, err := CompactCacheObject(target)
	if err != nil {
		t.Fatal(err)
	}
	got := compact.(*lab.Lab)
	if !got.Spec.Lifecycle.IsStopped() || len(got.Spec.Devices) != 0 {
		t.Fatal("compact lifecycle lost")
	}
	if a.Allowed(ctx, "ns", "p", "missing") {
		t.Fatal("unknown target lab allowed")
	}
}

func TestStoppedLabClosesNativeUpgradeAndRejectsStaleCookie(t *testing.T) {
	h, srv, _, cookie := liveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, conn)
	}))
	backend, err := h.resolver("web-abc123", "g1")
	if err != nil {
		t.Fatal(err)
	}
	backendURL, _ := url.Parse(backend)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, backendURL.Host)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	h.transport = transport
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	target := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "legacy"}}
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{names.LabelTenant: "acme"}}, Status: lab.LabGroupStatus{Namespace: "legacy"}}
	member := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p-u1", Namespace: "legacy"}}
	policy := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "legacy"}, Spec: lab.LabGroupAccessPolicySpec{Rules: []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessAllow}}}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web-abc123", Namespace: "legacy", Labels: map[string]string{names.LabelLab: "a", names.LabelDevice: "web"}}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, group, member, policy, svc).Build()
	h.WithAccessReader(&AccessReader{Reader: c})
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /term HTTP/1.1\r\nHost: web-abc123.challenges.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nCookie: challenge=%s\r\n\r\n", cookie)
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		t.Fatal("native upgrade control", line, err)
	}
	for {
		line, _ = br.ReadString('\n')
		if line == "\r\n" {
			break
		}
	}
	fmt.Fprint(conn, "control\n")
	if line, err := br.ReadString('\n'); err != nil || line != "control\n" {
		t.Fatal("native established socket failed", line, err)
	}
	start := time.Now()
	target.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}
	if err := c.Update(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if closed := h.CheckLive(); closed != 1 {
		t.Fatal("native session not revoked", closed)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("existing upgrade still open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("upgrade not closed within measured boundary")
	}
	req, _ := http.NewRequest("GET", srv.URL+"/term", nil)
	req.Host = "web-abc123.challenges.example.com"
	req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatal("stale cookie bypassed desired stop", resp.StatusCode)
	}
	t.Logf("desired stop in cache to native upgrade close and stale-cookie rejection: %s", time.Since(start))
}
