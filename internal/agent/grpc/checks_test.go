package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	devnames "github.com/cybericebox/laboratory/internal/devices"
	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// Markers that must never leave the Secrets: they stand for a flag, an expected value and a script body.
const (
	secretScript   = "echo SCRIPT-MARKER-7"
	secretFileBody = "FILE-BODY-MARKER-8"
	secretExpected = "EXPECTED-MARKER-9"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }
func u32p(v uint32) *uint32 { return &v }

func bootstrapOf(device string) *protobuf.DeviceBootstrap {
	return &protobuf.DeviceBootstrap{
		Device: device, Script: secretScript,
		Files: []*protobuf.BootstrapFile{{Path: "/etc/app/app.conf", Content: []byte(secretFileBody), Owner: "app"}},
	}
}

func fileCheck(id, device string) *protobuf.Check {
	return &protobuf.Check{
		Id: id, Device: device, Mode: protobuf.CheckMode_CHECK_MODE_CONTINUOUS, ScheduleSeconds: 30, ManualRun: true,
		Parts: []*protobuf.CheckPart{
			{Observation: &protobuf.CheckPart_File{File: &protobuf.FileObservation{Path: "/etc/app/app.conf", ContentContains: strp(secretExpected)}}},
			{Observation: &protobuf.CheckPart_Custom{Custom: &protobuf.CustomObservation{Command: []string{"/bin/true"}, OutputMatches: strp("^ok$")}}},
		},
	}
}

func contractSpecJSON(t *testing.T, mod func(*laboratoryv1alpha1.LabSpec), devices ...string) []byte {
	t.Helper()
	var spec laboratoryv1alpha1.LabSpec
	if err := json.Unmarshal(specJSON(devices...), &spec); err != nil {
		t.Fatal(err)
	}
	if mod != nil {
		mod(&spec)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func contractHandler(t *testing.T, f Features) (*Handler, kubernetes.Interface) {
	t.Helper()
	h, k8s := newTestHandler(t)
	h.SetFeatures(f)
	readyGroup(t, h, k8s, "team-lab", "team-lab", nil)
	return h, k8s
}

func getSecretData(t *testing.T, k8s kubernetes.Interface, name string) map[string][]byte {
	t.Helper()
	s, err := k8s.CoreV1().Secrets("team-lab").Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Kind != "Lab" {
		t.Fatalf("%s: expected the Lab as the owner, got %+v", name, s.OwnerReferences)
	}
	return s.Data
}

func createContractLab(t *testing.T, h *Handler, variant *protobuf.LabVariant, item *protobuf.LabItem) *protobuf.BatchResult {
	t.Helper()
	variant.VariantId = "v"
	item.LabGroup, item.Name, item.VariantId = "team-lab", "c1", "v"
	res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{variant}, Items: []*protobuf.LabItem{item}})
	if err != nil {
		t.Fatalf("CreateLabs: %v", err)
	}
	return res
}

