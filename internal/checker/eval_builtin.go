package checker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/probe"
)

// The built-in observations are judged here, in the checker. The in-device program only reports facts (a directory listing, the process
// table, the sockets of a port, the value of a sysctl, the answer of a request, the verdict of access(2)); the expected values of the
// author never leave the checker. Reasons name the kind of expectation that did not hold and never the expected value.

// EvalDirectory compares a directory with the expectations of the part. key is the key of the question the facts answer: the names the
// author expects are hashed with it here, never sent to the device.
func EvalDirectory(p *labchecks.DirectoryPart, f *probe.DirectoryFacts, key []byte) (Outcome, []string) {
	v := evalDirectory(p, f, key)
	return v.outcome, v.reasons
}

func evalDirectory(p *labchecks.DirectoryPart, f *probe.DirectoryFacts, key []byte) partVerdict {
	if p.Exists != nil && !*p.Exists {
		if f.Exists {
			return partVerdict{NotPassed, []string{"exists: the path is present"}}
		}
		return partVerdict{outcome: Passed}
	}
	switch {
	case !f.Exists:
		return partVerdict{NotPassed, []string{"exists: the directory is missing"}}
	case !f.IsDir:
		return partVerdict{NotPassed, []string{"type: not a directory"}}
	}
	var fails []string
	if p.Mode != nil && f.Mode != *p.Mode {
		fails = append(fails, fmt.Sprintf("mode: is %04o", f.Mode))
	}
	if p.Owner != nil && !ownerMatches(*p.Owner, f.UID, f.User, false) {
		fails = append(fails, "owner: differs")
	}
	if p.Group != nil && !ownerMatches(*p.Group, f.GID, f.Group, false) {
		fails = append(fails, "group: differs")
	}
	have := make(map[string]bool, len(f.Hashes))
	for _, h := range f.Hashes {
		have[string(h)] = true
	}
	present := func(name string) bool { return have[string(probe.EntryHash(key, name))] }
	// A listing that was cut says nothing about the names it did not reach: what was seen is seen, what was not is not known, in both
	// the positive and the negative form.
	undecided := false
	for _, n := range p.HasEntries {
		if !present(n) {
			if f.Truncated {
				undecided = true
			} else {
				fails = append(fails, "has_entries: a name is missing")
			}
			break
		}
	}
	for _, n := range p.LacksEntries {
		if present(n) {
			fails = append(fails, "lacks_entries: a name is present")
			undecided = false
			break
		}
		undecided = undecided || f.Truncated
	}
	switch {
	case len(fails) > 0:
		return partVerdict{NotPassed, fails}
	case undecided:
		return partVerdict{Failed, []string{"entries: the directory has more entries than are listed"}}
	}
	return partVerdict{outcome: Passed}
}

// EvalProcess judges the process table.
func EvalProcess(p *labchecks.ProcessPart, f *probe.ProcessFacts) (Outcome, []string) {
	v := evalProcess(p, f)
	return v.outcome, v.reasons
}

// processVerdict: does the process match what is looked for? known is false when it cannot be told: the command line was cut and the
// pattern is about the command line (an anchor would fit the cut).
func processVerdict(p *labchecks.ProcessPart, re *regexp.Regexp, q probe.Process) (match, known bool) {
	if p.Name != nil {
		name := *p.Name
		// The kernel keeps 15 characters of the name.
		cut := len(name) > 15 && q.Comm == name[:15]
		if q.Comm != name && q.Arg0 != name && !cut {
			return false, true
		}
	}
	if p.User != nil && !ownerMatches(*p.User, q.UID, q.User, false) {
		return false, true
	}
	if re != nil {
		if q.Cut {
			return false, false
		}
		if !re.MatchString(q.Cmdline) {
			return false, true
		}
	}
	return true, true
}

func evalProcess(p *labchecks.ProcessPart, f *probe.ProcessFacts) partVerdict {
	var re *regexp.Regexp
	if p.Matches != nil {
		var err error
		if re, err = compile(*p.Matches); err != nil {
			return partVerdict{Failed, []string{"matches: the pattern does not compile"}}
		}
	}
	unknown := f.Truncated || f.Incomplete > 0
	found := false
	for _, q := range f.List {
		match, known := processVerdict(p, re, q)
		switch {
		case match:
			found = true
		case !known:
			unknown = true
		}
	}
	wantRunning := p.Running == nil || *p.Running
	switch {
	case found && wantRunning:
		return partVerdict{outcome: Passed}
	case found:
		return partVerdict{NotPassed, []string{"running: such a process runs"}}
	case unknown:
		// Nothing matched among what could be judged; the rest of the table (cut, unreadable, or a command line too long to judge) may
		// hold the process. Neither "no such process" nor "none runs" is known.
		return partVerdict{Failed, []string{"running: part of the process table could not be judged"}}
	case wantRunning:
		return partVerdict{NotPassed, []string{"running: no such process"}}
	}
	return partVerdict{outcome: Passed}
}

// addrMatches: the listener is on the address, or on the wildcard of its family (0.0.0.0 accepts every ipv4 address, :: every address).
func addrMatches(listener, want string) bool {
	l, w := net.ParseIP(listener), net.ParseIP(want)
	if l == nil || w == nil {
		return false
	}
	if l.Equal(w) {
		return true
	}
	return l.IsUnspecified() && (l.To4() == nil || w.To4() != nil)
}

// EvalPort judges the sockets found on a port.
func EvalPort(p *labchecks.PortPart, f *probe.PortFacts) (Outcome, []string) {
	v := evalPort(p, f)
	return v.outcome, v.reasons
}

