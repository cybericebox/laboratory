// Package labchecks is the contract of device bootstrap and lab checks: the secret documents the management agent writes
// (Secret <device>-bootstrap, key bootstrap.json; Secret <lab>-checks, key checks.json), the limits on them and the
// validation the agent applies before anything is written. The launcher and the checker read the same types.
package labchecks

import (
	"encoding/json"
	"fmt"
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
	DefaultInterpreter        = "/bin/sh"
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
)

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

// Part is one observation: exactly one of File and Custom is set.
type Part struct {
	File   *FilePart   `json:"file,omitempty"`
	Custom *CustomPart `json:"custom,omitempty"`
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
	switch {
	case p.File != nil && p.Custom != nil:
		return fmt.Errorf("a part is either a file or a custom observation, not both")
	case p.File != nil:
		return validFilePart(p.File)
	case p.Custom != nil:
		return validCustomPart(p.Custom)
	}
	return fmt.Errorf("the observation is empty")
}

func validFilePart(f *FilePart) error {
	if err := validAbsPath(f.Path); err != nil {
		return err
	}
	if f.Exists == nil && f.ContentContains == nil && f.ContentMatches == nil && f.Mode == nil && f.Owner == nil && f.Group == nil {
		return fmt.Errorf("a file observation needs at least one expectation")
	}
	if f.Exists != nil && !*f.Exists && (f.ContentContains != nil || f.ContentMatches != nil || f.Mode != nil || f.Owner != nil || f.Group != nil) {
		return fmt.Errorf("a file expected to be absent cannot have other expectations")
	}
	if err := validExpect("content_contains", f.ContentContains); err != nil {
		return err
	}
	if err := validPattern("content_matches", f.ContentMatches); err != nil {
		return err
	}
	if f.Mode != nil && *f.Mode > 0o7777 {
		return fmt.Errorf("mode %o is not permission bits", *f.Mode)
	}
	if f.Owner != nil && !ownerName.MatchString(*f.Owner) {
		return fmt.Errorf("invalid owner %q", *f.Owner)
	}
	if f.Group != nil && !ownerName.MatchString(*f.Group) {
		return fmt.Errorf("invalid group %q", *f.Group)
	}
	return nil
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