// Bootstrap and checks land in write-only Secrets with their defaults filled in, and nowhere else.
func TestBootstrapAndChecksAreSecretsOnly(t *testing.T) {
	h, k8s := contractHandler(t, Features{ChecksEnabled: true, TerminalEnabled: true})
	ctx := context.Background()
	res := createContractLab(t, h,
		&protobuf.LabVariant{
			SpecJson:  contractSpecJSON(t, nil, "web", "db"),
			Bootstrap: []*protobuf.DeviceBootstrap{bootstrapOf("web")},
			Env:       []*protobuf.DeviceEnv{envOf("web", "MODE", "ctf")},
		},
		&protobuf.LabItem{Checks: &protobuf.LabChecks{Items: []*protobuf.Check{fileCheck("cfg", "web")}}})
	wantStates(t, res, nil, stCreated)

	// the bootstrap Secret: the format the launcher reads, defaults filled
	data := getSecretData(t, k8s, devnames.Name("c1", "web")+labchecks.BootstrapSecretSuffix)
	var b labchecks.Bootstrap
	if err := json.Unmarshal(data[labchecks.BootstrapKey], &b); err != nil {
		t.Fatalf("bootstrap.json: %v (%v)", err, data)
	}
	if b.Script != secretScript || b.Interpreter != "/bin/sh" || b.TimeLimitSeconds != labchecks.DefaultBootstrapTimeLimit ||
		len(b.Files) != 1 || string(b.Files[0].Content) != secretFileBody || b.Files[0].Mode != 0o644 || b.Files[0].Owner != "app" {
		t.Fatalf("bootstrap document: %+v", b)
	}
	if getSecretData(t, k8s, devnames.Name("c1", "db")+labchecks.BootstrapSecretSuffix) != nil {
		t.Fatal("a device without a list has no bootstrap Secret")
	}

	// the checks Secret of the lab
	var doc labchecks.ChecksDoc
	if err := json.Unmarshal(getSecretData(t, k8s, "c1"+labchecks.ChecksSecretSuffix)[labchecks.ChecksKey], &doc); err != nil {
		t.Fatalf("checks.json: %v", err)
	}
	if len(doc.Checks) != 1 || doc.Checks[0].Mode != "continuous" || doc.Checks[0].TimeLimitSeconds != labchecks.DefaultCheckTimeLimit ||
		doc.Checks[0].Parts[1].Custom.User != "nobody" || *doc.Checks[0].Parts[0].File.ContentContains != secretExpected {
		t.Fatalf("checks document: %+v", doc.Checks)
	}

	// the environment mechanism is unchanged and separate
	if env := getSecretData(t, k8s, devnames.Name("c1", "web")+"-env"); string(env["MODE"]) != "ctf" || len(env) != 1 {
		t.Fatalf("env Secret: %v", env)
	}

	// the CR carries only the markers, never a value
	lab, err := h.cs.LaboratoryV1alpha1().Labs("team-lab").Get(ctx, "c1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lab.Spec.Devices[0].Bootstrap == nil || !lab.Spec.Devices[0].Bootstrap.Enabled || lab.Spec.Devices[1].Bootstrap != nil {
		t.Fatalf("bootstrap markers: %+v", lab.Spec.Devices)
	}
	if lab.Spec.Checks == nil || len(lab.Spec.Checks.Items) != 1 || lab.Spec.Checks.Items[0] != (laboratoryv1alpha1.CheckRef{ID: "cfg", Device: "web", ManualRun: true}) {
		t.Fatalf("checks markers: %+v", lab.Spec.Checks)
	}
	crJSON, _ := json.Marshal(lab)

	// and no read call returns a value: List, the monitoring projection, the status
	list, err := h.ListLabs(ctx, &protobuf.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	listRaw, _ := proto.Marshal(list)
	monRaw, _ := proto.Marshal(labMonitoringToProto(lab, "team-lab"))
	for what, raw := range map[string][]byte{"the Lab CR": crJSON, "ListLabs": listRaw, "monitoring": monRaw} {
		for _, marker := range []string{secretScript, secretFileBody, secretExpected, "SCRIPT-MARKER", "app.conf"} {
			if bytes.Contains(raw, []byte(marker)) {
				t.Errorf("%s carries %q", what, marker)
			}
		}
	}
}

// A resend rewrites the Secrets (like the env Secrets); a value a client puts in spec_json for the markers is ignored.
func TestBootstrapResendRewritesAndClientMarkersAreIgnored(t *testing.T) {
	h, k8s := contractHandler(t, Features{ChecksEnabled: true})
	spec := contractSpecJSON(t, func(s *laboratoryv1alpha1.LabSpec) {
		s.Devices[0].Bootstrap = &laboratoryv1alpha1.DeviceBootstrapSpec{Enabled: true}
		s.Checks = &laboratoryv1alpha1.LabChecksSpec{Items: []laboratoryv1alpha1.CheckRef{{ID: "ghost", Device: "web"}}}
	}, "web")
	res := createContractLab(t, h, &protobuf.LabVariant{SpecJson: spec}, &protobuf.LabItem{})
	wantStates(t, res, nil, stCreated)
	lab, _ := h.cs.LaboratoryV1alpha1().Labs("team-lab").Get(context.Background(), "c1", metav1.GetOptions{})
	if lab.Spec.Devices[0].Bootstrap != nil || lab.Spec.Checks != nil {
		t.Fatalf("the agent alone says that a Secret exists: %+v / %+v", lab.Spec.Devices[0].Bootstrap, lab.Spec.Checks)
	}

	// create again with a bootstrap: a different spec (the marker differs), refused for that item
	res = createContractLab(t, h, &protobuf.LabVariant{SpecJson: spec, Bootstrap: []*protobuf.DeviceBootstrap{bootstrapOf("web")}}, &protobuf.LabItem{})
	wantStates(t, res, nil, stFailed)

	// a second lab with a bootstrap, resent with another script: EXISTS and the Secret follows
	send := func(script string) {
		t.Helper()
		b := bootstrapOf("web")
		b.Script = script
		variant := &protobuf.LabVariant{VariantId: "w", SpecJson: contractSpecJSON(t, nil, "web"), Bootstrap: []*protobuf.DeviceBootstrap{b}}
		res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{variant}, Items: []*protobuf.LabItem{{LabGroup: "team-lab", Name: "c2", VariantId: "w"}}})
		if script == "echo one" {
			wantStates(t, res, err, stCreated)
		} else {
			wantStates(t, res, err, stExists)
		}
	}
	send("echo one")
	send("echo two")
	var b labchecks.Bootstrap
	_ = json.Unmarshal(getSecretData(t, k8s, devnames.Name("c2", "web")+labchecks.BootstrapSecretSuffix)[labchecks.BootstrapKey], &b)
	if b.Script != "echo two" {
		t.Fatalf("the resend must rewrite the bootstrap Secret: %+v", b)
	}
}

