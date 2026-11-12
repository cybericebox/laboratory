package checker

import (
	"context"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/probe"
)

func i32p(v int32) *int32 { return &v }

type verdictCase struct {
	name string
	got  func() (Outcome, []string)
	want Outcome
	hint string
}

func runVerdicts(t *testing.T, cases []verdictCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reasons := c.got()
			if got != c.want || (c.hint != "" && !strings.Contains(strings.Join(reasons, ";"), c.hint)) {
				t.Fatalf("got %v %v, want %v containing %q", got, reasons, c.want, c.hint)
			}
		})
	}
}

func TestEvalFileTypeAndChecksum(t *testing.T) {
	content := present("hello")
	content.Type = "file"
	sum := sha256Of([]byte("hello"))
	big := content
	big.Truncated = true
	dir := FileFacts{Exists: true, Type: "directory"}
	unknown := present("x")
	runVerdicts(t, []verdictCase{
		{"type yes", func() (Outcome, []string) {
			return EvalFile(&labchecks.FilePart{Path: "/f", Type: sp("file")}, content)
		}, Passed, ""},
		{"type no", func() (Outcome, []string) {
			return EvalFile(&labchecks.FilePart{Path: "/f", Type: sp("directory")}, content)
		}, NotPassed, "type: is file"},
		{"directory", func() (Outcome, []string) {
			return EvalFile(&labchecks.FilePart{Path: "/f", Type: sp("directory")}, dir)
		}, Passed, ""},
		{"type unknown to the reader", func() (Outcome, []string) {
			return EvalFile(&labchecks.FilePart{Path: "/f", Type: sp("file")}, unknown)
		}, Failed, "cannot tell"},
		{"checksum yes", func() (Outcome, []string) { return EvalFile(&labchecks.FilePart{Path: "/f", Checksum: &sum}, content) }, Passed, ""},
		{"checksum no", func() (Outcome, []string) {
			return EvalFile(&labchecks.FilePart{Path: "/f", Checksum: sp(strings.Repeat("0", 64))}, content)
		}, NotPassed, "checksum: differs"},
		{"checksum of a big file cannot be decided", func() (Outcome, []string) { return EvalFile(&labchecks.FilePart{Path: "/f", Checksum: &sum}, big) }, Failed, "larger"},
		{"checksum of a directory", func() (Outcome, []string) { return EvalFile(&labchecks.FilePart{Path: "/f", Checksum: &sum}, dir) }, NotPassed, "regular"},
	})
	if sum != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("sha256 of hello: %s", sum)
	}
}

var dirKey = []byte("0123456789abcdef0123456789abcdef")

func dirFacts(names ...string) *probe.DirectoryFacts {
	f := &probe.DirectoryFacts{Exists: true, IsDir: true, Mode: 0o750, User: "www", Group: "www"}
	for _, n := range names {
		f.Hashes = append(f.Hashes, probe.EntryHash(dirKey, n))
	}
	return f
}

