// Package web serves the web UI and HTTP API (spec section 7).
package web

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/config"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/logging"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
	npmsync "github.com/toyonix/nginx-proxy-manager-sync/internal/sync"
)

//go:embed static
var staticFS embed.FS

// Scheduler is what the server needs from the scheduler.
type Scheduler interface {
	Trigger(trigger string, opts npmsync.RunOptions) bool
	Running() bool
	Last() *npmsync.RunResult
	Next() time.Time
}

// Server is the web UI and API.
type Server struct {
	cfg     *config.Config
	sched   Scheduler
	logs    *logging.Buffer
	metrics *Metrics
	log     *slog.Logger
	version string

	probeMu   sync.Mutex
	probed    time.Time
	instances []InstanceStatus
}

// New returns a server.
func New(cfg *config.Config, sched Scheduler, logs *logging.Buffer, metrics *Metrics, version string, log *slog.Logger) *Server {
	return &Server{cfg: cfg, sched: sched, logs: logs, metrics: metrics, version: version, log: log}
}

// Handler returns the HTTP handler. Basic auth, if configured, protects the
// UI and /api/*, but not /healthz, /readyz or /metrics (spec 7.2).
func (s *Server) Handler() http.Handler {
	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/v1/status", s.status)
	protected.HandleFunc("POST /api/v1/sync", s.sync)
	protected.HandleFunc("GET /api/v1/logs", s.logsGet)
	protected.HandleFunc("POST /api/v1/logs/clear", s.logsClear)
	protected.HandleFunc("GET /", s.static)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK); io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", s.readyz)
	if s.cfg.Web.Metrics {
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			s.metrics.Write(w)
		})
	}
	mux.Handle("/", s.basicAuth(protected))

	// Browsers must not be able to trigger writes from another site.
	return http.NewCrossOriginProtection().Handler(securityHeaders(mux))
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) basicAuth(h http.Handler) http.Handler {
	user, pass := s.cfg.Web.Username, s.cfg.Web.Password
	if user == "" && pass == "" {
		return h
	}
	wantUser, wantPass := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		gotUser, gotPass := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(p))
		if !ok || subtle.ConstantTimeCompare(gotUser[:], wantUser[:])&subtle.ConstantTimeCompare(gotPass[:], wantPass[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="npm-sync", charset="UTF-8"`)
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ---- API ----

// InstanceStatus is the reachability of one NPM instance.
type InstanceStatus struct {
	Role      string `json:"role"` // master or backup
	URL       string `json:"url"`
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Status is the answer of GET /api/v1/status.
type Status struct {
	Version    string              `json:"version"`
	Running    bool                `json:"running"`
	DryRun     bool                `json:"dryRun"`
	Cron       string              `json:"cron"`
	NextRun    *time.Time          `json:"nextRun,omitempty"`
	Ready      bool                `json:"ready"`
	Instances  []InstanceStatus    `json:"instances"`
	LastRun    *npmsync.RunResult  `json:"lastRun"`
	Features   map[string][]string `json:"features"`
	MaxDeletes int                 `json:"maxDeletes"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	st := Status{
		Version:    s.version,
		Running:    s.sched.Running(),
		DryRun:     s.cfg.DryRun,
		Cron:       s.cfg.Cron,
		LastRun:    s.sched.Last(),
		Instances:  s.probe(r.Context()),
		Features:   map[string][]string{},
		MaxDeletes: s.cfg.MaxDeletes,
	}
	if next := s.sched.Next(); !next.IsZero() {
		st.NextRun = &next
	}
	st.Ready = st.LastRun != nil && st.LastRun.Succeeded()
	for i, b := range s.cfg.Backups {
		for _, f := range config.AllFeatures {
			if s.cfg.FeatureEnabled(i, f) {
				st.Features[b.URL] = append(st.Features[b.URL], f)
			}
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// probe checks every instance's reachability (GET /api/, no login),
// cached for 15 seconds.
func (s *Server) probe(ctx context.Context) []InstanceStatus {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if time.Since(s.probed) < 15*time.Second && s.instances != nil {
		return s.instances
	}
	insts := []InstanceStatus{{Role: "master", URL: s.cfg.Master.URL}}
	for _, b := range s.cfg.Backups {
		insts = append(insts, InstanceStatus{Role: "backup", URL: b.URL})
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range insts {
		wg.Add(1)
		go func(in *InstanceStatus) {
			defer wg.Done()
			c, err := npm.New(in.URL, "", "", 4*time.Second, nil)
			if err != nil {
				in.Error = err.Error()
				return
			}
			c.Backoff = nil // a single attempt
			v, err := c.Version(ctx)
			if err != nil {
				in.Error = err.Error()
				return
			}
			in.Reachable, in.Version = true, v.String()
		}(&insts[i])
	}
	wg.Wait()
	s.instances, s.probed = insts, time.Now()
	return insts
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	opts := npmsync.RunOptions{ForcePasswords: r.URL.Query().Get("forcePasswords") == "true"}
	if !s.sched.Trigger("manual", opts) {
		writeJSON(w, http.StatusConflict, map[string]any{"started": false, "message": "a run is already in progress"})
		return
	}
	s.log.Info("sync started from the web UI or API", "forcePasswords", opts.ForcePasswords, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "forcePasswords": opts.ForcePasswords})
}

func (s *Server) logsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": s.logs.Entries(r.URL.Query().Get("level"))})
}

func (s *Server) logsClear(w http.ResponseWriter, _ *http.Request) {
	s.logs.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}

// readyz answers 200 if the last run succeeded for every backup, otherwise
// 503, also before the first run finishes (spec 7.2).
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	last := s.sched.Last()
	switch {
	case last == nil:
		http.Error(w, "no sync run has finished yet", http.StatusServiceUnavailable)
	case !last.Succeeded():
		http.Error(w, "the last sync run did not succeed for every backup", http.StatusServiceUnavailable)
	default:
		io.WriteString(w, "ok\n")
	}
}

// ---- static files ----

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	sub, _ := fs.Sub(staticFS, "static")

	// Large files are embedded gzip-compressed.
	if data, err := fs.ReadFile(sub, name+".gz"); err == nil {
		setContentType(w, name)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("Vary", "Accept-Encoding")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(data)
			return
		}
		zr, err := gzip.NewReader(strings.NewReader(string(data)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		io.Copy(w, zr)
		return
	}
	data, err := fs.ReadFile(sub, name)
	if err != nil || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	setContentType(w, name)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func setContentType(w http.ResponseWriter, name string) {
	types := map[string]string{
		".html": "text/html; charset=utf-8",
		".css":  "text/css; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
		".svg":  "image/svg+xml",
	}
	for ext, t := range types {
		if strings.HasSuffix(name, ext) {
			w.Header().Set("Content-Type", t)
			return
		}
	}
}
