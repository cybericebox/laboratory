package labchecks

import (
	"strings"
	"testing"
)

func u32(v uint32) *uint32 { return &v }
func i32(v int32) *int32   { return &v }

func checkWith(p Part) []Check {
	c := goodCheck()
	c.Parts = []Part{p}
	return []Check{c}
}

func TestBuiltinDefaults(t *testing.T) {
	checks := checkWith(Part{Process: &ProcessPart{Name: s("nginx")}})
	checks = append(checks, checkWith(Part{Port: &PortPart{Port: 80}})...)
	checks[1].ID = "p"
	checks = append(checks, checkWith(Part{Request: &RequestPart{Protocol: "http", Port: 8080}})...)
	checks[2].ID = "r"
	checks = append(checks, checkWith(Part{Request: &RequestPart{Protocol: "tcp", Port: 22}})...)
	checks[3].ID = "t"
	NormalizeChecks(checks)
	if r := checks[0].Parts[0].Process; r.Running == nil || !*r.Running {
		t.Errorf("running defaults to true: %+v", r)
	}
	if po := checks[1].Parts[0].Port; po.Protocol != "tcp" || po.Listening == nil || !*po.Listening {
		t.Errorf("port: %+v", po)
	}
	if r := checks[2].Parts[0].Request; r.Host != "127.0.0.1" || r.Method != "GET" || r.Path != "/" || r.Status == nil || *r.Status != 200 {
		t.Errorf("http request: %+v", r)
	}
	if r := checks[3].Parts[0].Request; r.Host != "127.0.0.1" || r.Method != "" || r.Status != nil {
		t.Errorf("a tcp request takes no http defaults: %+v", r)
	}
	if err := ValidateChecks(checks, devices); err != nil {
		t.Fatal(err)
	}
	if checks[0].Parts[0].Custom != nil {
		t.Fatal("unexpected")
	}
	// A custom command is for nobody unless the author says otherwise.
	cu := checkWith(Part{Custom: &CustomPart{Command: []string{"true"}}})
	NormalizeChecks(cu)
	if cu[0].Parts[0].Custom.User != "nobody" {
		t.Errorf("default user %q", cu[0].Parts[0].Custom.User)
	}
	cu = checkWith(Part{Custom: &CustomPart{Command: []string{"true"}, User: "root"}})
	NormalizeChecks(cu)
	if cu[0].Parts[0].Custom.User != "root" {
		t.Errorf("root is an explicit choice that stays: %q", cu[0].Parts[0].Custom.User)
	}
}