func TestEvalDirectory(t *testing.T) {
	d := dirFacts("config.php", "uploads")
	cut := dirFacts("a", "b")
	cut.Truncated = true
	file := &probe.DirectoryFacts{Exists: true}
	e := func(p labchecks.DirectoryPart, f *probe.DirectoryFacts) func() (Outcome, []string) {
		return func() (Outcome, []string) { return EvalDirectory(&p, f, dirKey) }
	}
	runVerdicts(t, []verdictCase{
		{"exists", e(labchecks.DirectoryPart{Path: "/d"}, d), Passed, ""},
		{"missing", e(labchecks.DirectoryPart{Path: "/d"}, &probe.DirectoryFacts{}), NotPassed, "missing"},
		{"a file is no directory", e(labchecks.DirectoryPart{Path: "/d"}, file), NotPassed, "not a directory"},
		{"absent and absent", e(labchecks.DirectoryPart{Path: "/d", Exists: bp(false)}, &probe.DirectoryFacts{}), Passed, ""},
		{"absent but there", e(labchecks.DirectoryPart{Path: "/d", Exists: bp(false)}, d), NotPassed, "present"},
		{"mode", e(labchecks.DirectoryPart{Path: "/d", Mode: mp(0o750)}, d), Passed, ""},
		{"mode no", e(labchecks.DirectoryPart{Path: "/d", Mode: mp(0o700)}, d), NotPassed, "mode: is 0750"},
		{"owner and group", e(labchecks.DirectoryPart{Path: "/d", Owner: sp("www"), Group: sp("www")}, d), Passed, ""},
		{"owner no", e(labchecks.DirectoryPart{Path: "/d", Owner: sp("root")}, d), NotPassed, "owner"},
		{"has entries", e(labchecks.DirectoryPart{Path: "/d", HasEntries: []string{"config.php", "uploads"}}, d), Passed, ""},
		{"has entries missing", e(labchecks.DirectoryPart{Path: "/d", HasEntries: []string{"config.php", "install.php"}}, d), NotPassed, "has_entries"},
		{"lacks entries", e(labchecks.DirectoryPart{Path: "/d", LacksEntries: []string{"install.php"}}, d), Passed, ""},
		{"lacks entries present", e(labchecks.DirectoryPart{Path: "/d", LacksEntries: []string{"uploads"}}, d), NotPassed, "lacks_entries"},
		{"hashes made with another key match nothing", e(labchecks.DirectoryPart{Path: "/d", LacksEntries: []string{"uploads"}}, func() *probe.DirectoryFacts {
			f := &probe.DirectoryFacts{Exists: true, IsDir: true}
			f.Hashes = append(f.Hashes, probe.EntryHash([]byte("another key, another key, another"), "uploads"))
			return f
		}()), Passed, ""},
		// A listing cut at the cap says nothing about the names it did not reach: both forms are undecided, unless what was seen decides.
		{"cut listing, lacks: not seen", e(labchecks.DirectoryPart{Path: "/d", LacksEntries: []string{"forbidden.php"}}, cut), Failed, "more entries"},
		{"cut listing, lacks: seen", e(labchecks.DirectoryPart{Path: "/d", LacksEntries: []string{"a"}}, cut), NotPassed, "lacks_entries"},
		{"cut listing, has: seen", e(labchecks.DirectoryPart{Path: "/d", HasEntries: []string{"a", "b"}}, cut), Passed, ""},
		{"cut listing, has: not seen", e(labchecks.DirectoryPart{Path: "/d", HasEntries: []string{"z"}}, cut), Failed, "more entries"},
		{"cut listing, has not seen but mode wrong is decided", e(labchecks.DirectoryPart{Path: "/d", HasEntries: []string{"z"}, Mode: mp(0o700)}, cut), NotPassed, "mode"},
		{"cut listing, a seen forbidden name beats the doubt", e(labchecks.DirectoryPart{Path: "/d", HasEntries: []string{"z"}, LacksEntries: []string{"a"}}, cut), NotPassed, "lacks_entries"},
		{"cut listing, no entry rule, nothing is in doubt", e(labchecks.DirectoryPart{Path: "/d", Mode: mp(0o750)}, cut), Passed, ""},
	})
}

