package web

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	npmsync "github.com/toyonix/nginx-proxy-manager-sync/internal/sync"
)

// Metrics renders the Prometheus metrics of spec 7.3 in the text exposition
// format. It is small enough not to need the Prometheus client library.
type Metrics struct {
	mu   sync.Mutex
	runs map[string]int
	last *npmsync.RunResult
}

// NewMetrics returns empty metrics.
func NewMetrics() *Metrics { return &Metrics{runs: map[string]int{"success": 0, "failure": 0}} }

// Observe records a finished run.
func (m *Metrics) Observe(r *npmsync.RunResult) {
	if r == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Succeeded() {
		m.runs["success"]++
	} else {
		m.runs["failure"]++
	}
	m.last = r
}

// Write writes every metric to w.
func (m *Metrics) Write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Fprintln(w, "# HELP npm_sync_run_total Sync runs, by result.")
	fmt.Fprintln(w, "# TYPE npm_sync_run_total counter")
	for _, result := range []string{"success", "failure"} {
		fmt.Fprintf(w, "npm_sync_run_total{result=%q} %d\n", result, m.runs[result])
	}
	if m.last == nil {
		return
	}
	r := m.last

	fmt.Fprintln(w, "# HELP npm_sync_run_duration_seconds Duration of the last run.")
	fmt.Fprintln(w, "# TYPE npm_sync_run_duration_seconds gauge")
	fmt.Fprintf(w, "npm_sync_run_duration_seconds %g\n", r.Finished.Sub(r.Started).Seconds())

	fmt.Fprintln(w, "# HELP npm_sync_last_run_timestamp_seconds When the last run finished, as a Unix timestamp.")
	fmt.Fprintln(w, "# TYPE npm_sync_last_run_timestamp_seconds gauge")
	fmt.Fprintf(w, "npm_sync_last_run_timestamp_seconds %d\n", r.Finished.Unix())

	type objKey struct{ backup, typ, state string }
	objects := map[objKey]int{}
	type certKey struct{ backup, cert string }
	expiry := map[certKey]int64{}
	for _, b := range r.Backups {
		for _, o := range b.Objects {
			objects[objKey{b.URL, o.Type, string(o.Action)}]++
			if o.Type == npmsync.TypeCertificate && o.ExpiresOn != nil && o.Action != npmsync.ActionDeleted && !o.IsError() {
				expiry[certKey{b.URL, o.Name}] = o.ExpiresOn.Unix()
			}
		}
	}

	fmt.Fprintln(w, "# HELP npm_sync_objects Objects in the last run, by backup, type and state.")
	fmt.Fprintln(w, "# TYPE npm_sync_objects gauge")
	okeys := make([]objKey, 0, len(objects))
	for k := range objects {
		okeys = append(okeys, k)
	}
	sort.Slice(okeys, func(i, j int) bool {
		return okeys[i].backup+okeys[i].typ+okeys[i].state < okeys[j].backup+okeys[j].typ+okeys[j].state
	})
	for _, k := range okeys {
		fmt.Fprintf(w, "npm_sync_objects{backup=\"%s\",type=\"%s\",state=\"%s\"} %d\n", esc(k.backup), esc(k.typ), esc(k.state), objects[k])
	}

	fmt.Fprintln(w, "# HELP npm_sync_certificate_expiry_timestamp_seconds Expiry of each certificate copy, as a Unix timestamp.")
	fmt.Fprintln(w, "# TYPE npm_sync_certificate_expiry_timestamp_seconds gauge")
	ckeys := make([]certKey, 0, len(expiry))
	for k := range expiry {
		ckeys = append(ckeys, k)
	}
	sort.Slice(ckeys, func(i, j int) bool { return ckeys[i].backup+ckeys[i].cert < ckeys[j].backup+ckeys[j].cert })
	for _, k := range ckeys {
		fmt.Fprintf(w, "npm_sync_certificate_expiry_timestamp_seconds{backup=\"%s\",certificate=\"%s\"} %d\n", esc(k.backup), esc(k.cert), expiry[k])
	}
}

// esc escapes a Prometheus label value.
func esc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
