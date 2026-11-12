package checker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/probe"
)

// The built-in observations are judged here, in the checker. The in-device program only reports facts (a directory listing, the process
// table, the sockets of a port, the value of a sysctl, the answer of a request, the verdict of access(2)); the expected values of the
// author never leave the checker. Reasons name the kind of expectation that did not hold and never the expected value.

// EvalDirectory compares a directory listing with the expectations of the part.
func EvalDirectory(p *labchecks.DirectoryPart, f *probe.DirectoryFacts) (Outcome, []string) {
	v := evalDirectory(p, f)
	return v.outcome, v.reasons
}

func evalDirectory(p *labchecks.DirectoryPart, f *probe.DirectoryFacts) partVerdict {
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
	have := make(map[string]bool, len(f.Present))
	for _, e := range f.Present {
		have[e] = true
	}
	for _, n := range p.HasEntries {
		if !have[n] {
			fails = append(fails, "has_entries: a name is missing")
			break
		}
	}
	for _, n := range p.LacksEntries {
		if have[n] {
			fails = append(fails, "lacks_entries: a name is present")
			break
		}
	}
	if len(fails) > 0 {
		return partVerdict{NotPassed, fails}
	}
	return partVerdict{outcome: Passed}
}

// EvalProcess judges the result of the scan of the process table: the program looked at every process and says whether one matched.
func EvalProcess(p *labchecks.ProcessPart, f *probe.ProcessFacts) (Outcome, []string) {
	v := evalProcess(p, f)
	return v.outcome, v.reasons
}

func evalProcess(p *labchecks.ProcessPart, f *probe.ProcessFacts) partVerdict {
	if p.Matches != nil {
		if _, err := compile(*p.Matches); err != nil {
			return partVerdict{Failed, []string{"matches: the pattern does not compile"}}
		}
	}
	wantRunning := p.Running == nil || *p.Running
	switch {
	case f.Found && wantRunning:
		return partVerdict{outcome: Passed}
	case f.Found:
		return partVerdict{NotPassed, []string{"running: such a process runs"}}
	case f.Incomplete > 0:
		// Nothing matched among what could be seen; a process that could not be seen completely may match. Neither "no such process" nor
		// "none runs" is known.
		return partVerdict{Failed, []string{"running: part of the process table could not be seen"}}
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
