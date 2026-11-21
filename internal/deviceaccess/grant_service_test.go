package deviceaccess

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/grant"
	"github.com/cybericebox/laboratory/internal/grant/granttest"
	"github.com/cybericebox/laboratory/internal/names"
)

func refusedWith(t *testing.T, svc *Service, r wire.OpenRequest) string {
	t.Helper()
	c, done := run(svc, context.Background())
	c.sendOpen(t, r)
	reply := c.reply(t)
	waitDone(t, done)
	if reply.OK {
		t.Fatalf("the console opened for %+v", r)
	}
	return reply.Error
}

func TestServiceRefusesARequestWithoutAValidGrantAsUnauthorized(t *testing.T) {
	svc := testService(labRuntime())
	if code := refusedWith(t, svc, func() wire.OpenRequest { r := openReq(); r.Grant = "x.y.z"; return r }()); code != wire.CodeUnauthorized {
		t.Errorf("garbage grant: %q", code)
	}
	web := openReq()
	c := testGrantKey.Claims(time.Now(), time.Hour, "dev-abc12w.labs.example.test")
	c.Group, c.Lab, c.Dev, c.Subject, c.Audience = web.Group, web.Lab, web.Device, web.Client, jwt.ClaimStrings{grant.AudienceProxy, grant.AudienceDeviceAccess}
	web.Grant = testGrantKey.Sign(t, c) // kind web
	if code := refusedWith(t, svc, web); code != wire.CodeUnauthorized {
		t.Errorf("a web grant: %q", code)
	}
	// a request for another device than the one the grant names
	other := openReq()
	other.Grant = testGrantFor(t, other, time.Now())
	other.Device = "another"
	if code := refusedWith(t, svc, other); code != wire.CodeUnauthorized {
		t.Errorf("a grant for another device: %q", code)
	}
}

// The stream ends at the exp of the grant it was opened with, whatever the lifetime limit says.
func TestConsoleEndsWhenTheGrantEnds(t *testing.T) {
	rt := labRuntime()
	svc := testService(rt)
	c, done := run(svc, context.Background())
	r := openReq()
	claims := testGrantKey.Claims(time.Now(), 2*time.Second, "dev-abc12t.labs.example.test")
	claims.Group, claims.Lab, claims.Dev, claims.Kind, claims.Subject = r.Group, r.Lab, r.Device, grant.KindConsole, r.Client
	claims.Audience = jwt.ClaimStrings{grant.AudienceProxy, grant.AudienceDeviceAccess}
	r.Grant = testGrantKey.Sign(t, claims)
	c.sendOpen(t, r)
	if reply := c.reply(t); !reply.OK {
		t.Fatalf("refused: %+v", reply)
	}
	start := time.Now()
	expectClose(t, c, wire.ReasonAccess)
	waitDone(t, done)
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("the console outlived its grant by %v", d)
	}
	if !rt.allClosed() {
		t.Fatal("exec left behind")
	}
}

// A lab and a device name repeat across groups, and the namespace of the request is the proxy's word: the pod must carry the name of the
// group the grant is for, or the device is not found.
func TestConsoleIsBoundToTheGroupOfTheGrant(t *testing.T) {
	for name, label := range map[string]string{
		"a pod of another group":  names.EncodeName("g-2"),
		"a pod without the label": "",
	} {
		rt := labRuntime()
		for _, c := range rt.containers {
			if c.Labels[labelKind] == kindSandbox {
				if label == "" {
					delete(c.Labels, names.LabelGroupName)
				} else {
					c.Labels[names.LabelGroupName] = label
				}
			}
		}
		if code := refusedWith(t, testService(rt), openReq()); code != wire.CodeNotFound {
			t.Errorf("%s: %q", name, code)
		}
	}
	// and the right group opens it
	svc := testService(labRuntime())
	c, done := run(svc, context.Background())
	c.sendOpen(t, openReq())
	if reply := c.reply(t); !reply.OK {
		t.Fatalf("the group of the grant is refused: %+v", reply)
	}
	c.send(wire.Close(wire.ReasonClient))
	expectClose(t, c, wire.ReasonClient)
	waitDone(t, done)
}