func TestEvalProcess(t *testing.T) {
	list := []probe.Process{
		{PID: 1, Comm: "nginx", Arg0: "nginx", Cmdline: "nginx: master process /usr/sbin/nginx -g daemon off;", User: "root"},
		{PID: 7, Comm: "nginx", Arg0: "nginx", Cmdline: "nginx: worker process", UID: 33, User: "www-data"},
		{PID: 9, Comm: "postgresql-16-ma", Arg0: "postgresql-16-main", Cmdline: "/usr/lib/postgresql/16/bin/postgres", User: "postgres", UID: 70},
	}
	full := &probe.ProcessFacts{List: list}
	cutTable := &probe.ProcessFacts{List: list, Truncated: true}
	unreadable := &probe.ProcessFacts{List: list, Incomplete: 1}
	withLong := &probe.ProcessFacts{List: append(append([]probe.Process(nil), list...), probe.Process{PID: 11, Comm: "java", Arg0: "java", Cmdline: "java -jar a", Cut: true, User: "app"})}
	e := func(p labchecks.ProcessPart, f *probe.ProcessFacts) func() (Outcome, []string) {
		return func() (Outcome, []string) { return EvalProcess(&p, f) }
	}
	runVerdicts(t, []verdictCase{
		{"by name", e(labchecks.ProcessPart{Name: sp("nginx")}, full), Passed, ""},
		{"by a long name the kernel cut", e(labchecks.ProcessPart{Name: sp("postgresql-16-main")}, full), Passed, ""},
		{"by pattern", e(labchecks.ProcessPart{Matches: sp(`nginx: master process`)}, full), Passed, ""},
		{"by pattern none", e(labchecks.ProcessPart{Matches: sp(`apache`)}, full), NotPassed, "no such process"},
		{"by user", e(labchecks.ProcessPart{Name: sp("nginx"), User: sp("www-data")}, full), Passed, ""},
		{"by uid", e(labchecks.ProcessPart{Name: sp("nginx"), User: sp("33")}, full), Passed, ""},
		{"name and user together, no one process has both", e(labchecks.ProcessPart{Name: sp("postgresql-16-main"), User: sp("root")}, full), NotPassed, "no such process"},
		{"must not run, does not", e(labchecks.ProcessPart{Name: sp("telnetd"), Running: bp(false)}, full), Passed, ""},
		{"must not run, runs", e(labchecks.ProcessPart{Name: sp("nginx"), Running: bp(false)}, full), NotPassed, "such a process runs"},
		// Neither a cut table, nor an unreadable entry, nor a command line too long to judge can show that something is absent.
		{"must not run, not found in a cut table", e(labchecks.ProcessPart{Name: sp("telnetd"), Running: bp(false)}, cutTable), Failed, "could not be judged"},
		{"must run, not found in a cut table", e(labchecks.ProcessPart{Name: sp("telnetd")}, cutTable), Failed, "could not be judged"},
		{"must not run, not found, an entry was unreadable", e(labchecks.ProcessPart{Name: sp("telnetd"), Running: bp(false)}, unreadable), Failed, "could not be judged"},
		{"must run, found, in a cut table", e(labchecks.ProcessPart{Name: sp("nginx")}, cutTable), Passed, ""},
		{"must not run, found, in a cut table", e(labchecks.ProcessPart{Name: sp("nginx"), Running: bp(false)}, cutTable), NotPassed, "runs"},
		{"pattern on a command line that was cut: not judged", e(labchecks.ProcessPart{Matches: sp(`-jar a$`)}, withLong), Failed, "could not be judged"},
		{"pattern on a command line that was cut, another process matches", e(labchecks.ProcessPart{Matches: sp(`nginx: worker`)}, withLong), Passed, ""},
		{"name of a process whose command line was cut: the name is whole", e(labchecks.ProcessPart{Name: sp("java")}, withLong), Passed, ""},
		{"name that does not match a cut one", e(labchecks.ProcessPart{Name: sp("telnetd"), Running: bp(false)}, withLong), Passed, ""},
		{"bad pattern", e(labchecks.ProcessPart{Matches: sp("(")}, full), Failed, "compile"},
	})
}

