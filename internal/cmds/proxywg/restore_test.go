package proxywg

import (
	"net"
	"slices"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/proxy/demux"
)

type fakeFastPath struct {
	pinned  []demux.Forward
	seen    map[uint32]time.Time
	deleted []uint32
}

func (f *fakeFastPath) Pinned() ([]demux.Forward, error)        { return f.pinned, nil }
func (f *fakeFastPath) LastSeen() (map[uint32]time.Time, error) { return f.seen, nil }
func (f *fakeFastPath) Delete(i uint32) error                   { f.deleted = append(f.deleted, i); return nil }

func sock(ip string, port uint16) demux.Socket { return demux.Socket{IP: net.ParseIP(ip), Port: port} }

var (
	probe = sock("10.0.2.160", 51820)
	entry = sock("192.168.16.3", 31820)
)

// pair is a session as the fast path holds it: owner si (client -> server) and the reply direction ci.
func pair(ci, si uint32, client, server demux.Socket, replyIf int) []demux.Forward {
	return []demux.Forward{
		{Index: si, Session: si, Expected: client, NewSource: probe, Dest: server},
		{Index: ci, Session: si, Expected: server, NewSource: entry, Dest: client, ToClient: true, Via: probe, ReplyIfindex: replyIf},
	}
}

// TestRestoreDropsUnknownServers: after a restart the sessions to a server of a known group come back into the table; the
// pinned records of every other server, of half sessions, and of sessions past their TTL are deleted from the fast path.
func TestRestoreDropsUnknownServers(t *testing.T) {
	now := time.Now()
	known, gone := sock("10.0.1.5", 51820), sock("10.0.9.9", 51820)
	clientA, clientB, clientC := sock("198.51.100.1", 40000), sock("198.51.100.2", 40000), sock("198.51.100.3", 40000)
	fp := &fakeFastPath{seen: map[uint32]time.Time{2: now.Add(-time.Minute), 4: now, 6: now.Add(-time.Hour)}}
	fp.pinned = append(fp.pinned, pair(1, 2, clientA, known, 17)...)                                                      // restored
	fp.pinned = append(fp.pinned, pair(3, 4, clientB, gone, 0)...)                                                        // unknown server
	fp.pinned = append(fp.pinned, pair(5, 6, clientC, known, 0)...)                                                       // past its TTL
	fp.pinned = append(fp.pinned, pair(7, 8, clientC, known, 0)[1])                                                       // a reply direction without its owner
	fp.pinned = append(fp.pinned, demux.Forward{Index: 9, Session: 10, Expected: clientC, NewSource: probe, Dest: known}) // not an owner

	ct := demux.NewConnTrack()
	groups := []demux.TableEntry{{UID: "g", Resolved: &net.UDPAddr{IP: known.IP, Port: int(known.Port)}}, {UID: "unresolved"}}
	restored, dropped, err := restoreSessions(fp, ct, groups)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 1 || dropped != 4 {
		t.Fatalf("restored %d dropped %d, want 1 and 4", restored, dropped)
	}
	slices.Sort(fp.deleted)
	if want := []uint32{3, 4, 5, 6, 7, 9}; !slices.Equal(fp.deleted, want) {
		t.Fatalf("deleted %v, want %v", fp.deleted, want)
	}
	if dst, ok := ct.LookupForward(2, clientA); !ok || !dst.IP.Equal(known.IP) {
		t.Fatalf("the restored session does not forward: %v %v", dst, ok)
	}
	for _, f := range ct.Sessions() {
		if f.ToClient && f.ReplyIfindex != 17 {
			t.Fatalf("the reply device was not restored: %+v", f)
		}
	}
	// the group of the restored session is known: retiring it removes the session
	ct.RemoveGroup("g")
	if ct.Len() != 0 {
		t.Fatal("the restored session is not in its group")
	}
}

// A pinned index that a live session took meanwhile is neither restored nor deleted (the record is the live session's).
func TestRestoreKeepsALiveSessionsRecord(t *testing.T) {
	server := sock("10.0.1.5", 51820)
	ct := demux.NewConnTrack()
	if !ct.AddPartial(1, sock("203.0.113.9", 1), server, "g") || !ct.Complete(2, 1, sock("203.0.113.9", 1), server) {
		t.Fatal("live session")
	}
	fp := &fakeFastPath{seen: map[uint32]time.Time{2: time.Now()}, pinned: pair(1, 2, sock("198.51.100.1", 40000), server, 0)}
	groups := []demux.TableEntry{{UID: "g", Resolved: &net.UDPAddr{IP: server.IP, Port: int(server.Port)}}}
	if restored, _, err := restoreSessions(fp, ct, groups); err != nil || restored != 0 {
		t.Fatalf("restored %d err %v", restored, err)
	}
	if len(fp.deleted) != 0 {
		t.Fatalf("deleted the live session's records: %v", fp.deleted)
	}
}
