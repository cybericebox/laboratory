package labchecks

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func s(v string) *string { return &v }
func b(v bool) *bool     { return &v }

var devices = map[string]bool{"web": true}

func goodCheck() Check {
	return Check{ID: "c", Device: "web", Mode: "once", Parts: []Part{{File: &FilePart{Path: "/etc/x", Exists: b(true)}}}}
}

func TestBootstrapDefaultsAndRoundTrip(t *testing.T) {
	in := Bootstrap{Script: "echo hi", Files: []File{{Path: "/etc/app.conf", Content: []byte("k=v\n"), Owner: "app"}}}
	NormalizeBootstrap(&in)
	if in.Interpreter != "/bin/sh" || in.TimeLimitSeconds != DefaultBootstrapTimeLimit || in.Files[0].Mode != 0o644 {
		t.Fatalf("defaults: %+v", in)
	}
	if err := ValidateBootstrap(&in); err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalBootstrap(&in)
	if err != nil {
		t.Fatal(err)
	}
	var out Bootstrap
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Script != in.Script || string(out.Files[0].Content) != "k=v\n" || out.Files[0].Mode != 0o644 || out.TimeLimitSeconds != 300 {
		t.Fatalf("round trip: %+v", out)
	}
}

func TestBootstrapEmpty(t *testing.T) {
	var nilList *Bootstrap
	if !nilList.Empty() || !(&Bootstrap{}).Empty() || (&Bootstrap{Script: "x"}).Empty() || (&Bootstrap{Files: []File{{Path: "/a"}}}).Empty() {
		t.Fatal("a list without script and files is empty")
	}
}

