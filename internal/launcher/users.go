package launcher

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Identity is a resolved user: ids and the home directory and groups a container runtime would give it.
type Identity struct {
	UID, GID int
	Home     string
	// Groups are the supplementary groups: the groups of /etc/group that list the user.
	Groups []int
}

// Accounts reads /etc/passwd and /etc/group of the container (under Root; empty means "/").
type Accounts struct{ Root string }

func (a Accounts) path(name string) string { return filepath.Join(a.Root, "/etc", name) }

type passwdEntry struct {
	name     string
	uid, gid int
	home     string
}

func (a Accounts) passwd() []passwdEntry {
	var out []passwdEntry
	for _, f := range readColonFile(a.path("passwd"), 7) {
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, passwdEntry{name: f[0], uid: uid, gid: gid, home: f[5]})
	}
	return out
}

type groupEntry struct {
	name    string
	gid     int
	members []string
}

func (a Accounts) groups() []groupEntry {
	var out []groupEntry
	for _, f := range readColonFile(a.path("group"), 4) {
		gid, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		var members []string
		for _, m := range strings.Split(f[3], ",") {
			if m = strings.TrimSpace(m); m != "" {
				members = append(members, m)
			}
		}
		out = append(out, groupEntry{name: f[0], gid: gid, members: members})
	}
	return out
}

// readColonFile returns the lines with at least min colon-separated fields; a missing file is empty.
func readColonFile(path string, minFields int) [][]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Split(line, ":"); len(fields) >= minFields {
			out = append(out, fields)
		}
	}
	return out
}

// ID resolves a user or group id given as a number or a name. found is false for a name that does not exist.
func parseID(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// UID resolves an owner of a file: a number, or a user name of the container; empty is root.
func (a Accounts) UID(owner string) (int, error) {
	if owner == "" {
		return 0, nil
	}
	if n, ok := parseID(owner); ok {
		return n, nil
	}
	for _, e := range a.passwd() {
		if e.name == owner {
			return e.uid, nil
		}
	}
	return 0, errors.New("unknown owner")
}

// GID resolves a group of a file: a number, or a group name of the container; empty is root.
func (a Accounts) GID(group string) (int, error) {
	if group == "" {
		return 0, nil
	}
	if n, ok := parseID(group); ok {
		return n, nil
	}
	for _, e := range a.groups() {
		if e.name == group {
			return e.gid, nil
		}
	}
	return 0, errors.New("unknown group")
}

// Resolve reads a service user as an image configuration names it: "user", "uid", "user:group" or "uid:gid". The user may
// be a name of /etc/passwd or a number that /etc/passwd does not know (it then has group 0 and no home, like in a runtime).
// The supplementary groups are the groups of /etc/group that list the user by name.
func (a Accounts) Resolve(spec string) (Identity, error) {
	userPart, groupPart, _ := strings.Cut(spec, ":")
	var id Identity
	var name string
	passwd := a.passwd()
	if n, ok := parseID(userPart); ok {
		id.UID = n
		for _, e := range passwd {
			if e.uid == n {
				name, id.GID, id.Home = e.name, e.gid, e.home
				break
			}
		}
	} else {
		found := false
		for _, e := range passwd {
			if e.name == userPart {
				name, id.UID, id.GID, id.Home, found = e.name, e.uid, e.gid, e.home, true
				break
			}
		}
		if !found {
			return Identity{}, fmt.Errorf("the service user %q is not in /etc/passwd", userPart)
		}
	}
	if groupPart != "" {
		g, err := a.GID(groupPart)
		if err != nil {
			return Identity{}, fmt.Errorf("the service group %q is not in /etc/group", groupPart)
		}
		id.GID = g
	}
	if name != "" {
		for _, g := range a.groups() {
			for _, m := range g.members {
				if m == name {
					id.Groups = append(id.Groups, g.gid)
					break
				}
			}
		}
	}
	return id, nil
}
