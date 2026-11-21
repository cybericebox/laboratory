// Package labchecks is the contract of device bootstrap and lab checks: the secret documents the management agent writes
// (Secret <device>-bootstrap, key bootstrap.json; Secret <lab>-checks, key checks.json), the limits on them and the
// validation the agent applies before anything is written. The launcher and the checker read the same types.
package labchecks

import (
	"encoding/json"
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"
)

// Secret names and keys. The device Secret is named after the Device object (as the -env Secret is), the lab Secret after the Lab.
const (
	BootstrapSecretSuffix = "-bootstrap"
	ChecksSecretSuffix    = "-checks"
	// BootstrapKey and ChecksKey hold one JSON document each (Bootstrap and []Check wrapped in ChecksDoc).
	BootstrapKey = "bootstrap.json"
	ChecksKey    = "checks.json"
)

// Defaults filled in by Normalize, so a consumer never guesses them.
const (
	DefaultInterpreter = "/bin/sh"
	// DefaultCustomUser: a command of a check runs as nobody unless the author names a user (root included: an explicit choice).
	DefaultCustomUser         = "nobody"
	DefaultBootstrapTimeLimit = 300
	DefaultCheckTimeLimit     = 30
)

// Limits. Each is a platform ceiling: the agent refuses a request over it, and GetFeatures reports them.
const (
	// BootstrapMaxAttempts is the number of attempts before a device stays in "failed" (three, with growing pauses).
	BootstrapMaxAttempts = 3
	// MaxBootstrapTimeLimit and MaxCheckTimeLimit are in seconds.
	MaxBootstrapTimeLimit = 3600
	MaxCheckTimeLimit     = 600
	// MaxScheduleSeconds is the longest interval of a scheduled check (one day).
	MaxScheduleSeconds = 86400

	MaxScriptBytes       = 64 << 10
	MaxBootstrapFiles    = 32
	MaxFileBytes         = 256 << 10
	MaxBootstrapBytes    = 512 << 10 // script and all file contents of one device together; a Secret holds 1 MiB and JSON adds a third to bytes
	MaxChecksPerLab      = 64
	MaxPartsPerCheck     = 8
	MaxChecksBytes       = 512 << 10 // the whole checks document of a lab
	MaxPathBytes         = 4096
	MaxPatternBytes      = 1024
	MaxExpectBytes       = 4096
	MaxCommandItems      = 64
	MaxCommandItemBytes  = 4096
	MaxIDBytes           = 64
	MaxNameBytes         = 128
	MaxResultDetailBytes = 2048
	MaxResultValueBytes  = 256

	// The built-in observations.
	MaxEntryNames     = 64        // names in has_entries / lacks_entries
	MaxEntryNameBytes = 255       // one name
	MaxProcessName    = 128       // a process name or a user to match
	MaxHostBytes      = 253       // the host of a request
	MaxRequestPath    = 2048      // the path of an HTTP request
	MaxFileRead       = 256 << 10 // the part of a file that is read; a larger file is an error where the content matters
)

// The kinds of observation a laboratory's checker runs. File and custom are the first two; the rest read the state of the device
// themselves and execute nothing of it.
var Observations = []string{"file", "directory", "process", "port", "kernel_param", "request", "permission", "custom"}