func TestValidateBootstrapRefusals(t *testing.T) {
	cases := map[string]struct {
		mod  func(*Bootstrap)
		want string
	}{
		"script too big":           {func(x *Bootstrap) { x.Script = strings.Repeat("a", MaxScriptBytes+1) }, "the script takes"},
		"script with NUL":          {func(x *Bootstrap) { x.Script = "a\x00b" }, "NUL"},
		"interpreter without text": {func(x *Bootstrap) { x.Script = ""; x.Interpreter = "/bin/sh" }, "without a script"},
		"interpreter relative":     {func(x *Bootstrap) { x.Interpreter = "sh" }, "absolute path"},
		"interpreter with args":    {func(x *Bootstrap) { x.Interpreter = "/usr/bin/env bash" }, "absolute path"},
		"relative path":            {func(x *Bootstrap) { x.Files[0].Path = "a/b" }, "not absolute"},
		"dotdot path":              {func(x *Bootstrap) { x.Files[0].Path = "/a/../b" }, "clean file path"},
		"trailing slash":           {func(x *Bootstrap) { x.Files[0].Path = "/a/" }, "clean file path"},
		"root path":                {func(x *Bootstrap) { x.Files[0].Path = "/" }, "clean file path"},
		"long path":                {func(x *Bootstrap) { x.Files[0].Path = "/" + strings.Repeat("a", MaxPathBytes) }, "1 to"},
		"file too big":             {func(x *Bootstrap) { x.Files[0].Content = make([]byte, MaxFileBytes+1) }, "takes"},
		"mode too wide":            {func(x *Bootstrap) { x.Files[0].Mode = 0o10000 }, "permission bits"},
		"owner with space":         {func(x *Bootstrap) { x.Files[0].Owner = "a b" }, "invalid owner"},
		"group with colon":         {func(x *Bootstrap) { x.Files[0].Group = "a:b" }, "invalid group"},
		"time limit zero":          {func(x *Bootstrap) { x.TimeLimitSeconds = -1 }, "time limit"},
		"time limit over ceiling":  {func(x *Bootstrap) { x.TimeLimitSeconds = MaxBootstrapTimeLimit + 1 }, "time limit"},
		"too many files": {func(x *Bootstrap) {
			x.Files = nil
			for i := 0; i <= MaxBootstrapFiles; i++ {
				x.Files = append(x.Files, File{Path: "/f/" + strings.Repeat("x", i+1)})
			}
		}, "the limit is"},
		"duplicate path": {func(x *Bootstrap) { x.Files = append(x.Files, x.Files[0]) }, "listed twice"},
		"everything together too big": {func(x *Bootstrap) {
			x.Script = strings.Repeat("a", MaxScriptBytes)
			x.Files = nil
			for i := 0; i < 3; i++ {
				x.Files = append(x.Files, File{Path: "/f/" + string(rune('a'+i)), Content: make([]byte, MaxFileBytes)})
			}
		}, "the script and files take"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			x := Bootstrap{Script: "echo", Files: []File{{Path: "/etc/a", Content: []byte("x")}}}
			NormalizeBootstrap(&x)
			c.mod(&x)
			err := ValidateBootstrap(&x)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
	// the largest allowed list passes
	x := Bootstrap{Script: strings.Repeat("a", MaxScriptBytes), Files: []File{{Path: "/f/a", Content: make([]byte, MaxFileBytes)}, {Path: "/f/b", Content: make([]byte, MaxFileBytes-MaxScriptBytes)}}, TimeLimitSeconds: MaxBootstrapTimeLimit}
	NormalizeBootstrap(&x)
	if err := ValidateBootstrap(&x); err != nil {
		t.Fatalf("at the limits: %v", err)
	}
}

func TestChecksDefaultsAndRoundTrip(t *testing.T) {
	cs := []Check{{ID: "k", Device: "web", Mode: "continuous", ScheduleSeconds: 60, Parts: []Part{
		{Custom: &CustomPart{Script: "test -f /x"}},
		{Custom: &CustomPart{Command: []string{"/bin/true"}, OutputContains: s("ok")}},
	}}}
	NormalizeChecks(cs)
	if cs[0].TimeLimitSeconds != DefaultCheckTimeLimit || cs[0].Parts[0].Custom.User != "root" || cs[0].Parts[0].Custom.Interpreter != "/bin/sh" || cs[0].Parts[1].Custom.Interpreter != "" {
		t.Fatalf("defaults: %+v", cs[0])
	}
	if err := ValidateChecks(cs, devices); err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalChecks(cs)
	if err != nil {
		t.Fatal(err)
	}
	var doc ChecksDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Checks) != 1 || *doc.Checks[0].Parts[1].Custom.OutputContains != "ok" || doc.Checks[0].ScheduleSeconds != 60 {
		t.Fatalf("round trip: %+v", doc)
	}
}

func TestValidateChecksRefusals(t *testing.T) {
	cases := map[string]struct {
		mod  func(*Check)
		want string
	}{
		"empty id":            {func(c *Check) { c.ID = "" }, "id must be"},
		"long id":             {func(c *Check) { c.ID = strings.Repeat("a", MaxIDBytes+1) }, "id must be"},
		"unknown device":      {func(c *Check) { c.Device = "ghost" }, "unknown device"},
		"bad mode":            {func(c *Check) { c.Mode = "sometimes" }, "neither once nor continuous"},
		"negative schedule":   {func(c *Check) { c.ScheduleSeconds = -1 }, "schedule"},
		"schedule too long":   {func(c *Check) { c.ScheduleSeconds = MaxScheduleSeconds + 1 }, "schedule"},
		"time limit too long": {func(c *Check) { c.TimeLimitSeconds = MaxCheckTimeLimit + 1 }, "time limit"},
		"schedule under time": {func(c *Check) { c.ScheduleSeconds, c.TimeLimitSeconds = 5, 10 }, "shorter than the time limit"},
		"no parts":            {func(c *Check) { c.Parts = nil }, "needs 1 to"},
		"too many parts": {func(c *Check) {
			for len(c.Parts) <= MaxPartsPerCheck {
				c.Parts = append(c.Parts, c.Parts[0])
			}
		}, "needs 1 to"},
		"empty part":               {func(c *Check) { c.Parts = []Part{{}} }, "observation is empty"},
		"both kinds":               {func(c *Check) { c.Parts[0].Custom = &CustomPart{Command: []string{"x"}} }, "not both"},
		"file path relative":       {func(c *Check) { c.Parts[0].File.Path = "etc/x" }, "not absolute"},
		"file without expectation": {func(c *Check) { c.Parts[0].File = &FilePart{Path: "/x"} }, "at least one expectation"},
		"absent with content":      {func(c *Check) { c.Parts[0].File = &FilePart{Path: "/x", Exists: b(false), ContentContains: s("a")} }, "absent"},
		"bad content regex":        {func(c *Check) { c.Parts[0].File.ContentMatches = s("a(") }, "regular expression"},
		"long content regex":       {func(c *Check) { c.Parts[0].File.ContentMatches = s(strings.Repeat("a", MaxPatternBytes+1)) }, "over"},
		"long contains":            {func(c *Check) { c.Parts[0].File.ContentContains = s(strings.Repeat("a", MaxExpectBytes+1)) }, "over"},
		"wide mode":                {func(c *Check) { m := uint32(0o10000); c.Parts[0].File.Mode = &m }, "permission bits"},
		"bad owner":                {func(c *Check) { c.Parts[0].File.Owner = s("a b") }, "invalid owner"},
		"command and script": {func(c *Check) {
			c.Parts = []Part{{Custom: &CustomPart{Command: []string{"a"}, Script: "b", User: "root"}}}
		}, "exactly one"},
		"neither command nor script": {func(c *Check) { c.Parts = []Part{{Custom: &CustomPart{User: "root"}}} }, "exactly one"},
		"empty command word":         {func(c *Check) { c.Parts = []Part{{Custom: &CustomPart{Command: []string{""}, User: "root"}}} }, "empty"},
		"interpreter with command": {func(c *Check) {
			c.Parts = []Part{{Custom: &CustomPart{Command: []string{"a"}, Interpreter: "/bin/sh", User: "root"}}}
		}, "without a script"},
		"script interpreter bad": {func(c *Check) { c.Parts = []Part{{Custom: &CustomPart{Script: "x", Interpreter: "sh", User: "root"}}} }, "interpreter"},
		"bad user":               {func(c *Check) { c.Parts = []Part{{Custom: &CustomPart{Command: []string{"a"}, User: "a b"}}} }, "invalid user"},
		"bad output regex": {func(c *Check) {
			c.Parts = []Part{{Custom: &CustomPart{Command: []string{"a"}, User: "root", OutputMatches: s("[")}}}
		}, "regular expression"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ck := goodCheck()
			ck.Parts[0].File = &FilePart{Path: "/etc/x", ContentContains: s("v")}
			cs := []Check{ck}
			NormalizeChecks(cs)
			c.mod(&cs[0])
			err := ValidateChecks(cs, devices)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestValidateChecksListRules(t *testing.T) {
	a, bb := goodCheck(), goodCheck()
	NormalizeChecks([]Check{a, bb})
	one := []Check{a}
	NormalizeChecks(one)
	if err := ValidateChecks(one, devices); err != nil {
		t.Fatal(err)
	}
	dup := []Check{one[0], one[0]}
	if err := ValidateChecks(dup, devices); err == nil || !strings.Contains(err.Error(), "used twice") {
		t.Fatalf("duplicate ids: %v", err)
	}
	var many []Check
	for i := 0; i <= MaxChecksPerLab; i++ {
		c := one[0]
		c.ID = strings.Repeat("x", i+1)
		many = append(many, c)
	}
	if err := ValidateChecks(many, devices); err == nil || !strings.Contains(err.Error(), "the limit is") {
		t.Fatalf("too many: %v", err)
	}
	if err := ValidateChecks(many[:MaxChecksPerLab], devices); err != nil {
		t.Fatalf("exactly the limit: %v", err)
	}
	big := one[0]
	big.Parts = nil
	for i := 0; i < MaxPartsPerCheck; i++ {
		big.Parts = append(big.Parts, Part{File: &FilePart{Path: "/x", ContentContains: s(strings.Repeat("a", MaxExpectBytes))}})
	}
	var huge []Check
	for i := 0; i < MaxChecksPerLab; i++ {
		c := big
		c.ID = strings.Repeat("y", i+1)
		huge = append(huge, c)
	}
	if _, err := MarshalChecks(huge); err == nil || !strings.Contains(err.Error(), "the limit is") {
		t.Fatalf("the document size is limited: %v", err)
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("é", MaxResultDetailBytes)
	got := TruncateDetail(long)
	if len(got) > MaxResultDetailBytes || !utf8.ValidString(got) || !strings.HasPrefix(long, got) {
		t.Fatalf("detail: %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
	if TruncateDetail("short") != "short" || len(TruncateValue(strings.Repeat("a", 1000))) != MaxResultValueBytes {
		t.Fatal("short text stays, long value is cut")
	}
}