func TestEvalPort(t *testing.T) {
	two := &probe.PortFacts{Listeners: []probe.Listener{
		{Protocol: "tcp", Addr: "127.0.0.1", Procs: []string{"redis-server"}, Arg0s: []string{"redis-server"}},
	}}
	wild := &probe.PortFacts{Listeners: []probe.Listener{{Protocol: "tcp", Addr: "0.0.0.0", Procs: []string{"nginx"}}}}
	wild6 := &probe.PortFacts{Listeners: []probe.Listener{{Protocol: "tcp", Addr: "::"}}}
	none := &probe.PortFacts{Listeners: []probe.Listener{}}
	capped := &probe.PortFacts{Listeners: []probe.Listener{{Addr: "10.0.0.1"}}, Truncated: true}
	blind := &probe.PortFacts{Listeners: []probe.Listener{{Addr: "0.0.0.0"}}, OwnersIncomplete: true}
	e := func(p labchecks.PortPart, f *probe.PortFacts) func() (Outcome, []string) {
		return func() (Outcome, []string) { return EvalPort(&p, f) }
	}
	runVerdicts(t, []verdictCase{
		{"listening", e(labchecks.PortPart{Port: 80}, wild), Passed, ""},
		{"nothing listens", e(labchecks.PortPart{Port: 80}, none), NotPassed, "nothing listens"},
		{"must be closed, is", e(labchecks.PortPart{Port: 80, Listening: bp(false)}, wild), NotPassed, "something listens"},
		{"must be closed, is", e(labchecks.PortPart{Port: 80, Listening: bp(false)}, none), Passed, ""},
		{"address exact", e(labchecks.PortPart{Port: 6379, Address: sp("127.0.0.1")}, two), Passed, ""},
		{"address other", e(labchecks.PortPart{Port: 6379, Address: sp("10.0.0.5")}, two), NotPassed, "address"},
		{"wildcard counts for any ipv4", e(labchecks.PortPart{Port: 80, Address: sp("10.0.0.5")}, wild), Passed, ""},
		{"ipv4 wildcard is not ipv6", e(labchecks.PortPart{Port: 80, Address: sp("::1")}, wild), NotPassed, "address"},
		{"ipv6 wildcard counts for both", e(labchecks.PortPart{Port: 80, Address: sp("10.0.0.5")}, wild6), Passed, ""},
		{"process", e(labchecks.PortPart{Port: 80, Process: sp("nginx")}, wild), Passed, ""},
		{"process other", e(labchecks.PortPart{Port: 80, Process: sp("apache2")}, wild), NotPassed, "process"},
		{"must be closed, the table was cut, a listener was seen", e(labchecks.PortPart{Port: 80, Listening: bp(false)}, capped), NotPassed, "something listens"},
		{"must listen on an address, the table was cut and no listener fits", e(labchecks.PortPart{Port: 80, Address: sp("10.9.9.9")}, capped), Failed, "more sockets"},
		{"must listen, the table was cut, a listener fits", e(labchecks.PortPart{Port: 80, Address: sp("10.0.0.1")}, capped), Passed, ""},
		{"process asked, some processes could not be looked into", e(labchecks.PortPart{Port: 80, Process: sp("nginx")}, blind), Failed, "could not be looked into"},
		{"process asked and found, though some could not be looked into", e(labchecks.PortPart{Port: 80, Process: sp("nginx")}, &probe.PortFacts{Listeners: []probe.Listener{{Addr: "0.0.0.0", Procs: []string{"nginx"}}}, OwnersIncomplete: true}), Passed, ""},
		{"address and process on the same listener", e(labchecks.PortPart{Port: 6379, Address: sp("127.0.0.1"), Process: sp("redis-server")}, two), Passed, ""},
		{"address and process on different listeners do not add up", e(labchecks.PortPart{Port: 80, Address: sp("0.0.0.0"), Process: sp("redis-server")}, &probe.PortFacts{Listeners: []probe.Listener{
			{Addr: "0.0.0.0", Procs: []string{"nginx"}}, {Addr: "127.0.0.1", Procs: []string{"redis-server"}}}}), NotPassed, "process"},
	})
}

func TestEvalKernelParam(t *testing.T) {
	f := &probe.KernelParamFacts{Exists: true, Value: "1"}
	e := func(p labchecks.KernelParamPart, f *probe.KernelParamFacts) func() (Outcome, []string) {
		return func() (Outcome, []string) { return EvalKernelParam(&p, f) }
	}
	runVerdicts(t, []verdictCase{
		{"equals", e(labchecks.KernelParamPart{Name: "n", Equals: sp("1")}, f), Passed, ""},
		{"equals with spaces", e(labchecks.KernelParamPart{Name: "n", Equals: sp(" 1\n")}, f), Passed, ""},
		{"equals no", e(labchecks.KernelParamPart{Name: "n", Equals: sp("0")}, f), NotPassed, "equals"},
		{"matches", e(labchecks.KernelParamPart{Name: "n", Matches: sp("^[12]$")}, f), Passed, ""},
		{"matches no", e(labchecks.KernelParamPart{Name: "n", Matches: sp("^2$")}, f), NotPassed, "matches"},
		{"both", e(labchecks.KernelParamPart{Name: "n", Equals: sp("1"), Matches: sp("^2$")}, f), NotPassed, "matches"},
		{"missing", e(labchecks.KernelParamPart{Name: "n", Equals: sp("1")}, &probe.KernelParamFacts{}), NotPassed, "does not exist"},
		{"bad pattern", e(labchecks.KernelParamPart{Name: "n", Matches: sp("(")}, f), Failed, "compile"},
	})
}