func TestBootstrapUpdate(t *testing.T) {
	h, k8s := contractHandler(t, Features{ChecksEnabled: true})
	ctx := context.Background()
	res := createContractLab(t, h,
		&protobuf.LabVariant{SpecJson: contractSpecJSON(t, nil, "web", "db"), Bootstrap: []*protobuf.DeviceBootstrap{bootstrapOf("web")}},
		&protobuf.LabItem{Checks: &protobuf.LabChecks{Items: []*protobuf.Check{fileCheck("cfg", "web")}}})
	wantStates(t, res, nil, stCreated)

	update := func(ch *protobuf.LabChanges) *protobuf.ItemResult {
		t.Helper()
		out, err := h.UpdateLabs(ctx, &protobuf.UpdateLabsRequest{Items: []*protobuf.UpdateLabItem{{LabGroup: "team-lab", Name: "c1", Changes: ch}}})
		if err != nil {
			t.Fatal(err)
		}
		return out.Results[0]
	}
	// rewrite of a declared device
	b := bootstrapOf("web")
	b.Script = "echo rewritten"
	if r := update(&protobuf.LabChanges{Bootstrap: []*protobuf.DeviceBootstrap{b}}); r.State != stUpdated {
		t.Fatalf("rewrite: %v", r)
	}
	var doc labchecks.Bootstrap
	_ = json.Unmarshal(getSecretData(t, k8s, devnames.Name("c1", "web")+labchecks.BootstrapSecretSuffix)[labchecks.BootstrapKey], &doc)
	if doc.Script != "echo rewritten" {
		t.Fatalf("got %+v", doc)
	}
	// a device that had no bootstrap cannot get one: the CR spec does not change
	if r := update(&protobuf.LabChanges{Bootstrap: []*protobuf.DeviceBootstrap{bootstrapOf("db")}}); r.State != stFailed || !strings.Contains(r.Error, "cannot be added later") {
		t.Fatalf("add: %v", r)
	}
	// an unknown device
	if r := update(&protobuf.LabChanges{Bootstrap: []*protobuf.DeviceBootstrap{bootstrapOf("ghost")}}); r.State != stFailed || !strings.Contains(r.Error, "unknown device") {
		t.Fatalf("unknown: %v", r)
	}
	// an empty list removes the Secret
	if r := update(&protobuf.LabChanges{Bootstrap: []*protobuf.DeviceBootstrap{{Device: "web"}}}); r.State != stUpdated {
		t.Fatalf("remove: %v", r)
	}
	if getSecretData(t, k8s, devnames.Name("c1", "web")+labchecks.BootstrapSecretSuffix) != nil {
		t.Fatal("an empty list removes the bootstrap Secret")
	}
	// checks: new expected value is fine, other ids are not
	changed := fileCheck("cfg", "web")
	changed.Parts[0].GetFile().ContentContains = strp("OTHER")
	if r := update(&protobuf.LabChanges{Checks: &protobuf.LabChecks{Items: []*protobuf.Check{changed}}}); r.State != stUpdated {
		t.Fatalf("checks rewrite: %v", r)
	}
	var cd labchecks.ChecksDoc
	_ = json.Unmarshal(getSecretData(t, k8s, "c1"+labchecks.ChecksSecretSuffix)[labchecks.ChecksKey], &cd)
	if *cd.Checks[0].Parts[0].File.ContentContains != "OTHER" {
		t.Fatalf("got %+v", cd)
	}
	if r := update(&protobuf.LabChanges{Checks: &protobuf.LabChecks{Items: []*protobuf.Check{fileCheck("new", "web")}}}); r.State != stFailed {
		t.Fatalf("other id: %v", r)
	}
}

func invalidArg(t *testing.T, res *protobuf.BatchResult, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want InvalidArgument containing %q, got %v", want, res)
	}
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), want) {
		t.Fatalf("want InvalidArgument containing %q, got %v", want, err)
	}
}

