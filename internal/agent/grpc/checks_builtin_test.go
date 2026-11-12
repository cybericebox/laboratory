package grpc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func builtinCheck() *protobuf.Check {
	i32 := func(v int32) *int32 { return &v }
	part := func(o *protobuf.CheckPart) *protobuf.CheckPart { return o }
	return &protobuf.Check{Id: "all", Device: "web", Mode: protobuf.CheckMode_CHECK_MODE_ONCE, ScheduleSeconds: 30, Parts: []*protobuf.CheckPart{
		part(&protobuf.CheckPart{Observation: &protobuf.CheckPart_Directory{Directory: &protobuf.DirectoryObservation{Path: "/srv/app", HasEntries: []string{"config.php"}, LacksEntries: []string{"install.php"}, Mode: u32p(0o750)}}}),
		part(&protobuf.CheckPart{Observation: &protobuf.CheckPart_Process{Process: &protobuf.ProcessObservation{Name: strp("nginx"), User: strp("www-data")}}}),
		part(&protobuf.CheckPart{Observation: &protobuf.CheckPart_Port{Port: &protobuf.PortObservation{Port: 8080}}}),
		part(&protobuf.CheckPart{Observation: &protobuf.CheckPart_KernelParam{KernelParam: &protobuf.KernelParamObservation{Name: "net.ipv4.ip_forward", Equals: strp("1")}}}),
		part(&protobuf.CheckPart{Observation: &protobuf.CheckPart_Request{Request: &protobuf.RequestObservation{Protocol: "http", Port: 8080, BodyContains: strp("ok")}}}),
		part(&protobuf.CheckPart{Observation: &protobuf.CheckPart_Request{Request: &protobuf.RequestObservation{Protocol: "https", Host: "db", Port: 443, Method: "HEAD", Path: "/", Status: i32(204)}}}),
		part(&protobuf.CheckPart{Observation: &protobuf.CheckPart_Permission{Permission: &protobuf.PermissionObservation{User: "www-data", Path: "/etc/shadow", Read: boolp(false)}}}),
		part(&protobuf.CheckPart{Observation: &protobuf.CheckPart_File{File: &protobuf.FileObservation{Path: "/etc/x", Type: strp("symlink"), Checksum: strp("0000000000000000000000000000000000000000000000000000000000000000")}}}),
	}}
}

// The built-in observations travel into the checks Secret with their defaults filled in.
func TestBuiltinObservationsReachTheSecretWithDefaults(t *testing.T) {
	h, k8s := contractHandler(t, Features{ChecksEnabled: true})
	res := createContractLab(t, h, &protobuf.LabVariant{SpecJson: contractSpecJSON(t, nil, "web")},
		&protobuf.LabItem{Checks: &protobuf.LabChecks{Items: []*protobuf.Check{builtinCheck()}}})
	wantStates(t, res, nil, stCreated)
	var doc labchecks.ChecksDoc
	if err := json.Unmarshal(getSecretData(t, k8s, "c1"+labchecks.ChecksSecretSuffix)[labchecks.ChecksKey], &doc); err != nil {
		t.Fatal(err)
	}
	parts := doc.Checks[0].Parts
	if len(parts) != 8 {
		t.Fatalf("%d parts", len(parts))
	}
	if d := parts[0].Directory; d == nil || d.Path != "/srv/app" || len(d.HasEntries) != 1 || *d.Mode != 0o750 {
		t.Errorf("directory: %+v", d)
	}
	if p := parts[1].Process; p == nil || *p.Running != true || *p.User != "www-data" {
		t.Errorf("process (running defaults to true): %+v", p)
	}
	if p := parts[2].Port; p == nil || p.Protocol != "tcp" || !*p.Listening {
		t.Errorf("port: %+v", p)
	}
	if k := parts[3].KernelParam; k == nil || *k.Equals != "1" {
		t.Errorf("kernel param: %+v", k)
	}
	if r := parts[4].Request; r == nil || r.Host != "127.0.0.1" || r.Method != "GET" || r.Path != "/" || *r.Status != 200 || *r.BodyContains != "ok" {
		t.Errorf("request defaults: %+v", r)
	}
	if r := parts[5].Request; r == nil || r.Host != "db" || r.Method != "HEAD" || *r.Status != 204 {
		t.Errorf("request given: %+v", r)
	}
	if p := parts[6].Permission; p == nil || p.User != "www-data" || *p.Read != false || p.Write != nil {
		t.Errorf("permission: %+v", p)
	}
	if f := parts[7].File; f == nil || *f.Type != "symlink" || f.Checksum == nil {
		t.Errorf("file extras: %+v", f)
	}
}

func TestBuiltinObservationsAreValidatedAtTheAgent(t *testing.T) {
	for name, bad := range map[string]*protobuf.CheckPart{
		"port zero":       {Observation: &protobuf.CheckPart_Port{Port: &protobuf.PortObservation{Port: 0}}},
		"sysctl path":     {Observation: &protobuf.CheckPart_KernelParam{KernelParam: &protobuf.KernelParamObservation{Name: "../etc/passwd", Equals: strp("x")}}},
		"request post":    {Observation: &protobuf.CheckPart_Request{Request: &protobuf.RequestObservation{Protocol: "http", Port: 80, Method: "POST"}}},
		"permission none": {Observation: &protobuf.CheckPart_Permission{Permission: &protobuf.PermissionObservation{User: "x", Path: "/etc"}}},
		"process none":    {Observation: &protobuf.CheckPart_Process{Process: &protobuf.ProcessObservation{User: strp("x")}}},
		"directory slash": {Observation: &protobuf.CheckPart_Directory{Directory: &protobuf.DirectoryObservation{Path: "/x", HasEntries: []string{"a/b"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := contractHandler(t, Features{ChecksEnabled: true})
			c := &protobuf.Check{Id: "x", Device: "web", Mode: protobuf.CheckMode_CHECK_MODE_ONCE, Parts: []*protobuf.CheckPart{bad}}
			res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
				Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: contractSpecJSON(t, nil, "web")}},
				Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: "x", VariantId: "v", Checks: &protobuf.LabChecks{Items: []*protobuf.Check{c}}}}})
			invalidArg(t, res, err, "check \"x\" part 0")
		})
	}
}

func TestFeaturesListTheObservationsTheCheckerRuns(t *testing.T) {
	f := testFeatures
	f.ChecksEnabled = true
	h := featuresHandler(t, f, newTenantTenant("a", true, nil))
	got, err := h.GetFeatures(asClient("a"), &protobuf.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"file": true, "directory": true, "process": true, "port": true, "kernel_param": true, "request": true, "permission": true, "custom": true}
	obs := got.GetChecks().GetObservations()
	if len(obs) != len(want) {
		t.Fatalf("observations %v", obs)
	}
	for _, o := range obs {
		if !want[o] {
			t.Errorf("unexpected observation %q", o)
		}
	}
}