func TestEvalRequest(t *testing.T) {
	ok := &probe.RequestFacts{Connected: true, Status: 200, Body: []byte(`{"state": "up"}`)}
	cut := &probe.RequestFacts{Connected: true, Status: 200, Body: []byte("abc"), Truncated: true}
	refused := &probe.RequestFacts{Failure: "refused"}
	timedOut := &probe.RequestFacts{Failure: "timeout"}
	e := func(p labchecks.RequestPart, f *probe.RequestFacts) func() (Outcome, []string) {
		return func() (Outcome, []string) { return EvalRequest(&p, f) }
	}
	h := func(status int32) labchecks.RequestPart {
		return labchecks.RequestPart{Protocol: "http", Host: "127.0.0.1", Port: 80, Method: "GET", Path: "/", Status: i32p(status)}
	}
	withBody := func(contains, matches *string) labchecks.RequestPart {
		r := h(200)
		r.BodyContains, r.BodyMatches = contains, matches
		return r
	}
	runVerdicts(t, []verdictCase{
		{"tcp opens", e(labchecks.RequestPart{Protocol: "tcp", Port: 22}, &probe.RequestFacts{Connected: true}), Passed, ""},
		{"tcp refused", e(labchecks.RequestPart{Protocol: "tcp", Port: 22}, refused), NotPassed, "refused"},
		{"tcp times out", e(labchecks.RequestPart{Protocol: "tcp", Port: 22}, timedOut), Failed, "no answer in time"},
		{"tcp, an unnamed error", e(labchecks.RequestPart{Protocol: "tcp", Port: 22}, &probe.RequestFacts{}), Failed, "unknown error"},
		{"tcp, the network is unreachable", e(labchecks.RequestPart{Protocol: "tcp", Port: 22}, &probe.RequestFacts{Failure: "unreachable"}), NotPassed, "unreachable"},
		{"http times out", e(h(200), timedOut), Failed, "no answer in time"},
		{"http status", e(h(200), ok), Passed, ""},
		{"http other status", e(h(204), ok), NotPassed, "status: is 200"},
		{"http refused", e(h(200), refused), NotPassed, "connect: failed (refused)"},
		{"body contains", e(withBody(sp("up"), nil), ok), Passed, ""},
		{"body contains no", e(withBody(sp("down"), nil), ok), NotPassed, "body_contains"},
		{"body matches", e(withBody(nil, sp(`"state":\s*"up"`)), ok), Passed, ""},
		{"body matches no", e(withBody(nil, sp(`down`)), ok), NotPassed, "body_matches"},
		{"cut body decides nothing: contains found", e(withBody(sp("ab"), nil), cut), Failed, "longer"},
		{"cut body decides nothing: contains not found", e(withBody(sp("zzz"), nil), cut), Failed, "longer"},
		{"cut body decides nothing: matches (an anchor would fit the cut)", e(withBody(nil, sp(`abc$`)), cut), Failed, "longer"},
		{"cut body, no body rule, status is enough", e(h(200), cut), Passed, ""},
		{"cut body, wrong status is decided", e(func() labchecks.RequestPart { r := withBody(sp("zzz"), nil); r.Status = i32p(500); return r }(), cut), NotPassed, "status"},
		{"bad pattern", e(withBody(nil, sp("(")), ok), Failed, "compile"},
	})
}