// File is a file to place on a device.
type File struct {
	Path    string `json:"path"`
	Content []byte `json:"content,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Group   string `json:"group,omitempty"`
	// Mode is the permission bits (for example 0o644); 0 means the default 0o644.
	Mode uint32 `json:"mode,omitempty"`
}

// Bootstrap is the list of one device with the values already filled in: an optional script and files, and a time limit.
type Bootstrap struct {
	Script           string `json:"script,omitempty"`
	Interpreter      string `json:"interpreter,omitempty"`
	Files            []File `json:"files,omitempty"`
	TimeLimitSeconds int32  `json:"timeLimitSeconds"`
}

// Empty reports a list that does nothing: no script and no files (it removes the Secret).
func (b *Bootstrap) Empty() bool { return b == nil || (b.Script == "" && len(b.Files) == 0) }

// Check is one check of a lab.
type Check struct {
	ID               string `json:"id"`
	Device           string `json:"device"`
	Mode             string `json:"mode"` // once | continuous
	ScheduleSeconds  int32  `json:"scheduleSeconds,omitempty"`
	ManualRun        bool   `json:"manualRun,omitempty"`
	TimeLimitSeconds int32  `json:"timeLimitSeconds"`
	// Parts must all hold in the same run.
	Parts []Part `json:"parts"`
}

// Part is one observation: exactly one member is set.
type Part struct {
	File        *FilePart        `json:"file,omitempty"`
	Custom      *CustomPart      `json:"custom,omitempty"`
	Directory   *DirectoryPart   `json:"directory,omitempty"`
	Process     *ProcessPart     `json:"process,omitempty"`
	Port        *PortPart        `json:"port,omitempty"`
	KernelParam *KernelParamPart `json:"kernelParam,omitempty"`
	Request     *RequestPart     `json:"request,omitempty"`
	Permission  *PermissionPart  `json:"permission,omitempty"`
}

// FilePart reads a file of the device and compares. Every expectation is optional, at least one is required.
type FilePart struct {
	Path string `json:"path"`
	// Exists true: the file exists; false: it is absent (and no other expectation may be set).
	Exists          *bool   `json:"exists,omitempty"`
	ContentContains *string `json:"contentContains,omitempty"`
	ContentMatches  *string `json:"contentMatches,omitempty"`
	Mode            *uint32 `json:"mode,omitempty"`
	Owner           *string `json:"owner,omitempty"`
	Group           *string `json:"group,omitempty"`
	// Type is what the path itself is (it is not followed): file, directory or symlink. Checksum is the sha256 of the content in
	// lower-case hex. Both need the file to exist; the checksum needs a regular file of at most MaxFileRead bytes.
	Type     *string `json:"type,omitempty"`
	Checksum *string `json:"checksum,omitempty"`
}

// DirectoryPart: exists (default true), mode, owner, group, names that must be directly in the directory and names that must not.
type DirectoryPart struct {
	Path         string   `json:"path"`
	Exists       *bool    `json:"exists,omitempty"`
	Mode         *uint32  `json:"mode,omitempty"`
	Owner        *string  `json:"owner,omitempty"`
	Group        *string  `json:"group,omitempty"`
	HasEntries   []string `json:"hasEntries,omitempty"`
	LacksEntries []string `json:"lacksEntries,omitempty"`
}

// ProcessPart: a process by Name (the command without its directory) or by a regular expression on the command line; at least one.
// Running defaults to true (false: no such process may run).
type ProcessPart struct {
	Name    *string `json:"name,omitempty"`
	Matches *string `json:"matches,omitempty"`
	User    *string `json:"user,omitempty"`
	Running *bool   `json:"running,omitempty"`
}

// PortPart: whether something listens on a port. Protocol defaults to tcp, Listening to true. A listener on the wildcard address counts
// for Address.
type PortPart struct {
	Port      int32   `json:"port"`
	Protocol  string  `json:"protocol,omitempty"`
	Listening *bool   `json:"listening,omitempty"`
	Address   *string `json:"address,omitempty"`
	Process   *string `json:"process,omitempty"`
}

// KernelParamPart: the value of a sysctl; at least one of Equals and Matches.
type KernelParamPart struct {
	Name    string  `json:"name"`
	Equals  *string `json:"equals,omitempty"`
	Matches *string `json:"matches,omitempty"`
}

// RequestPart: a connection from inside the device. Protocol is tcp, http or https; Host defaults to 127.0.0.1. For http and https:
// Method (GET by default, or HEAD), Path (/ by default), Status (200 by default), and rules on the body (GET only).
type RequestPart struct {
	Protocol     string  `json:"protocol"`
	Host         string  `json:"host,omitempty"`
	Port         int32   `json:"port"`
	Method       string  `json:"method,omitempty"`
	Path         string  `json:"path,omitempty"`
	Status       *int32  `json:"status,omitempty"`
	BodyContains *string `json:"bodyContains,omitempty"`
	BodyMatches  *string `json:"bodyMatches,omitempty"`
}

// PermissionPart: whether a user may read, write or execute a path, tested without changing anything. Each given boolean must equal the
// real answer; at least one is required.
type PermissionPart struct {
	User    string `json:"user"`
	Path    string `json:"path"`
	Read    *bool  `json:"read,omitempty"`
	Write   *bool  `json:"write,omitempty"`
	Execute *bool  `json:"execute,omitempty"`
}

// CustomPart runs a command (or a script) in the device. Exit code 0 means passed.
type CustomPart struct {
	Command     []string `json:"command,omitempty"`
	Script      string   `json:"script,omitempty"`
	Interpreter string   `json:"interpreter,omitempty"`
	User        string   `json:"user,omitempty"`
	// OutputContains and OutputMatches are optional additional expectations on the output.
	OutputContains *string `json:"outputContains,omitempty"`
	OutputMatches  *string `json:"outputMatches,omitempty"`
}

// ChecksDoc is the document in the lab's checks Secret.
type ChecksDoc struct {
	Checks []Check `json:"checks"`
}

// MarshalBootstrap and MarshalChecks produce the Secret documents (after Normalize and Validate).
func MarshalBootstrap(b *Bootstrap) ([]byte, error) { return json.Marshal(b) }

// MarshalChecks encodes the checks document and refuses one over MaxChecksBytes.
func MarshalChecks(checks []Check) ([]byte, error) {
	raw, err := json.Marshal(ChecksDoc{Checks: checks})
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxChecksBytes {
		return nil, fmt.Errorf("the checks take %d bytes, the limit is %d", len(raw), MaxChecksBytes)
	}
	return raw, nil
}

var ownerName = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.-]{0,63}|[0-9]{1,10})$`)