func TestContractValidationAtTheAgent(t *testing.T) {
	h, _ := contractHandler(t, Features{ChecksEnabled: true, TerminalEnabled: true})
	hub := func(s *laboratoryv1alpha1.LabSpec) {
		s.Devices = append(s.Devices, laboratoryv1alpha1.DeviceTemplate{Name: "sw", Type: laboratoryv1alpha1.DeviceTypeHub})
	}
	manyFiles := bootstrapOf("web")
	for i := 0; i <= labchecks.MaxBootstrapFiles; i++ {
		manyFiles.Files = append(manyFiles.Files, &protobuf.BootstrapFile{Path: "/f/" + strings.Repeat("x", i+1)})
	}
	bigScript := bootstrapOf("web")
	bigScript.Script = strings.Repeat("a", labchecks.MaxScriptBytes+1)
	bigFile := bootstrapOf("web")
	bigFile.Files[0].Content = make([]byte, labchecks.MaxFileBytes+1)
	hugeTotal := bootstrapOf("web")
	hugeTotal.Files = nil
	for i := 0; i < 3; i++ {
		hugeTotal.Files = append(hugeTotal.Files, &protobuf.BootstrapFile{Path: "/big/" + string(rune('a'+i)), Content: make([]byte, labchecks.MaxFileBytes)})
	}
	slowBoot := bootstrapOf("web")
	slowBoot.TimeLimitSeconds = labchecks.MaxBootstrapTimeLimit + 1
	relPath := bootstrapOf("web")
	relPath.Files[0].Path = "etc/app.conf"
	dupPath := bootstrapOf("web")
	dupPath.Files = append(dupPath.Files, dupPath.Files[0])
	badInterp := bootstrapOf("web")
	badInterp.Interpreter = "bash"

	badRegex := fileCheck("cfg", "web")
	badRegex.Parts[0].GetFile().ContentMatches = strp("(unclosed")
	emptyFile := fileCheck("cfg", "web")
	emptyFile.Parts = []*protobuf.CheckPart{{Observation: &protobuf.CheckPart_File{File: &protobuf.FileObservation{Path: "/etc/x"}}}}
	emptyPart := fileCheck("cfg", "web")
	emptyPart.Parts = []*protobuf.CheckPart{{}}
	noParts := fileCheck("cfg", "web")
	noParts.Parts = nil
	relCheck := fileCheck("cfg", "web")
	relCheck.Parts[0].GetFile().Path = "relative/path"
	absentPlus := fileCheck("cfg", "web")
	absentPlus.Parts = []*protobuf.CheckPart{{Observation: &protobuf.CheckPart_File{File: &protobuf.FileObservation{Path: "/x", Exists: boolp(false), Mode: u32p(0o600)}}}}
	bothCmd := fileCheck("cfg", "web")
	bothCmd.Parts = []*protobuf.CheckPart{{Observation: &protobuf.CheckPart_Custom{Custom: &protobuf.CustomObservation{Command: []string{"a"}, Script: "b"}}}}
	slowCheck := fileCheck("cfg", "web")
	slowCheck.TimeLimitSeconds = labchecks.MaxCheckTimeLimit + 1
	fastSchedule := fileCheck("cfg", "web")
	fastSchedule.TimeLimitSeconds, fastSchedule.ScheduleSeconds = 20, 10
	noMode := fileCheck("cfg", "web")
	noMode.Mode = protobuf.CheckMode_CHECK_MODE_UNSPECIFIED
	var tooMany []*protobuf.Check
	for i := 0; i <= labchecks.MaxChecksPerLab; i++ {
		tooMany = append(tooMany, fileCheck("c"+strings.Repeat("x", i), "web"))
	}

	cases := []struct {
		name      string
		mod       func(*laboratoryv1alpha1.LabSpec)
		bootstrap []*protobuf.DeviceBootstrap
		checks    []*protobuf.Check
		want      string
	}{
		{"bootstrap for an unknown device", nil, []*protobuf.DeviceBootstrap{bootstrapOf("ghost")}, nil, `unknown device "ghost"`},
		{"bootstrap for a hub", hub, []*protobuf.DeviceBootstrap{bootstrapOf("sw")}, nil, "only a container device"},
		{"too many files", nil, []*protobuf.DeviceBootstrap{manyFiles}, nil, "files, the limit"},
		{"script over the limit", nil, []*protobuf.DeviceBootstrap{bigScript}, nil, "the script takes"},
		{"file over the limit", nil, []*protobuf.DeviceBootstrap{bigFile}, nil, "takes"},
		{"all content over the limit", nil, []*protobuf.DeviceBootstrap{hugeTotal}, nil, "the script and files take"},
		{"bootstrap time limit over the ceiling", nil, []*protobuf.DeviceBootstrap{slowBoot}, nil, "time limit"},
		{"relative file path", nil, []*protobuf.DeviceBootstrap{relPath}, nil, "not absolute"},
		{"duplicate file path", nil, []*protobuf.DeviceBootstrap{dupPath}, nil, "listed twice"},
		{"interpreter not a path", nil, []*protobuf.DeviceBootstrap{badInterp}, nil, "interpreter"},
		{"check on an unknown device", nil, nil, []*protobuf.Check{fileCheck("cfg", "ghost")}, `unknown device "ghost"`},
		{"check on a hub", hub, nil, []*protobuf.Check{fileCheck("cfg", "sw")}, "only a container device can be checked"},
		{"duplicate check ids", nil, nil, []*protobuf.Check{fileCheck("cfg", "web"), fileCheck("cfg", "web")}, "used twice"},
		{"invalid regular expression", nil, nil, []*protobuf.Check{badRegex}, "regular expression"},
		{"file observation without expectation", nil, nil, []*protobuf.Check{emptyFile}, "at least one expectation"},
		{"empty observation", nil, nil, []*protobuf.Check{emptyPart}, "observation is empty"},
		{"check without parts", nil, nil, []*protobuf.Check{noParts}, "needs 1 to"},
		{"relative check path", nil, nil, []*protobuf.Check{relCheck}, "not absolute"},
		{"absent file with other expectations", nil, nil, []*protobuf.Check{absentPlus}, "absent"},
		{"command and script both", nil, nil, []*protobuf.Check{bothCmd}, "exactly one of a command and a script"},
		{"check time limit over the ceiling", nil, nil, []*protobuf.Check{slowCheck}, "time limit"},
		{"schedule shorter than the time limit", nil, nil, []*protobuf.Check{fastSchedule}, "shorter than the time limit"},
		{"mode missing", nil, nil, []*protobuf.Check{noMode}, "neither once nor continuous"},
		{"too many checks", nil, nil, tooMany, "the limit is"},
		{"two http accesses", func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].Access = []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessHTTP, Port: 80}, {Kind: laboratoryv1alpha1.AccessHTTP, Port: 81}}
		}, nil, nil, "more than one"},
		{"http without a port", func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].Access = []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessHTTP}}
		}, nil, nil, "needs a port"},
		{"vnc with a protocol", func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].Access = []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessVNC, Port: 5900, Protocol: "https"}}
		}, nil, nil, "only an http access has a protocol"},
		{"terminal with a port", func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].Access = []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessTerminal, Port: 22}}
		}, nil, nil, "no port or protocol"},
		{"http with a command", func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].Access = []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessHTTP, Port: 80, Command: []string{"sh"}}}
		}, nil, nil, "no command or user"},
		{"unknown kind", func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].Access = []laboratoryv1alpha1.DeviceAccess{{Kind: "ssh", Port: 22}}
		}, nil, nil, "unknown access kind"},
		{"terminal on a hub", func(s *laboratoryv1alpha1.LabSpec) {
			hub(s)
			s.Devices[1].Access = []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessTerminal}}
		}, nil, nil, "only a container device can have a terminal"},
		{"service on a hub", func(s *laboratoryv1alpha1.LabSpec) {
			hub(s)
			s.Devices[1].Service = &laboratoryv1alpha1.DeviceService{Command: []string{"x"}}
		}, nil, nil, "only a container device has a service command"},
		{"relative working directory", func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].Service = &laboratoryv1alpha1.DeviceService{WorkingDir: "app"}
		}, nil, nil, "workingDir"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			item := &protobuf.LabItem{}
			if c.checks != nil {
				item.Checks = &protobuf.LabChecks{Items: c.checks}
			}
			variant := &protobuf.LabVariant{VariantId: "v", SpecJson: contractSpecJSON(t, c.mod, "web"), Bootstrap: c.bootstrap}
			item.LabGroup, item.Name, item.VariantId = "team-lab", "bad", "v"
			res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{variant}, Items: []*protobuf.LabItem{item}})
			invalidArg(t, res, err, c.want)
		})
	}
}