func TestEvalPermission(t *testing.T) {
	f := &probe.PermissionFacts{UserFound: true, Exists: true, Read: true}
	e := func(p labchecks.PermissionPart, f *probe.PermissionFacts) func() (Outcome, []string) {
		return func() (Outcome, []string) { return EvalPermission(&p, f) }
	}
	runVerdicts(t, []verdictCase{
		{"may read", e(labchecks.PermissionPart{User: "u", Path: "/p", Read: bp(true)}, f), Passed, ""},
		{"may not write", e(labchecks.PermissionPart{User: "u", Path: "/p", Write: bp(false)}, f), Passed, ""},
		{"all three", e(labchecks.PermissionPart{User: "u", Path: "/p", Read: bp(true), Write: bp(false), Execute: bp(false)}, f), Passed, ""},
		{"must not read, may", e(labchecks.PermissionPart{User: "u", Path: "/p", Read: bp(false)}, f), NotPassed, "read: is true"},
		{"a key left out is not tested", e(labchecks.PermissionPart{User: "u", Path: "/p", Read: bp(true)}, &probe.PermissionFacts{UserFound: true, Exists: true, Read: true, Write: true}), Passed, ""},
		{"no such user", e(labchecks.PermissionPart{User: "u", Path: "/p", Read: bp(false)}, &probe.PermissionFacts{}), NotPassed, "user"},
		{"a missing path does not pass 'may not read'", e(labchecks.PermissionPart{User: "u", Path: "/p", Read: bp(false)}, &probe.PermissionFacts{UserFound: true}), NotPassed, "path: missing"},
	})
}

// recordingProber answers from a function and records the questions.
type recordingProber struct {
	fakeObserver
	asked []probe.ObserveRequest
	reply func(probe.ObserveRequest) (probe.ObserveResponse, error)
}

func (r *recordingProber) Probe(_ context.Context, q probe.ObserveRequest) (probe.ObserveResponse, error) {
	r.asked = append(r.asked, q)
	return r.reply(q)
}

func TestRunCheckBuiltinsAreJudgedByTheChecker(t *testing.T) {
	obs := &recordingProber{reply: func(q probe.ObserveRequest) (probe.ObserveResponse, error) {
		switch q.Kind {
		case probe.KindDirectory:
			return probe.ObserveResponse{Directory: dirFactsFor(q.Key, "a")}, nil
		case probe.KindKernelParam:
			return probe.ObserveResponse{KernelParam: &probe.KernelParamFacts{Exists: true, Value: "1"}}, nil
		case probe.KindPort:
			return probe.ObserveResponse{Port: &probe.PortFacts{Listeners: []probe.Listener{{Addr: "0.0.0.0"}}}}, nil
		}
		return probe.ObserveResponse{Error: "no"}, nil
	}}
	ok := check(
		labchecks.Part{Directory: &labchecks.DirectoryPart{Path: "/srv", HasEntries: []string{"a"}}},
		labchecks.Part{KernelParam: &labchecks.KernelParamPart{Name: "net.ipv4.ip_forward", Equals: sp("1")}},
		labchecks.Part{Port: &labchecks.PortPart{Port: 80, Protocol: "tcp"}})
	if r := RunCheck(context.Background(), ok, obs); r.Outcome != Passed || len(obs.asked) != 3 {
		t.Fatalf("got %+v after %d questions", r, len(obs.asked))
	}
	bad := check(
		labchecks.Part{KernelParam: &labchecks.KernelParamPart{Name: "net.ipv4.ip_forward", Equals: sp("0")}},
		labchecks.Part{File: &labchecks.FilePart{Path: "/f", Exists: bp(true)}})
	obs.files = map[string]FileFacts{"/f": present("x")}
	r := RunCheck(context.Background(), bad, obs)
	if r.Outcome != NotPassed || r.Detail != "part 1 (kernel_param): equals: another value" {
		t.Fatalf("got %+v", r)
	}
	// The device could not answer: the check could not be made.
	r = RunCheck(context.Background(), check(labchecks.Part{Process: &labchecks.ProcessPart{Name: sp("x")}}), obs)
	if r.Outcome != Failed || !strings.Contains(r.Detail, "could not answer") {
		t.Fatalf("got %+v", r)
	}
	// An observer without the ability.
	r = RunCheck(context.Background(), check(labchecks.Part{Process: &labchecks.ProcessPart{Name: sp("x")}}), &fakeObserver{})
	if r.Outcome != Failed || !strings.Contains(r.Detail, "cannot run built-in") {
		t.Fatalf("got %+v", r)
	}
}

// dirFactsFor answers a directory question with the hashes under the key the checker sent.
func dirFactsFor(key []byte, names ...string) *probe.DirectoryFacts {
	f := &probe.DirectoryFacts{Exists: true, IsDir: true}
	for _, n := range names {
		f.Hashes = append(f.Hashes, probe.EntryHash(key, n))
	}
	return f
}
