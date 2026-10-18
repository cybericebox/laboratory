package proxywg

import (
	"time"

	"github.com/cybericebox/laboratory/internal/proxy/demux"
)

// pinnedSessions is what the restore reads from the fast path (fastpath.FastPath).
type pinnedSessions interface {
	Pinned() ([]demux.Forward, error)
	LastSeen() (map[uint32]time.Time, error)
	Delete(index uint32) error
}

// restoreSessions puts the sessions a previous demux left in the fast path back into the table, once the table knows every
// group: a session whose server is a group's resolved VPN address comes back in that group (the kernel kept forwarding it
// meanwhile). Every other pinned record is deleted: a server no group has, half a session, a session past its TTL or one the
// table refuses. An index a live session took in the meantime is left alone, its record is that session's.
func restoreSessions(fp pinnedSessions, ct *demux.ConnTrack, groups []demux.TableEntry) (restored, dropped int, err error) {
	pinned, err := fp.Pinned()
	if err != nil {
		return 0, 0, err
	}
	seen, err := fp.LastSeen()
	if err != nil {
		return 0, 0, err
	}
	owners := map[uint32]demux.Forward{}
	replies := map[uint32]demux.Forward{} // by session
	var orphans []uint32
	for _, f := range pinned {
		switch {
		case !f.ToClient && f.Session == f.Index:
			owners[f.Index] = f
		case f.ToClient && f.Session != f.Index:
			if _, dup := replies[f.Session]; dup {
				orphans = append(orphans, f.Index)
				continue
			}
			replies[f.Session] = f
		default:
			orphans = append(orphans, f.Index)
		}
	}
	drop := func(indices ...uint32) {
		dropped++
		for _, idx := range indices {
			if _, live := ct.Expected(idx); !live {
				_ = fp.Delete(idx)
			}
		}
	}
	for si, owner := range owners {
		reply, ok := replies[si]
		delete(replies, si)
		if !ok {
			drop(si)
			continue
		}
		group, known := groupOf(owner.Dest, groups)
		at, haveSeen := seen[si]
		if !known || !haveSeen || !sameSocket(reply.Expected, owner.Dest) || !sameSocket(reply.Dest, owner.Expected) ||
			!ct.Restore(reply.Index, si, owner.Expected, owner.Dest, group, at, reply.ReplyIfindex, reply.NewSource) {
			drop(reply.Index, si)
			continue
		}
		restored++
	}
	for _, reply := range replies {
		drop(reply.Index)
	}
	for _, idx := range orphans {
		drop(idx)
	}
	return restored, dropped, nil
}

func groupOf(server demux.Socket, groups []demux.TableEntry) (string, bool) {
	for _, g := range groups {
		if g.Resolved != nil && g.Resolved.IP.Equal(server.IP) && g.Resolved.Port == int(server.Port) {
			return g.UID, true
		}
	}
	return "", false
}

func sameSocket(a, b demux.Socket) bool { return a.Port == b.Port && a.IP.Equal(b.IP) }
