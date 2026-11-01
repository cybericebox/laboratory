package laboratory

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The connection log is kept in daily files in a directory (a volume of the operator): egress-YYYY-MM-DD.jsonl while the day (UTC) is
// open, egress-YYYY-MM-DD.jsonl.gz once it is over. A file is one JSON row (EgressLogRow) per line.
const (
	egressLogPrefix = "egress-"
	egressLogPlain  = ".jsonl"
	egressLogGzip   = ".jsonl.gz"
	egressLogDay    = "2006-01-02"
)

// egressLogFiles lists the daily files of a directory with the day each one holds, oldest first.
func egressLogFiles(dir string) ([]struct{ Day, Path string }, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []struct{ Day, Path string }
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, egressLogPrefix) {
			continue
		}
		var day string
		switch {
		case strings.HasSuffix(n, egressLogGzip):
			day = strings.TrimSuffix(strings.TrimPrefix(n, egressLogPrefix), egressLogGzip)
		case strings.HasSuffix(n, egressLogPlain):
			day = strings.TrimSuffix(strings.TrimPrefix(n, egressLogPrefix), egressLogPlain)
		default:
			continue
		}
		if _, err := time.Parse(egressLogDay, day); err != nil {
			continue
		}
		out = append(out, struct{ Day, Path string }{day, filepath.Join(dir, n)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out, nil
}

// eachEgressLogRow calls fn for every row of a daily file (plain or gzip); a line that is not a row (a write cut short by a crash) is skipped.
func eachEgressLogRow(path string, fn func(EgressLogRow) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(path, egressLogGzip) {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer func() { _ = zr.Close() }()
		r = zr
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var row EgressLogRow
		if json.Unmarshal(sc.Bytes(), &row) != nil || row.Minute.IsZero() {
			continue
		}
		if !fn(row) {
			return nil
		}
	}
	return sc.Err()
}

// egressLogCursors is, for each namespace, the newest minute already in the log of the last two days: where to go on pulling after a restart.
func egressLogCursors(dir string, now time.Time) (map[string]time.Time, error) {
	files, err := egressLogFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]time.Time{}, nil
		}
		return nil, err
	}
	from := now.UTC().AddDate(0, 0, -1).Format(egressLogDay)
	cur := map[string]time.Time{}
	for _, f := range files {
		if f.Day < from {
			continue
		}
		if err := eachEgressLogRow(f.Path, func(r EgressLogRow) bool {
			if r.Minute.After(cur[r.Namespace]) {
				cur[r.Namespace] = r.Minute
			}
			return true
		}); err != nil {
			return nil, err
		}
	}
	return cur, nil
}

// compressEgressLogDays gzips every plain file of a day that is over, and removes the plain file once the gzip is complete.
func compressEgressLogDays(dir string, now time.Time) error {
	files, err := egressLogFiles(dir)
	if err != nil {
		return err
	}
	today := now.UTC().Format(egressLogDay)
	for _, f := range files {
		if f.Day >= today || !strings.HasSuffix(f.Path, egressLogPlain) {
			continue
		}
		if err := gzipFile(f.Path, strings.TrimSuffix(f.Path, egressLogPlain)+egressLogGzip); err != nil {
			return err
		}
	}
	return nil
}

func gzipFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
		}
	}()
	zw := gzip.NewWriter(out)
	if _, err = io.Copy(zw, in); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// removeEgressLogDaysBefore deletes the files of the days before the limit day.
func removeEgressLogDaysBefore(dir, limit string) error {
	files, err := egressLogFiles(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.Day < limit {
			if err := os.Remove(f.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

// EgressLogQuery selects rows of the connection log. Empty fields match everything; Host is a name, or "*.suffix" for the name and
// everything under it.
type EgressLogQuery struct {
	Host, Lab, Namespace string
	From, To             time.Time // From inclusive, To exclusive; zero = open
	Limit                int       // 0 = no limit
}

func (q EgressLogQuery) matches(r EgressLogRow) bool {
	if q.Namespace != "" && r.Namespace != q.Namespace || q.Lab != "" && r.Lab != q.Lab {
		return false
	}
	if !q.From.IsZero() && r.Minute.Before(q.From) || !q.To.IsZero() && !r.Minute.Before(q.To) {
		return false
	}
	switch {
	case q.Host == "":
	case strings.HasPrefix(q.Host, "*."):
		suffix := q.Host[1:]
		if r.Host != q.Host[2:] && !strings.HasSuffix(r.Host, suffix) {
			return false
		}
	case r.Host != q.Host:
		return false
	}
	return true
}

// SearchEgressLog writes the matching rows of the log in dir to w, one JSON row per line, oldest first. It reads only the files of the days
// the query can touch.
func SearchEgressLog(dir string, q EgressLogQuery, w io.Writer) (int, error) {
	files, err := egressLogFiles(dir)
	if err != nil {
		return 0, err
	}
	var from, to string
	if !q.From.IsZero() {
		from = q.From.UTC().Format(egressLogDay)
	}
	if !q.To.IsZero() {
		to = q.To.UTC().Format(egressLogDay)
	}
	enc := json.NewEncoder(w)
	n := 0
	for _, f := range files {
		if from != "" && f.Day < from || to != "" && f.Day > to {
			continue
		}
		var werr error
		if err := eachEgressLogRow(f.Path, func(r EgressLogRow) bool {
			if !q.matches(r) {
				return true
			}
			if werr = enc.Encode(r); werr != nil {
				return false
			}
			n++
			return q.Limit == 0 || n < q.Limit
		}); err != nil {
			return n, fmt.Errorf("%s: %w", filepath.Base(f.Path), err)
		}
		if werr != nil {
			return n, werr
		}
		if q.Limit > 0 && n >= q.Limit {
			break
		}
	}
	return n, nil
}
