package manager

import (
	"flag"
	"fmt"
	"os"
	"time"

	laboratorycontroller "github.com/cybericebox/laboratory/internal/controller/laboratory"
)

// RunEgressLog is `/manager egress-log`: it searches the connection log of the internet egress that the operator keeps in its volume, for
// the cluster's administrator (kubectl exec deploy/laboratory-controller-manager -- /manager egress-log --host pypi.org). One JSON row per line.
func RunEgressLog(args []string) int {
	fs := flag.NewFlagSet("egress-log", flag.ContinueOnError)
	dir := fs.String("dir", os.Getenv("EGRESS_LOG_DIR"), "the log directory (default: EGRESS_LOG_DIR of the operator)")
	host := fs.String("host", "", "host name, or *.suffix for the name and everything under it")
	lab := fs.String("lab", "", "lab name")
	namespace := fs.String("namespace", "", "namespace of the lab group")
	from := fs.String("from", "", "from this time, inclusive (RFC 3339, or a date YYYY-MM-DD, UTC)")
	to := fs.String("to", "", "until this time, exclusive")
	limit := fs.Int("limit", 0, "at most this many rows (0 = all)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "no log directory: the connection log is kept only with internetEgress.log.persistence.enabled (or log.dir)")
		return 2
	}
	q := laboratorycontroller.EgressLogQuery{Host: *host, Lab: *lab, Namespace: *namespace, Limit: *limit}
	var err error
	if q.From, err = parseLogTime(*from); err != nil {
		fmt.Fprintln(os.Stderr, "--from:", err)
		return 2
	}
	if q.To, err = parseLogTime(*to); err != nil {
		fmt.Fprintln(os.Stderr, "--to:", err)
		return 2
	}
	if _, err := laboratorycontroller.SearchEgressLog(*dir, q, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "search:", err)
		return 1
	}
	return 0
}

func parseLogTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}