func evalPort(p *labchecks.PortPart, f *probe.PortFacts) partVerdict {
	if p.Listening != nil && !*p.Listening {
		// Any listener found is enough to fail; none found is the whole table (it is read in full), so it is the answer.
		if len(f.Listeners) == 0 {
			return partVerdict{outcome: Passed}
		}
		return partVerdict{NotPassed, []string{"listening: something listens"}}
	}
	if len(f.Listeners) == 0 {
		return partVerdict{NotPassed, []string{"listening: nothing listens"}}
	}
	for _, l := range f.Listeners {
		okAddr := p.Address == nil || addrMatches(l.Addr, *p.Address)
		okProc := p.Process == nil || containsString(l.Procs, *p.Process) || containsString(l.Arg0s, *p.Process)
		if okAddr && okProc {
			return partVerdict{outcome: Passed}
		}
	}
	// No listener that was looked at fits. A table cut at the cap, or owners that could not all be looked up, leave that open.
	if f.Truncated {
		return partVerdict{Failed, []string{"listening: more sockets than are listed"}}
	}
	if p.Process != nil && f.OwnersIncomplete {
		return partVerdict{Failed, []string{"process: some processes could not be looked into, the owner of the socket is not known"}}
	}
	var fails []string
	if p.Address != nil {
		fails = append(fails, "address: no listener on that address")
	}
	if p.Process != nil {
		fails = append(fails, "process: no listener belongs to that process")
	}
	return partVerdict{NotPassed, fails}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// EvalKernelParam judges the value of a sysctl.
func EvalKernelParam(p *labchecks.KernelParamPart, f *probe.KernelParamFacts) (Outcome, []string) {
	v := evalKernelParam(p, f)
	return v.outcome, v.reasons
}

func evalKernelParam(p *labchecks.KernelParamPart, f *probe.KernelParamFacts) partVerdict {
	if !f.Exists {
		return partVerdict{NotPassed, []string{"exists: the parameter does not exist"}}
	}
	var fails []string
	if p.Equals != nil && strings.TrimSpace(*p.Equals) != f.Value {
		fails = append(fails, "equals: another value")
	}
	if p.Matches != nil {
		re, err := compile(*p.Matches)
		switch {
		case err != nil:
			return partVerdict{Failed, []string{"matches: the pattern does not compile"}}
		case !re.MatchString(f.Value):
			fails = append(fails, "matches: no match")
		}
	}
	if len(fails) > 0 {
		return partVerdict{NotPassed, fails}
	}
	return partVerdict{outcome: Passed}
}

// EvalRequest judges the answer of a request made from inside the device. A connection that does not open is a fact about the
// service (not passed), not an error of the check.
func EvalRequest(p *labchecks.RequestPart, f *probe.RequestFacts) (Outcome, []string) {
	v := evalRequest(p, f)
	return v.outcome, v.reasons
}

func evalRequest(p *labchecks.RequestPart, f *probe.RequestFacts) partVerdict {
	if !f.Connected {
		// A refusal, an unreachable network, a name that does not resolve, a failed TLS handshake are facts about the service. A timeout
		// and an unnamed error are not (the service may be slow, or the request did not get its turn): undecided.
		switch f.Failure {
		case "refused", "unreachable", "resolve", "tls":
			return partVerdict{NotPassed, []string{"connect: failed (" + f.Failure + ")"}}
		}
		return partVerdict{Failed, []string{"connect: no answer in time or an unknown error"}}
	}
	if p.Protocol == "tcp" {
		return partVerdict{outcome: Passed}
	}
	if p.Status != nil && int32(f.Status) != *p.Status {
		return partVerdict{NotPassed, []string{fmt.Sprintf("status: is %d", f.Status)}}
	}
	if p.BodyContains == nil && p.BodyMatches == nil {
		return partVerdict{outcome: Passed}
	}
	if f.Truncated {
		// A body cut at the cap decides nothing: not "contains", not "matches" (an anchor would fit the cut).
		return partVerdict{Failed, []string{"body: longer than the part that is read"}}
	}
	var fails []string
	if p.BodyContains != nil && !strings.Contains(string(f.Body), *p.BodyContains) {
		fails = append(fails, "body_contains: not found")
	}
	if p.BodyMatches != nil {
		re, err := compile(*p.BodyMatches)
		switch {
		case err != nil:
			return partVerdict{Failed, []string{"body_matches: the pattern does not compile"}}
		case !re.Match(f.Body):
			fails = append(fails, "body_matches: no match")
		}
	}
	if len(fails) > 0 {
		return partVerdict{NotPassed, fails}
	}
	return partVerdict{outcome: Passed}
}

// EvalPermission judges what the kernel said to the user.
func EvalPermission(p *labchecks.PermissionPart, f *probe.PermissionFacts) (Outcome, []string) {
	v := evalPermission(p, f)
	return v.outcome, v.reasons
}

func evalPermission(p *labchecks.PermissionPart, f *probe.PermissionFacts) partVerdict {
	switch {
	case !f.UserFound:
		return partVerdict{NotPassed, []string{"user: not found in the device"}}
	case !f.Exists:
		return partVerdict{NotPassed, []string{"path: missing"}}
	}
	var fails []string
	for _, c := range []struct {
		name string
		want *bool
		got  bool
	}{{"read", p.Read, f.Read}, {"write", p.Write, f.Write}, {"execute", p.Execute, f.Execute}} {
		if c.want != nil && *c.want != c.got {
			fails = append(fails, fmt.Sprintf("%s: is %t", c.name, c.got))
		}
	}
	if len(fails) > 0 {
		return partVerdict{NotPassed, fails}
	}
	return partVerdict{outcome: Passed}
}

// sha256Of is the checksum a file expectation is compared with.
func sha256Of(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