// NormalizeBootstrap fills the defaults of a bootstrap list.
func NormalizeBootstrap(b *Bootstrap) {
	if b.Script != "" && b.Interpreter == "" {
		b.Interpreter = DefaultInterpreter
	}
	if b.TimeLimitSeconds == 0 {
		b.TimeLimitSeconds = DefaultBootstrapTimeLimit
	}
	for i := range b.Files {
		if b.Files[i].Mode == 0 {
			b.Files[i].Mode = 0o644
		}
	}
}

// ValidateBootstrap checks the bootstrap list of one device. It must be called after NormalizeBootstrap.
func ValidateBootstrap(b *Bootstrap) error {
	if len(b.Script) > MaxScriptBytes {
		return fmt.Errorf("the script takes %d bytes, the limit is %d", len(b.Script), MaxScriptBytes)
	}
	if strings.ContainsRune(b.Script, 0) {
		return fmt.Errorf("the script contains a NUL byte")
	}
	if b.Script != "" {
		if err := validInterpreter(b.Interpreter); err != nil {
			return err
		}
	} else if b.Interpreter != "" {
		return fmt.Errorf("an interpreter is given without a script")
	}
	if len(b.Files) > MaxBootstrapFiles {
		return fmt.Errorf("%d files, the limit is %d", len(b.Files), MaxBootstrapFiles)
	}
	total := len(b.Script)
	seen := map[string]bool{}
	for i := range b.Files {
		f := &b.Files[i]
		if err := validAbsPath(f.Path); err != nil {
			return fmt.Errorf("file %d: %w", i, err)
		}
		if seen[f.Path] {
			return fmt.Errorf("file %d: the path %q is listed twice", i, f.Path)
		}
		seen[f.Path] = true
		if len(f.Content) > MaxFileBytes {
			return fmt.Errorf("file %q takes %d bytes, the limit is %d", f.Path, len(f.Content), MaxFileBytes)
		}
		total += len(f.Content)
		if f.Mode > 0o7777 {
			return fmt.Errorf("file %q: mode %o is not permission bits", f.Path, f.Mode)
		}
		if f.Owner != "" && !ownerName.MatchString(f.Owner) {
			return fmt.Errorf("file %q: invalid owner %q", f.Path, f.Owner)
		}
		if f.Group != "" && !ownerName.MatchString(f.Group) {
			return fmt.Errorf("file %q: invalid group %q", f.Path, f.Group)
		}
	}
	if total > MaxBootstrapBytes {
		return fmt.Errorf("the script and files take %d bytes, the limit is %d", total, MaxBootstrapBytes)
	}
	if b.TimeLimitSeconds < 1 || b.TimeLimitSeconds > MaxBootstrapTimeLimit {
		return fmt.Errorf("time limit %ds is outside 1..%d", b.TimeLimitSeconds, MaxBootstrapTimeLimit)
	}
	return nil
}