func TestResolveGroupNameIsOptionalForTheCheckerAndRequiredWhenGiven(t *testing.T) {
	rt := labRuntime()
	target := Target{Namespace: testNS, Lab: testLab, Device: testDev}
	if _, err := Resolve(context.Background(), rt, target); err != nil {
		t.Fatalf("no group asked (the checker socket): %v", err)
	}
	target.GroupName = names.EncodeName("g-1")
	if _, err := Resolve(context.Background(), rt, target); err != nil {
		t.Fatalf("the right group: %v", err)
	}
	target.GroupName = names.EncodeName("g-2")
	if _, err := Resolve(context.Background(), rt, target); err == nil {
		t.Fatal("another group resolved")
	}
}

// The service verifies a grant with the keys of every tenant, so the tenant that signed it must be the tenant of the pod: a second tenant
// that holds a valid key cannot open the device of another tenant's group.
func TestConsoleIsBoundToTheTenantOfTheGrant(t *testing.T) {
	beta := granttest.MustKey("beta", "k1")
	both := func(tenant, id string) (ed25519.PublicKey, bool) {
		if k, ok := testGrantKey.Lookup()(tenant, id); ok {
			return k, ok
		}
		return beta.Lookup()(tenant, id)
	}
	service := func(rt Runtime) *Service {
		svc := testService(rt)
		svc.auth = NewAuth(both, 8*time.Hour)
		return svc
	}
	// beta signs a perfect grant for the group, lab and device of a pod of acme
	r := openReq()
	c := beta.Claims(time.Now(), time.Hour, "dev-abc12t.labs.example.test")
	c.Group, c.Lab, c.Dev, c.Kind, c.Subject = r.Group, r.Lab, r.Device, grant.KindConsole, r.Client
	c.Audience = jwt.ClaimStrings{grant.AudienceProxy, grant.AudienceDeviceAccess}
	r.Grant = beta.Sign(t, c)
	if code := refusedWith(t, service(labRuntime()), r); code != wire.CodeNotFound {
		t.Errorf("a grant of another tenant: %q", code)
	}
	// a pod without the tenant label never matches
	rt := labRuntime()
	for _, c := range rt.containers {
		if c.Labels[labelKind] == kindSandbox {
			delete(c.Labels, names.LabelTenant)
		}
	}
	if code := refusedWith(t, service(rt), openReq()); code != wire.CodeNotFound {
		t.Errorf("a pod without the tenant label: %q", code)
	}
	// the owner opens it
	svc := service(labRuntime())
	cn, done := run(svc, context.Background())
	cn.sendOpen(t, openReq())
	if reply := cn.reply(t); !reply.OK {
		t.Fatalf("the tenant of the pod is refused: %+v", reply)
	}
	cn.send(wire.Close(wire.ReasonClient))
	expectClose(t, cn, wire.ReasonClient)
	waitDone(t, done)
}

// The command and the user are the lab author's as the operator stamped them: a request that names others, or a pod without the stamp, is refused.
func TestConsoleRunsOnlyWhatTheOperatorStamped(t *testing.T) {
	r := openReq()
	r.Command, r.User = []string{"/bin/bash"}, "root"
	r.Grant = testGrantFor(t, r, time.Now())
	if code := refusedWith(t, testService(labRuntime()), r); code != wire.CodeUnauthorized {
		t.Errorf("a command the device does not name: %q", code)
	}
	rt := labRuntime()
	for _, c := range rt.containers {
		if c.Labels[labelKind] == kindSandbox {
			delete(c.Labels, names.LabelTerminalSpec)
		}
	}
	if code := refusedWith(t, testService(rt), openReq()); code != wire.CodeUnauthorized {
		t.Errorf("a pod without the stamp: %q", code)
	}
	// the stamped command and user pass
	rt = labRuntime()
	for _, c := range rt.containers {
		if c.Labels[labelKind] == kindSandbox {
			c.Labels[names.LabelTerminalSpec] = names.TerminalSpecHash([]string{"/bin/bash"}, "")
		}
	}
	ok := openReq()
	ok.Command = []string{"/bin/bash"}
	ok.Grant = testGrantFor(t, ok, time.Now())
	svc := testService(rt)
	cn, done := run(svc, context.Background())
	cn.sendOpen(t, ok)
	if reply := cn.reply(t); !reply.OK {
		t.Fatalf("the stamped command was refused: %+v", reply)
	}
	cn.send(wire.Close(wire.ReasonClient))
	expectClose(t, cn, wire.ReasonClient)
	waitDone(t, done)
}