func TestChecksAndTerminalAreRefusedWhileOff(t *testing.T) {
	ctx := context.Background()
	terminalSpec := func(t *testing.T) []byte {
		return contractSpecJSON(t, func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].Access = []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessTerminal}}
		}, "web")
	}
	checks := &protobuf.LabChecks{Items: []*protobuf.Check{fileCheck("cfg", "web")}}
	send := func(h *Handler, spec []byte, ch *protobuf.LabChecks, name string) (*protobuf.BatchResult, error) {
		return h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
			Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: spec}},
			Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: name, VariantId: "v", Checks: ch}},
		})
	}

	off, _ := contractHandler(t, Features{})
	res, err := send(off, contractSpecJSON(t, nil, "web"), checks, "a")
	invalidArg(t, res, err, "checks option is off")
	res, err = send(off, terminalSpec(t), nil, "b")
	invalidArg(t, res, err, "terminal option is off")
	// a lab without either is fine, and bootstrap does not depend on either switch
	res, err = off.CreateLabs(ctx, &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: contractSpecJSON(t, nil, "web"), Bootstrap: []*protobuf.DeviceBootstrap{bootstrapOf("web")}}},
		Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: "plain", VariantId: "v"}}})
	wantStates(t, res, err, stCreated)

	// the two switches are independent
	checksOnly, _ := contractHandler(t, Features{ChecksEnabled: true})
	res, err = send(checksOnly, terminalSpec(t), nil, "c")
	invalidArg(t, res, err, "terminal option is off")
	res, err = send(checksOnly, contractSpecJSON(t, nil, "web"), checks, "d")
	wantStates(t, res, err, stCreated)

	terminalOnly, _ := contractHandler(t, Features{TerminalEnabled: true})
	res, err = send(terminalOnly, contractSpecJSON(t, nil, "web"), checks, "e")
	invalidArg(t, res, err, "checks option is off")
	res, err = send(terminalOnly, terminalSpec(t), nil, "f")
	wantStates(t, res, err, stCreated)
	lab, _ := terminalOnly.cs.LaboratoryV1alpha1().Labs("team-lab").Get(ctx, "f", metav1.GetOptions{})
	if tm := laboratoryv1alpha1.AccessOf(lab.Spec.Devices[0].Access, laboratoryv1alpha1.AccessTerminal); tm == nil {
		t.Fatalf("the terminal request must reach the spec: %+v", lab.Spec.Devices[0])
	}
}