func TestValidateBuiltinRefusals(t *testing.T) {
	long := strings.Repeat("a", MaxEntryNameBytes+1)
	many := make([]string, MaxEntryNames+1)
	for i := range many {
		many[i] = "f"
	}
	for _, c := range []struct {
		name string
		part Part
		want string // empty: valid
	}{
		{"file type", Part{File: &FilePart{Path: "/a", Type: s("symlink")}}, ""},
		{"file bad type", Part{File: &FilePart{Path: "/a", Type: s("socket")}}, "not file, directory or symlink"},
		{"file checksum", Part{File: &FilePart{Path: "/a", Checksum: s(strings.Repeat("a", 64))}}, ""},
		{"file bad checksum", Part{File: &FilePart{Path: "/a", Checksum: s("ABC")}}, "64 lower-case hex"},
		{"absent file with a type", Part{File: &FilePart{Path: "/a", Exists: b(false), Type: s("file")}}, "absent"},

		{"directory minimal", Part{Directory: &DirectoryPart{Path: "/srv"}}, ""},
		{"directory entries", Part{Directory: &DirectoryPart{Path: "/srv", HasEntries: []string{"a"}, LacksEntries: []string{"b"}, Mode: u32(0o750), Owner: s("www"), Group: s("0")}}, ""},
		{"directory relative", Part{Directory: &DirectoryPart{Path: "srv"}}, "not absolute"},
		{"directory absent with entries", Part{Directory: &DirectoryPart{Path: "/srv", Exists: b(false), HasEntries: []string{"a"}}}, "absent"},
		{"directory entry with a slash", Part{Directory: &DirectoryPart{Path: "/srv", HasEntries: []string{"a/b"}}}, "not the name of a directory entry"},
		{"directory entry dotdot", Part{Directory: &DirectoryPart{Path: "/srv", LacksEntries: []string{".."}}}, "not the name"},
		{"directory entry too long", Part{Directory: &DirectoryPart{Path: "/srv", HasEntries: []string{long}}}, "not the name"},
		{"directory too many entries", Part{Directory: &DirectoryPart{Path: "/srv", HasEntries: many}}, "the limit is"},
		{"directory bad mode", Part{Directory: &DirectoryPart{Path: "/srv", Mode: u32(0o17777)}}, "permission bits"},

		{"process by name", Part{Process: &ProcessPart{Name: s("nginx"), User: s("www-data")}}, ""},
		{"process by pattern", Part{Process: &ProcessPart{Matches: s(`nginx: master`)}}, ""},
		{"process nothing", Part{Process: &ProcessPart{User: s("root")}}, "name or a pattern"},
		{"process name with a path", Part{Process: &ProcessPart{Name: s("/usr/sbin/nginx")}}, "without its directory"},
		{"process bad pattern", Part{Process: &ProcessPart{Matches: s("(")}}, "regular expression"},
		{"process bad user", Part{Process: &ProcessPart{Name: s("x"), User: s("a b")}}, "invalid user"},

		{"port", Part{Port: &PortPart{Port: 8080}}, ""},
		{"port address and process", Part{Port: &PortPart{Port: 8080, Address: s("::1"), Process: s("nginx")}}, ""},
		{"port zero", Part{Port: &PortPart{Port: 0}}, "outside 1..65535"},
		{"port too big", Part{Port: &PortPart{Port: 70000}}, "outside 1..65535"},
		{"port protocol", Part{Port: &PortPart{Port: 80, Protocol: "sctp"}}, "neither tcp nor udp"},
		{"port bad address", Part{Port: &PortPart{Port: 80, Address: s("localhost")}}, "not an IP address"},
		{"closed port with a process", Part{Port: &PortPart{Port: 80, Listening: b(false), Process: s("nginx")}}, "closed"},

		{"sysctl equals", Part{KernelParam: &KernelParamPart{Name: "net.ipv4.ip_forward", Equals: s("1")}}, ""},
		{"sysctl matches", Part{KernelParam: &KernelParamPart{Name: "kernel.randomize_va_space", Matches: s("^[12]$")}}, ""},
		{"sysctl no expectation", Part{KernelParam: &KernelParamPart{Name: "net.ipv4.ip_forward"}}, "equals or matches"},
		{"sysctl slash", Part{KernelParam: &KernelParamPart{Name: "net/ipv4/ip_forward", Equals: s("1")}}, "sysctl name"},
		{"sysctl upper", Part{KernelParam: &KernelParamPart{Name: "Net.ipv4", Equals: s("1")}}, "sysctl name"},
		{"sysctl dotdot", Part{KernelParam: &KernelParamPart{Name: "..", Equals: s("1")}}, "sysctl name"},

		{"request tcp", Part{Request: &RequestPart{Protocol: "tcp", Host: "127.0.0.1", Port: 22}}, ""},
		{"request tcp with a path", Part{Request: &RequestPart{Protocol: "tcp", Host: "127.0.0.1", Port: 22, Path: "/"}}, "tcp request"},
		{"request http", Part{Request: &RequestPart{Protocol: "https", Host: "db.lab", Port: 443, Method: "GET", Path: "/health", Status: i32(204), BodyContains: s("ok"), BodyMatches: s(`"up"`)}}, ""},
		{"request ipv6 host", Part{Request: &RequestPart{Protocol: "http", Host: "[::1]", Port: 80, Method: "HEAD", Path: "/", Status: i32(200)}}, ""},
		{"request ftp", Part{Request: &RequestPart{Protocol: "ftp", Host: "x", Port: 21}}, "not tcp, http or https"},
		{"request POST", Part{Request: &RequestPart{Protocol: "http", Host: "x", Port: 80, Method: "POST", Path: "/", Status: i32(200)}}, "neither GET nor HEAD"},
		{"request path without slash", Part{Request: &RequestPart{Protocol: "http", Host: "x", Port: 80, Method: "GET", Path: "health", Status: i32(200)}}, "must start with /"},
		{"request path with a space", Part{Request: &RequestPart{Protocol: "http", Host: "x", Port: 80, Method: "GET", Path: "/a b", Status: i32(200)}}, "must start with /"},
		{"request status", Part{Request: &RequestPart{Protocol: "http", Host: "x", Port: 80, Method: "GET", Path: "/", Status: i32(99)}}, "status"},
		{"request head with a body rule", Part{Request: &RequestPart{Protocol: "http", Host: "x", Port: 80, Method: "HEAD", Path: "/", Status: i32(200), BodyContains: s("x")}}, "HEAD"},
		{"request host with a space", Part{Request: &RequestPart{Protocol: "tcp", Host: "a b", Port: 80}}, "invalid host"},
		{"request port", Part{Request: &RequestPart{Protocol: "tcp", Host: "x", Port: 0}}, "outside 1..65535"},

		{"permission", Part{Permission: &PermissionPart{User: "www-data", Path: "/etc/shadow", Read: b(false)}}, ""},
		{"permission nothing", Part{Permission: &PermissionPart{User: "www-data", Path: "/etc/shadow"}}, "read, write or execute"},
		{"permission relative", Part{Permission: &PermissionPart{User: "x", Path: "etc", Read: b(true)}}, "not absolute"},
		{"permission bad user", Part{Permission: &PermissionPart{User: "a;b", Path: "/etc", Read: b(true)}}, "invalid user"},

		{"two observations", Part{Process: &ProcessPart{Name: s("x")}, Port: &PortPart{Port: 80}}, "not both or several"},
		{"none", Part{}, "empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			checks := checkWith(c.part)
			NormalizeChecks(checks)
			err := ValidateChecks(checks, devices)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("must be valid: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestChecksRoundTripWithBuiltins(t *testing.T) {
	checks := checkWith(Part{Request: &RequestPart{Protocol: "http", Port: 8080, BodyContains: s("ok")}})
	NormalizeChecks(checks)
	raw, err := MarshalChecks(checks)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"request"`) || strings.Contains(string(raw), `"file"`) {
		t.Fatalf("%s", raw)
	}
}