// NormalizeChecks fills the defaults of every check.
func NormalizeChecks(checks []Check) {
	for i := range checks {
		c := &checks[i]
		if c.TimeLimitSeconds == 0 {
			c.TimeLimitSeconds = DefaultCheckTimeLimit
		}
		for j := range c.Parts {
			normalizeBuiltin(&c.Parts[j])
			if cu := c.Parts[j].Custom; cu != nil {
				if cu.User == "" {
					cu.User = DefaultCustomUser
				}
				if cu.Script != "" && cu.Interpreter == "" {
					cu.Interpreter = DefaultInterpreter
				}
			}
		}
	}
}

// ValidateChecks checks the checks of a lab against the names of its devices (the devices that can be observed). It must be
// called after NormalizeChecks.
func ValidateChecks(checks []Check, devices map[string]bool) error {
	if len(checks) > MaxChecksPerLab {
		return fmt.Errorf("%d checks, the limit is %d", len(checks), MaxChecksPerLab)
	}
	seen := map[string]bool{}
	for i := range checks {
		c := &checks[i]
		if c.ID == "" || len(c.ID) > MaxIDBytes {
			return fmt.Errorf("check %d: the id must be 1 to %d characters", i, MaxIDBytes)
		}
		if seen[c.ID] {
			return fmt.Errorf("check %q: the id is used twice", c.ID)
		}
		seen[c.ID] = true
		if !devices[c.Device] {
			return fmt.Errorf("check %q: unknown device %q", c.ID, c.Device)
		}
		switch c.Mode {
		case "once", "continuous":
		default:
			return fmt.Errorf("check %q: mode %q is neither once nor continuous", c.ID, c.Mode)
		}
		if c.ScheduleSeconds < 0 || c.ScheduleSeconds > MaxScheduleSeconds {
			return fmt.Errorf("check %q: schedule %ds is outside 0..%d", c.ID, c.ScheduleSeconds, MaxScheduleSeconds)
		}
		if c.TimeLimitSeconds < 1 || c.TimeLimitSeconds > MaxCheckTimeLimit {
			return fmt.Errorf("check %q: time limit %ds is outside 1..%d", c.ID, c.TimeLimitSeconds, MaxCheckTimeLimit)
		}
		if c.ScheduleSeconds > 0 && c.ScheduleSeconds < c.TimeLimitSeconds {
			return fmt.Errorf("check %q: the schedule (%ds) is shorter than the time limit (%ds)", c.ID, c.ScheduleSeconds, c.TimeLimitSeconds)
		}
		if len(c.Parts) == 0 || len(c.Parts) > MaxPartsPerCheck {
			return fmt.Errorf("check %q: needs 1 to %d parts, has %d", c.ID, MaxPartsPerCheck, len(c.Parts))
		}
		for j := range c.Parts {
			if err := validPart(&c.Parts[j]); err != nil {
				return fmt.Errorf("check %q part %d: %w", c.ID, j, err)
			}
		}
	}
	return nil
}