func TestServiceCommandReachesTheSpec(t *testing.T) {
	h, _ := contractHandler(t, Features{})
	svc := &laboratoryv1alpha1.DeviceService{Command: []string{"/usr/sbin/nginx"}, Args: []string{"-g", "daemon off;"}, User: "www-data", WorkingDir: "/srv"}
	res := createContractLab(t, h, &protobuf.LabVariant{SpecJson: contractSpecJSON(t, func(s *laboratoryv1alpha1.LabSpec) { s.Devices[0].Service = svc }, "web")}, &protobuf.LabItem{})
	wantStates(t, res, nil, stCreated)
	lab, _ := h.cs.LaboratoryV1alpha1().Labs("team-lab").Get(context.Background(), "c1", metav1.GetOptions{})
	got := lab.Spec.Devices[0].Service
	if got == nil || strings.Join(got.Command, " ") != "/usr/sbin/nginx" || strings.Join(got.Args, " ") != "-g daemon off;" || got.User != "www-data" || got.WorkingDir != "/srv" {
		t.Fatalf("service: %+v", got)
	}
}

func TestRunChecks(t *testing.T) {
	h, _ := contractHandler(t, Features{ChecksEnabled: true})
	ctx := context.Background()
	quiet := fileCheck("quiet", "web")
	quiet.ManualRun = false
	res := createContractLab(t, h, &protobuf.LabVariant{SpecJson: contractSpecJSON(t, nil, "web")},
		&protobuf.LabItem{Checks: &protobuf.LabChecks{Items: []*protobuf.Check{fileCheck("a", "web"), fileCheck("b", "web"), quiet}}})
	wantStates(t, res, nil, stCreated)
	plain, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{{VariantId: "p", SpecJson: contractSpecJSON(t, nil, "web")}}, Items: []*protobuf.LabItem{{LabGroup: "team-lab", Name: "plain", VariantId: "p"}}})
	wantStates(t, plain, err, stCreated)

	annotation := func(name string) runChecksRequest {
		t.Helper()
		lab, err := h.cs.LaboratoryV1alpha1().Labs("team-lab").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var r runChecksRequest
		if raw := lab.Annotations[names.AnnotationRunChecks]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &r); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	run := func(lab string, ids ...string) *protobuf.ItemResult {
		t.Helper()
		out, err := h.RunChecks(ctx, &protobuf.RunChecksRequest{Items: []*protobuf.RunChecksItem{{LabGroup: "team-lab", Lab: lab, CheckIds: ids}}})
		if err != nil {
			t.Fatal(err)
		}
		return out.Results[0]
	}

	if r := run("c1", "b"); r.State != stUpdated {
		t.Fatalf("run b: %v", r)
	}
	if got := annotation("c1"); got.Token != 1 || strings.Join(got.IDs, ",") != "b" {
		t.Fatalf("first request: %+v", got)
	}
	// not taken yet: the next request is merged, the token still grows
	if r := run("c1", "a"); r.State != stUpdated {
		t.Fatalf("run a: %v", r)
	}
	if got := annotation("c1"); got.Token != 2 || strings.Join(got.IDs, ",") != "a,b" {
		t.Fatalf("merged request: %+v", got)
	}
	// "all" wins over a list
	if r := run("c1"); r.State != stUpdated {
		t.Fatalf("run all: %v", r)
	}
	if got := annotation("c1"); got.Token != 3 || len(got.IDs) != 0 {
		t.Fatalf("all: %+v", got)
	}
	// the checker takes the request: the next one stands alone and the token never goes back
	lab, _ := h.cs.LaboratoryV1alpha1().Labs("team-lab").Get(ctx, "c1", metav1.GetOptions{})
	lab.Status.ChecksRunAck = 3
	if _, err := h.cs.LaboratoryV1alpha1().Labs("team-lab").UpdateStatus(ctx, lab, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := run("c1", "a"); r.State != stUpdated {
		t.Fatalf("run a again: %v", r)
	}
	if got := annotation("c1"); got.Token != 4 || strings.Join(got.IDs, ",") != "a" {
		t.Fatalf("after the acknowledgement: %+v", got)
	}

	// refusals are per lab
	if r := run("c1", "quiet"); r.State != stFailed || !strings.Contains(r.Error, "does not allow a manual run") {
		t.Fatalf("manual run not allowed: %v", r)
	}
	if r := run("c1", "ghost"); r.State != stFailed || !strings.Contains(r.Error, "unknown check") {
		t.Fatalf("unknown check: %v", r)
	}
	if r := run("plain"); r.State != stFailed || !strings.Contains(r.Error, "no checks") {
		t.Fatalf("a lab without checks: %v", r)
	}
	if r := run("missing"); r.State != stNotFound {
		t.Fatalf("a missing lab: %v", r)
	}
	if annotation("c1").Token != 4 {
		t.Fatal("a refused request must not move the token")
	}
	if _, err := h.RunChecks(ctx, &protobuf.RunChecksRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no items: %v", err)
	}
	if out, err := h.RunChecks(ctx, &protobuf.RunChecksRequest{Items: []*protobuf.RunChecksItem{{LabGroup: "team-lab", Lab: "c1"}, {LabGroup: "team-lab", Lab: "c1"}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a lab listed twice: %v %v", out, err)
	}
}

func TestRunChecksIsRefusedWhileChecksAreOff(t *testing.T) {
	h, _ := contractHandler(t, Features{})
	_, err := h.RunChecks(context.Background(), &protobuf.RunChecksRequest{Items: []*protobuf.RunChecksItem{{LabGroup: "team-lab", Lab: "c1"}}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v", err)
	}
}

// The features report carries both switches, independent, and the limits.
func TestFeaturesReportChecksAndAccess(t *testing.T) {
	for _, c := range []struct{ checks, terminal, served bool }{{false, false, false}, {true, false, false}, {false, true, false}, {true, true, false}, {true, true, true}, {false, false, true}} {
		f := testFeatures
		f.ChecksEnabled, f.TerminalEnabled, f.TerminalServed = c.checks, c.terminal, c.served
		h := featuresHandler(t, f, newTenantTenant("a", true, nil))
		got, err := h.GetFeatures(asClient("a"), &protobuf.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		acc := got.GetAccess()
		if got.GetChecks().GetEnabled() != c.checks || acc.GetTerminal() != (c.terminal && c.served) || !acc.GetHttp() || acc.GetVnc() || acc.GetRdp() {
			t.Errorf("%+v: reported checks %v, access %+v (only http is always served, terminal needs the option and the component, vnc and rdp never)", c, got.GetChecks().GetEnabled(), acc)
		}
		// file transfer is possible exactly where the console is served
		if acc.GetTerminalFiles() != (c.terminal && c.served) {
			t.Errorf("%+v: terminal_files %v", c, acc.GetTerminalFiles())
		}
		if ch := got.GetChecks(); ch.GetMaxChecksPerLab() != labchecks.MaxChecksPerLab || ch.GetMaxTimeLimitSeconds() != labchecks.MaxCheckTimeLimit || ch.GetMaxDetailBytes() != labchecks.MaxResultDetailBytes {
			t.Errorf("checks limits: %+v", ch)
		}
		if b := got.GetBootstrap(); b.GetMaxFiles() != labchecks.MaxBootstrapFiles || b.GetMaxScriptBytes() != labchecks.MaxScriptBytes || b.GetMaxAttempts() != 3 {
			t.Errorf("bootstrap limits: %+v", b)
		}
	}
}

// ---- projection ----------------------------------------------------------------------------------------------------

func TestLabProjectionCarriesChecksBootstrapAndTerminal(t *testing.T) {
	long := strings.Repeat("é", labchecks.MaxResultDetailBytes) // two bytes per rune
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns"}}
	lab.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Access: []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessTerminal}}}}
	lab.Status.Devices = []laboratoryv1alpha1.DeviceRef{{Name: "web", Bootstrap: &laboratoryv1alpha1.DeviceBootstrapStatus{Phase: laboratoryv1alpha1.BootstrapFailed, Attempts: 3, Reason: "script exited 2"}}}
	lab.Status.Access = []laboratoryv1alpha1.AccessEntry{{Device: "web", Port: 8080, Protocol: "https", URL: "https://web.example"}, {Device: "web", Protocol: "terminal"}}
	lab.Status.ChecksRunAck = 7
	lab.Status.Checks = []laboratoryv1alpha1.CheckResult{
		{ID: "b", State: laboratoryv1alpha1.CheckNotPassed, ChangedMs: 20, Detail: long, Value: "v"},
		{ID: "a", State: laboratoryv1alpha1.CheckPassed, FirstPassedMs: 10, ChangedMs: 10},
		{ID: "c", State: laboratoryv1alpha1.CheckError},
		{ID: "d"},
	}
	p := labMonitoringToProto(lab, "g")
	st := p.GetStatus()
	if b := st.GetDevices()[0].GetBootstrap(); b.GetPhase() != protobuf.BootstrapPhase_BOOTSTRAP_PHASE_FAILED || b.GetAttempts() != 3 || b.GetReason() != "script exited 2" {
		t.Fatalf("bootstrap status: %+v", b)
	}
	if len(st.GetAccess()) != 2 || st.GetAccess()[1].GetProtocol() != "terminal" || st.GetAccess()[1].GetUrl() != "" || st.GetAccess()[1].GetPort() != 0 {
		t.Fatalf("access: %+v", st.GetAccess())
	}
	if len(st.GetAccessUrls()) != 1 || st.GetAccessUrls()[0] != "https://web.example" {
		t.Fatalf("the terminal entry has no link yet: %v", st.GetAccessUrls())
	}
	if st.GetChecksRunAck() != 7 || len(st.GetChecks()) != 4 {
		t.Fatalf("checks: %+v", st)
	}
	byID := map[string]*protobuf.CheckResult{}
	for _, c := range st.GetChecks() {
		byID[c.GetId()] = c
	}
	if byID["a"].GetState() != protobuf.CheckState_CHECK_STATE_PASSED || byID["a"].GetFirstPassedUnixMs() != 10 ||
		byID["b"].GetState() != protobuf.CheckState_CHECK_STATE_NOT_PASSED || byID["b"].GetChangedUnixMs() != 20 || byID["b"].GetValue() != "v" ||
		byID["c"].GetState() != protobuf.CheckState_CHECK_STATE_ERROR || byID["d"].GetState() != protobuf.CheckState_CHECK_STATE_UNKNOWN {
		t.Fatalf("states: %+v", byID)
	}
	if d := byID["b"].GetDetail(); len(d) > labchecks.MaxResultDetailBytes || len(d) == 0 || !strings.HasPrefix(long, d) {
		t.Fatalf("detail must be cut to %d bytes on a rune boundary, got %d", labchecks.MaxResultDetailBytes, len(d))
	}
	// the answer is the same whatever order the CR lists the results in
	if st.GetChecks()[0].GetId() != "a" {
		t.Fatalf("results are sorted by id: %v", st.GetChecks())
	}
}

// Only changes are published: the monitoring projection of an unchanged status is equal, a changed result differs.
func TestCheckResultProjectionIsStable(t *testing.T) {
	mk := func(state laboratoryv1alpha1.CheckState) *protobuf.Lab {
		lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns"}}
		lab.Status.Checks = []laboratoryv1alpha1.CheckResult{{ID: "a", State: state, ChangedMs: 5}}
		return labMonitoringToProto(lab, "g")
	}
	if !proto.Equal(mk(laboratoryv1alpha1.CheckPassed), mk(laboratoryv1alpha1.CheckPassed)) {
		t.Fatal("equal statuses must project equal")
	}
	if proto.Equal(mk(laboratoryv1alpha1.CheckPassed), mk(laboratoryv1alpha1.CheckNotPassed)) {
		t.Fatal("a changed result must change the projection")
	}
}
