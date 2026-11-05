package web

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/config"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/logging"
	npmsync "github.com/toyonix/nginx-proxy-manager-sync/internal/sync"
)

type fakeSched struct {
	mu       sync.Mutex
	running  bool
	last     *npmsync.RunResult
	triggers []npmsync.RunOptions
}

func (f *fakeSched) Trigger(_ string, o npmsync.RunOptions) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return false
	}
	f.triggers = append(f.triggers, o)
	return true
}
func (f *fakeSched) Running() bool            { return f.running }
func (f *fakeSched) Last() *npmsync.RunResult { return f.last }
func (f *fakeSched) Next() time.Time          { return time.Time{} }

func newServer(t *testing.T, mutate func(*config.Config)) (*httptest.Server, *fakeSched, *logging.Buffer, *Metrics) {
	t.Helper()
	cfg := config.Default()
	cfg.Master.URL = "http://127.0.0.1:1"
	cfg.Backups = []config.Backup{{URL: "http://127.0.0.1:2"}}
	if mutate != nil {
		mutate(&cfg)
	}
	sched := &fakeSched{}
	buf := logging.NewBuffer(1000)
	log := logging.New(io.Discard, "console", "debug", buf)
	m := NewMetrics()
	srv := httptest.NewServer(New(&cfg, sched, buf, m, "test", log).Handler())
	t.Cleanup(srv.Close)
	return srv, sched, buf, m
}

func do(t *testing.T, method, url string, header map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req) // no automatic gzip handling
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func successRun() *npmsync.RunResult {
	now := time.Now()
	exp := now.Add(48 * time.Hour)
	return &npmsync.RunResult{Trigger: "test", Started: now.Add(-2 * time.Second), Finished: now, Backups: []*npmsync.BackupResult{{
		URL: "http://b:81", Objects: []npmsync.ObjectResult{
			{Type: npmsync.TypeCertificate, Name: `a "quoted" cert`, Action: npmsync.ActionUnchanged, ExpiresOn: &exp},
			{Type: npmsync.TypeProxyHost, Name: "a.example.com", Action: npmsync.ActionCreated},
			{Type: npmsync.TypeProxyHost, Name: "b.example.com", Action: npmsync.ActionCreated},
		}}}}
}