func validPart(p *Part) error {
	n := 0
	for _, set := range []bool{p.File != nil, p.Custom != nil, p.Directory != nil, p.Process != nil, p.Port != nil, p.KernelParam != nil, p.Request != nil, p.Permission != nil} {
		if set {
			n++
		}
	}
	switch {
	case n > 1:
		return fmt.Errorf("a part has one observation, not both or several")
	case p.File != nil:
		return validFilePart(p.File)
	case p.Custom != nil:
		return validCustomPart(p.Custom)
	case p.Directory != nil:
		return validDirectoryPart(p.Directory)
	case p.Process != nil:
		return validProcessPart(p.Process)
	case p.Port != nil:
		return validPortPart(p.Port)
	case p.KernelParam != nil:
		return validKernelParamPart(p.KernelParam)
	case p.Request != nil:
		return validRequestPart(p.Request)
	case p.Permission != nil:
		return validPermissionPart(p.Permission)
	}
	return fmt.Errorf("the observation is empty")
}

func validFilePart(f *FilePart) error {
	if err := validAbsPath(f.Path); err != nil {
		return err
	}
	if f.Exists == nil && f.ContentContains == nil && f.ContentMatches == nil && f.Mode == nil && f.Owner == nil && f.Group == nil && f.Type == nil && f.Checksum == nil {
		return fmt.Errorf("a file observation needs at least one expectation")
	}
	if f.Exists != nil && !*f.Exists && (f.ContentContains != nil || f.ContentMatches != nil || f.Mode != nil || f.Owner != nil || f.Group != nil || f.Type != nil || f.Checksum != nil) {
		return fmt.Errorf("a file expected to be absent cannot have other expectations")
	}
	if err := validExpect("content_contains", f.ContentContains); err != nil {
		return err
	}
	if err := validPattern("content_matches", f.ContentMatches); err != nil {
		return err
	}
	if f.Type != nil && *f.Type != "file" && *f.Type != "directory" && *f.Type != "symlink" {
		return fmt.Errorf("type %q is not file, directory or symlink", *f.Type)
	}
	if f.Checksum != nil && !sha256Hex.MatchString(*f.Checksum) {
		return fmt.Errorf("checksum must be 64 lower-case hex characters (sha256)")
	}
	return validOwnership(f.Mode, f.Owner, f.Group)
}

var (
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sysctlName = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_-]+)*$`)
	hostName   = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]+$`)
)

func validOwnership(mode *uint32, owner, group *string) error {
	if mode != nil && *mode > 0o7777 {
		return fmt.Errorf("mode %o is not permission bits", *mode)
	}
	if owner != nil && !ownerName.MatchString(*owner) {
		return fmt.Errorf("invalid owner %q", *owner)
	}
	if group != nil && !ownerName.MatchString(*group) {
		return fmt.Errorf("invalid group %q", *group)
	}
	return nil
}

func validEntryNames(what string, names []string) error {
	if len(names) > MaxEntryNames {
		return fmt.Errorf("%s has %d names, the limit is %d", what, len(names), MaxEntryNames)
	}
	for _, n := range names {
		if n == "" || n == "." || n == ".." || len(n) > MaxEntryNameBytes || strings.ContainsAny(n, "/\x00") {
			return fmt.Errorf("%s: %q is not the name of a directory entry", what, n)
		}
	}
	return nil
}

func validDirectoryPart(d *DirectoryPart) error {
	if err := validAbsPath(d.Path); err != nil {
		return err
	}
	if d.Exists != nil && !*d.Exists && (d.Mode != nil || d.Owner != nil || d.Group != nil || len(d.HasEntries) > 0 || len(d.LacksEntries) > 0) {
		return fmt.Errorf("a directory expected to be absent cannot have other expectations")
	}
	if err := validEntryNames("has_entries", d.HasEntries); err != nil {
		return err
	}
	if err := validEntryNames("lacks_entries", d.LacksEntries); err != nil {
		return err
	}
	return validOwnership(d.Mode, d.Owner, d.Group)
}

func validProcessPart(p *ProcessPart) error {
	if p.Name == nil && p.Matches == nil {
		return fmt.Errorf("a process observation needs a name or a pattern")
	}
	if p.Name != nil && (*p.Name == "" || len(*p.Name) > MaxProcessName || strings.ContainsAny(*p.Name, "/\x00 ")) {
		return fmt.Errorf("the process name must be the command without its directory, up to %d bytes, without spaces", MaxProcessName)
	}
	if err := validPattern("matches", p.Matches); err != nil {
		return err
	}
	if p.User != nil && !ownerName.MatchString(*p.User) {
		return fmt.Errorf("invalid user %q", *p.User)
	}
	return nil
}

func validPortPart(p *PortPart) error {
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("port %d is outside 1..65535", p.Port)
	}
	if p.Protocol != "tcp" && p.Protocol != "udp" {
		return fmt.Errorf("protocol %q is neither tcp nor udp", p.Protocol)
	}
	if p.Listening != nil && !*p.Listening && (p.Address != nil || p.Process != nil) {
		return fmt.Errorf("a port expected to be closed cannot have an address or a process")
	}
	if p.Address != nil && net.ParseIP(*p.Address) == nil {
		return fmt.Errorf("address %q is not an IP address", *p.Address)
	}
	if p.Process != nil && (*p.Process == "" || len(*p.Process) > MaxProcessName || strings.ContainsAny(*p.Process, "/\x00 ")) {
		return fmt.Errorf("invalid process name")
	}
	return nil
}

func validKernelParamPart(k *KernelParamPart) error {
	if len(k.Name) > 256 || !sysctlName.MatchString(k.Name) {
		return fmt.Errorf("%q is not a sysctl name with dots", k.Name)
	}
	if k.Equals == nil && k.Matches == nil {
		return fmt.Errorf("a kernel_param observation needs equals or matches")
	}
	if err := validExpect("equals", k.Equals); err != nil {
		return err
	}
	return validPattern("matches", k.Matches)
}

func validRequestPart(r *RequestPart) error {
	if r.Host == "" || len(r.Host) > MaxHostBytes || !hostName.MatchString(r.Host) {
		return fmt.Errorf("invalid host %q", r.Host)
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("port %d is outside 1..65535", r.Port)
	}
	switch r.Protocol {
	case "tcp":
		if r.Method != "" || r.Path != "" || r.Status != nil || r.BodyContains != nil || r.BodyMatches != nil {
			return fmt.Errorf("a tcp request has no method, path, status or body rules")
		}
		return nil
	case "http", "https":
	default:
		return fmt.Errorf("protocol %q is not tcp, http or https", r.Protocol)
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		return fmt.Errorf("method %q is neither GET nor HEAD", r.Method)
	}
	if r.Path == "" || r.Path[0] != '/' || len(r.Path) > MaxRequestPath || strings.ContainsAny(r.Path, " \t\r\n\x00#") {
		return fmt.Errorf("the path must start with / and hold no space, control character or #, up to %d bytes", MaxRequestPath)
	}
	if r.Status == nil || *r.Status < 100 || *r.Status > 599 {
		return fmt.Errorf("status must be 100..599")
	}
	if r.Method == "HEAD" && (r.BodyContains != nil || r.BodyMatches != nil) {
		return fmt.Errorf("a HEAD request has no body to check")
	}
	if err := validExpect("body_contains", r.BodyContains); err != nil {
		return err
	}
	return validPattern("body_matches", r.BodyMatches)
}

func validPermissionPart(p *PermissionPart) error {
	if !ownerName.MatchString(p.User) {
		return fmt.Errorf("invalid user %q", p.User)
	}
	if err := validAbsPath(p.Path); err != nil {
		return err
	}
	if p.Read == nil && p.Write == nil && p.Execute == nil {
		return fmt.Errorf("a permission observation needs read, write or execute")
	}
	return nil
}