func TestBasicAuth(t *testing.T) {
	srv, _, _, _ := newServer(t, func(c *config.Config) {
		c.Web.Username, c.Web.Password, c.Web.Metrics = "admin", "pw", true
	})
	for path, want := range map[string]int{"/": 401, "/app.js": 401, "/api/v1/status": 401, "/api/v1/logs": 401, "/healthz": 200, "/readyz": 503, "/metrics": 200} {
		if resp, _ := do(t, "GET", srv.URL+path, nil); resp.StatusCode != want {
			t.Errorf("GET %s without auth = %d, want %d", path, resp.StatusCode, want)
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/logs", nil)
	req.SetBasicAuth("admin", "pw")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 200 {
		t.Errorf("with auth = %d", resp.StatusCode)
	}
	req.SetBasicAuth("admin", "wrong")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Errorf("wrong password = %d", resp.StatusCode)
	}
}

func TestSyncEndpoint(t *testing.T) {
	srv, sched, _, _ := newServer(t, nil)
	if resp, _ := do(t, "POST", srv.URL+"/api/v1/sync?forcePasswords=true", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(sched.triggers) != 1 || !sched.triggers[0].ForcePasswords {
		t.Fatalf("triggers = %+v", sched.triggers)
	}
	sched.running = true
	if resp, _ := do(t, "POST", srv.URL+"/api/v1/sync", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("status while running = %d, want 409", resp.StatusCode)
	}
	sched.running = false
	if resp, _ := do(t, "GET", srv.URL+"/api/v1/sync", nil); resp.StatusCode < 400 || len(sched.triggers) != 1 {
		t.Fatalf("GET must not start a run: %d, triggers = %d", resp.StatusCode, len(sched.triggers))
	}
}

func TestCrossSiteWriteRejected(t *testing.T) {
	srv, sched, _, _ := newServer(t, nil)
	resp, _ := do(t, "POST", srv.URL+"/api/v1/sync", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"})
	if resp.StatusCode != http.StatusForbidden || len(sched.triggers) != 0 {
		t.Fatalf("cross-site POST = %d, triggers = %d", resp.StatusCode, len(sched.triggers))
	}
}

func TestReadyz(t *testing.T) {
	srv, sched, _, _ := newServer(t, nil)
	if resp, _ := do(t, "GET", srv.URL+"/readyz", nil); resp.StatusCode != 503 {
		t.Fatalf("before first run = %d", resp.StatusCode)
	}
	sched.last = successRun()
	if resp, _ := do(t, "GET", srv.URL+"/readyz", nil); resp.StatusCode != 200 {
		t.Fatalf("after success = %d", resp.StatusCode)
	}
	sched.last.Backups[0].Objects = append(sched.last.Backups[0].Objects, npmsync.ObjectResult{Action: npmsync.ActionSkipped, Failed: true})
	if resp, _ := do(t, "GET", srv.URL+"/readyz", nil); resp.StatusCode != 503 {
		t.Fatalf("with a failed skip = %d", resp.StatusCode)
	}
	sched.last.Backups[0].Objects = []npmsync.ObjectResult{{Action: npmsync.ActionDegraded}}
	if resp, _ := do(t, "GET", srv.URL+"/readyz", nil); resp.StatusCode != 200 {
		t.Fatalf("degraded must not fail readiness: %d", resp.StatusCode)
	}
}

func TestStatusAndLogs(t *testing.T) {
	srv, sched, buf, _ := newServer(t, nil)
	sched.last = successRun()
	resp, body := do(t, "GET", srv.URL+"/api/v1/status", nil)
	var st Status
	if err := json.Unmarshal([]byte(body), &st); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %v %s", resp.StatusCode, err, body)
	}
	if !st.Ready || st.LastRun == nil || len(st.Instances) != 2 || st.Instances[0].Reachable {
		t.Fatalf("status = %+v", st)
	}

	log := logging.New(io.Discard, "console", "debug", buf)
	log.Debug("debug line")
	log.Error("error line", slog.String("backup", "b"))
	_, body = do(t, "GET", srv.URL+"/api/v1/logs?level=error", nil)
	if !strings.Contains(body, "error line backup=b") || strings.Contains(body, "debug line") {
		t.Fatalf("logs = %s", body)
	}
	do(t, "POST", srv.URL+"/api/v1/logs/clear", nil)
	if len(buf.Entries("")) != 0 {
		t.Fatal("logs not cleared")
	}
}

func TestStaticFiles(t *testing.T) {
	srv, _, _, _ := newServer(t, nil)
	resp, body := do(t, "GET", srv.URL+"/", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "<title>npm-sync</title>") || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("index: %d", resp.StatusCode)
	}
	resp, body = do(t, "GET", srv.URL+"/tabler.min.css", map[string]string{"Accept-Encoding": "gzip"})
	if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatalf("css headers: %v", resp.Header)
	}
	zr, err := gzip.NewReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	css, _ := io.ReadAll(zr)
	if !strings.Contains(string(css), "Tabler v1.5.1") {
		t.Fatal("not tabler css")
	}
	_, plain := do(t, "GET", srv.URL+"/tabler.min.css", nil)
	if !strings.Contains(plain, "Tabler v1.5.1") {
		t.Fatal("css without gzip support")
	}
	if resp, _ := do(t, "GET", srv.URL+"/nope.js", nil); resp.StatusCode != 404 {
		t.Fatalf("missing file = %d", resp.StatusCode)
	}
}

func TestMetrics(t *testing.T) {
	srv, _, _, m := newServer(t, func(c *config.Config) { c.Web.Metrics = true })
	m.Observe(successRun())
	_, body := do(t, "GET", srv.URL+"/metrics", nil)
	for _, want := range []string{
		`npm_sync_run_total{result="success"} 1`,
		`npm_sync_run_total{result="failure"} 0`,
		"npm_sync_run_duration_seconds 2",
		"npm_sync_last_run_timestamp_seconds ",
		`npm_sync_objects{backup="http://b:81",type="proxy host",state="created"} 2`,
		`npm_sync_certificate_expiry_timestamp_seconds{backup="http://b:81",certificate="a \"quoted\" cert"} `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}

	off, _, _, _ := newServer(t, nil)
	if resp, _ := do(t, "GET", off.URL+"/metrics", nil); resp.StatusCode == 200 {
		t.Fatal("/metrics must be off unless enabled")
	}
}