// normalizeBuiltin fills the defaults of the built-in observations, so the checker never guesses them.
func normalizeBuiltin(p *Part) {
	t := true
	if pr := p.Process; pr != nil && pr.Running == nil {
		pr.Running = &t
	}
	if po := p.Port; po != nil {
		if po.Protocol == "" {
			po.Protocol = "tcp"
		}
		if po.Listening == nil {
			po.Listening = &t
		}
	}
	if r := p.Request; r != nil {
		if r.Host == "" {
			r.Host = "127.0.0.1"
		}
		if r.Protocol == "http" || r.Protocol == "https" {
			if r.Method == "" {
				r.Method = "GET"
			}
			if r.Path == "" {
				r.Path = "/"
			}
			if r.Status == nil {
				ok := int32(200)
				r.Status = &ok
			}
		}
	}
}

func validCustomPart(c *CustomPart) error {
	if (len(c.Command) == 0) == (c.Script == "") {
		return fmt.Errorf("a custom observation needs exactly one of a command and a script")
	}
	if len(c.Command) > 0 {
		if len(c.Command) > MaxCommandItems {
			return fmt.Errorf("the command has %d items, the limit is %d", len(c.Command), MaxCommandItems)
		}
		if c.Command[0] == "" {
			return fmt.Errorf("the command is empty")
		}
		for _, it := range c.Command {
			if len(it) > MaxCommandItemBytes || strings.ContainsRune(it, 0) {
				return fmt.Errorf("a command item is over %d bytes or contains NUL", MaxCommandItemBytes)
			}
		}
		if c.Interpreter != "" {
			return fmt.Errorf("an interpreter is given without a script")
		}
	} else {
		if len(c.Script) > MaxScriptBytes || strings.ContainsRune(c.Script, 0) {
			return fmt.Errorf("the script is over %d bytes or contains NUL", MaxScriptBytes)
		}
		if err := validInterpreter(c.Interpreter); err != nil {
			return err
		}
	}
	if !ownerName.MatchString(c.User) {
		return fmt.Errorf("invalid user %q", c.User)
	}
	if err := validExpect("output_contains", c.OutputContains); err != nil {
		return err
	}
	return validPattern("output_matches", c.OutputMatches)
}

func validExpect(what string, v *string) error {
	if v == nil {
		return nil
	}
	if len(*v) > MaxExpectBytes || strings.ContainsRune(*v, 0) {
		return fmt.Errorf("%s is over %d bytes or contains NUL", what, MaxExpectBytes)
	}
	return nil
}

func validPattern(what string, v *string) error {
	if v == nil {
		return nil
	}
	if len(*v) > MaxPatternBytes {
		return fmt.Errorf("%s is over %d bytes", what, MaxPatternBytes)
	}
	if _, err := regexp.Compile(*v); err != nil {
		return fmt.Errorf("%s is not a valid regular expression: %v", what, err)
	}
	return nil
}

func validInterpreter(s string) error {
	if len(s) == 0 || len(s) > 256 || !path.IsAbs(s) || strings.ContainsAny(s, " \t\n\x00") {
		return fmt.Errorf("the interpreter %q must be one absolute path", s)
	}
	return nil
}

// validAbsPath wants a clean absolute path of a file (not the root).
func validAbsPath(p string) error {
	if p == "" || len(p) > MaxPathBytes || strings.ContainsRune(p, 0) {
		return fmt.Errorf("the path must be 1 to %d characters without NUL", MaxPathBytes)
	}
	if !path.IsAbs(p) {
		return fmt.Errorf("the path %q is not absolute", p)
	}
	if p == "/" || path.Clean(p) != p {
		return fmt.Errorf("the path %q must be a clean file path (no \"..\", \".\", \"//\" or trailing \"/\")", p)
	}
	return nil
}

// TruncateDetail cuts a result detail to MaxResultDetailBytes on a UTF-8 boundary.
func TruncateDetail(s string) string { return truncate(s, MaxResultDetailBytes) }

// TruncateValue cuts a result value to MaxResultValueBytes on a UTF-8 boundary.
func TruncateValue(s string) string { return truncate(s, MaxResultValueBytes) }

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}
