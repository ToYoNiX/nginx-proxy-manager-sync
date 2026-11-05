// Package npm is a client for the Nginx Proxy Manager admin API, with the
// retry rules of spec 4.7 and 4.8.
package npm

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrTwoFactor is returned by Login when the NPM user has 2FA enabled.
var ErrTwoFactor = errors.New("user has 2FA enabled; use a dedicated NPM user without 2FA for npm-sync")

// APIError is a non-2xx answer from NPM.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s %s: %d: %s", e.Method, e.Path, e.Status, e.Message)
	}
	return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// IsStatus reports whether err is an APIError with the given status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// Client talks to one NPM instance. It is safe for concurrent use.
type Client struct {
	base     *url.URL
	username string
	password string
	http     *http.Client
	log      *slog.Logger

	// Backoff lists the waits between attempts; its length + 1 is the
	// number of attempts (spec 4.8: 3 attempts, 1 s then 2 s).
	Backoff []time.Duration
	// Jitter is the maximum random fraction added to each wait.
	Jitter float64
	// Sleep waits between attempts; replaced in tests.
	Sleep func(ctx context.Context, d time.Duration) error

	mu    sync.Mutex
	token string
}

// New returns a client for the NPM instance at baseURL.
func New(baseURL, username, password string, timeout time.Duration, log *slog.Logger) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{
		base:     u,
		username: username,
		password: password,
		http:     &http.Client{Timeout: timeout},
		log:      log,
		Backoff:  []time.Duration{time.Second, 2 * time.Second},
		Jitter:   0.2,
		Sleep:    sleepCtx,
	}, nil
}

// URL returns the instance's base URL.
func (c *Client) URL() string { return c.base.String() }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- request plumbing ----

type request struct {
	method      string
	path        string
	body        []byte
	contentType string
	auth        bool
}

type response struct {
	status int
	body   []byte
}

// send performs one HTTP exchange. A non-2xx status is returned as an
// *APIError together with the response.
func (c *Client) send(ctx context.Context, r request) (*response, error) {
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + r.path
	if i := strings.IndexByte(r.path, '?'); i >= 0 {
		u.Path = strings.TrimRight(c.base.Path, "/") + r.path[:i]
		u.RawQuery = r.path[i+1:]
	}
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	req.Header.Set("Accept", "application/json")
	if r.auth {
		c.mu.Lock()
		tok := c.token
		c.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Debug("npm request failed", "method", r.method, "path", r.path, "error", err)
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	c.log.Debug("npm request", "method", r.method, "path", r.path, "status", resp.StatusCode, "duration", time.Since(start).Round(time.Millisecond))
	out := &response{status: resp.StatusCode, body: data}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, &APIError{Method: r.method, Path: r.path, Status: resp.StatusCode, Message: errorMessage(data)}
	}
	return out, nil
}

func errorMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// IsTransient reports whether err is worth retrying (spec 4.8): connection
// refused or reset, timeouts, and HTTP 502, 503 and 504.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if ae, ok := errors.AsType[*APIError](err); ok {
		return ae.Status == http.StatusBadGateway || ae.Status == http.StatusServiceUnavailable || ae.Status == http.StatusGatewayTimeout
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// do sends an authenticated request, re-logging in once on 401 (spec 4.7).
func (c *Client) do(ctx context.Context, r request) (*response, error) {
	r.auth = true
	resp, err := c.send(ctx, r)
	if IsStatus(err, http.StatusUnauthorized) {
		c.log.Debug("npm token rejected, logging in again", "path", r.path)
		if lerr := c.Login(ctx); lerr != nil {
			return nil, lerr
		}
		resp, err = c.send(ctx, r)
	}
	return resp, err
}

// retry runs fn up to len(c.Backoff)+1 times while it fails with a
// transient error. attempt starts at 1.
func (c *Client) retry(ctx context.Context, what string, fn func(attempt int) error) error {
	var err error
	for attempt := 1; ; attempt++ {
		err = fn(attempt)
		if err == nil || !IsTransient(err) || attempt > len(c.Backoff) {
			return err
		}
		wait := c.Backoff[attempt-1]
		if c.Jitter > 0 {
			wait += time.Duration(rand.Float64() * c.Jitter * float64(wait))
		}
		c.log.Warn("npm request failed, retrying", "instance", c.URL(), "request", what, "attempt", attempt, "wait", wait.Round(time.Millisecond), "error", err)
		if serr := c.Sleep(ctx, wait); serr != nil {
			return err
		}
	}
}

// getJSON GETs path and decodes the answer into out. GETs are repeated as-is.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	return c.retry(ctx, "GET "+path, func(int) error {
		resp, err := c.do(ctx, request{method: http.MethodGet, path: path})
		if err != nil {
			return err
		}
		return decode(resp.body, out)
	})
}

func decode(body []byte, out any) error {
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decoding NPM response: %w", err)
	}
	return nil
}

// sendJSON sends a PUT (or other idempotent write) with a JSON body,
// repeated as-is on transient errors.
func (c *Client) sendJSON(ctx context.Context, method, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.retry(ctx, method+" "+path, func(int) error {
		resp, err := c.do(ctx, request{method: method, path: path, body: body, contentType: "application/json"})
		if err != nil {
			return err
		}
		return decode(resp.body, out)
	})
}

// deleteObject sends a DELETE. A 404 on a retry counts as success, because
// the first attempt already deleted the object (spec 4.8).
func (c *Client) deleteObject(ctx context.Context, path string) error {
	return c.retry(ctx, "DELETE "+path, func(attempt int) error {
		_, err := c.do(ctx, request{method: http.MethodDelete, path: path})
		if attempt > 1 && IsStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	})
}

// create POSTs a new object. A create is never repeated blindly: after a
// transient failure, find is called first and, if the object exists, the
// create counts as successful (spec 4.8).
func (c *Client) create(ctx context.Context, path string, payload any, out any, find func(ctx context.Context) (json.RawMessage, bool, error)) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.retry(ctx, "POST "+path, func(attempt int) error {
		if attempt > 1 {
			raw, found, ferr := find(ctx)
			if ferr != nil {
				return ferr
			}
			if found {
				c.log.Info("create had succeeded despite the error; not sending it again", "instance", c.URL(), "path", path)
				return decode(raw, out)
			}
		}
		resp, err := c.do(ctx, request{method: http.MethodPost, path: path, body: body, contentType: "application/json"})
		if err != nil {
			return err
		}
		return decode(resp.body, out)
	})
}

// ---- API ----

// Version reads the instance's NPM version (GET /api, no login needed).
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out struct {
		Version Version `json:"version"`
	}
	err := c.retry(ctx, "GET /api/", func(int) error {
		resp, err := c.send(ctx, request{method: http.MethodGet, path: "/api/"})
		if err != nil {
			return err
		}
		return json.Unmarshal(resp.body, &out)
	})
	return out.Version, err
}

// Login obtains a session token (POST /api/tokens).
func (c *Client) Login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"identity": c.username, "secret": c.password})
	var out struct {
		Token       string `json:"token"`
		Requires2FA bool   `json:"requires_2fa"`
	}
	err := c.retry(ctx, "POST /api/tokens", func(int) error {
		resp, err := c.send(ctx, request{method: http.MethodPost, path: "/api/tokens", body: body, contentType: "application/json"})
		if err != nil {
			return err
		}
		return json.Unmarshal(resp.body, &out)
	})
	if err != nil {
		return fmt.Errorf("login to %s failed: %w", c.URL(), err)
	}
	if out.Requires2FA {
		return fmt.Errorf("login to %s: %w", c.URL(), ErrTwoFactor)
	}
	if out.Token == "" {
		return fmt.Errorf("login to %s: no token in response", c.URL())
	}
	c.mu.Lock()
	c.token = out.Token
	c.mu.Unlock()
	return nil
}

// Certificates lists every certificate.
//
// NPM (up to at least v2.16.0) returns the full PEM contents of custom
// certificates, private key included, in their meta. They are moved into
// Certificate.Files (with a hash in FilesHash) and replaced with true in
// Meta, so they are never logged. Callers that do not need the files call
// WipeFiles. The key is only usable when certificate and key are both
// present as text.
func (c *Client) Certificates(ctx context.Context) ([]Certificate, error) {
	var out []Certificate
	err := c.retry(ctx, "GET /api/nginx/certificates", func(int) error {
		resp, err := c.do(ctx, request{method: http.MethodGet, path: "/api/nginx/certificates"})
		if err != nil {
			return err
		}
		defer wipe(resp.body)
		out = nil
		return decode(resp.body, &out)
	})
	for i := range out {
		cert, _ := out[i].Meta["certificate"].(string)
		key, _ := out[i].Meta["certificate_key"].(string)
		chain, _ := out[i].Meta["intermediate_certificate"].(string)
		if cert != "" {
			h := sha256.New()
			fmt.Fprintf(h, "%d:%s%d:%s", len(cert), cert, len(chain), chain)
			out[i].CertHash = hex.EncodeToString(h.Sum(nil))
		}
		if cert != "" && key != "" {
			f := &CertFiles{Certificate: []byte(cert), PrivateKey: []byte(key)}
			if chain != "" {
				f.Chain = []byte(chain)
			}
			out[i].Files = f
		}
		for _, k := range sslFileKeys {
			if v, ok := out[i].Meta[k]; ok {
				out[i].Meta[k] = truthy(v)
			}
		}
	}
	return out, err
}

// WipeFiles wipes and drops the files of every certificate in the list.
func WipeFiles(certs []Certificate) {
	for i := range certs {
		if certs[i].Files != nil {
			certs[i].Files.Wipe()
			certs[i].Files = nil
		}
	}
}

// sslFileKeys are the certificate meta keys that hold file contents.
var sslFileKeys = []string{"certificate", "certificate_key", "intermediate_certificate"}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// CreateCustomCertificate creates an empty custom ("other") certificate.
// find locates a certificate created by an earlier attempt.
func (c *Client) CreateCustomCertificate(ctx context.Context, niceName string, find func([]Certificate) (Certificate, bool)) (Certificate, error) {
	payload := map[string]any{"provider": ProviderOther, "nice_name": niceName, "domain_names": []string{}, "meta": map[string]any{}}
	var out Certificate
	err := c.create(ctx, "/api/nginx/certificates", payload, &out, func(ctx context.Context) (json.RawMessage, bool, error) {
		certs, err := c.Certificates(ctx)
		if err != nil {
			return nil, false, err
		}
		WipeFiles(certs)
		cert, ok := find(certs)
		if !ok {
			return nil, false, nil
		}
		raw, err := json.Marshal(cert)
		return raw, true, err
	})
	return out, err
}

// UploadCertificate uploads files to a custom certificate. Uploading again
// replaces the files; it is repeated as-is on transient errors.
func (c *Client) UploadCertificate(ctx context.Context, id int, files CertFiles) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	parts := []struct {
		field string
		data  []byte
	}{
		{"certificate", files.Certificate},
		{"certificate_key", files.PrivateKey},
		{"intermediate_certificate", files.Chain},
	}
	for _, p := range parts {
		if len(p.data) == 0 {
			continue
		}
		w, err := mw.CreateFormFile(p.field, p.field+".pem")
		if err != nil {
			return err
		}
		if _, err := w.Write(p.data); err != nil {
			return err
		}
	}
	if err := mw.Close(); err != nil {
		return err
	}
	body := buf.Bytes()
	defer wipe(body)
	path := fmt.Sprintf("/api/nginx/certificates/%d/upload", id)
	return c.retry(ctx, "POST "+path, func(int) error {
		// The response echoes the uploaded files, key included.
		resp, err := c.do(ctx, request{method: http.MethodPost, path: path, body: body, contentType: mw.FormDataContentType()})
		if resp != nil {
			wipe(resp.body)
		}
		return err
	})
}

// DownloadCertificate downloads a Let's Encrypt certificate's files. The zip
// is read in memory and never written to disk (spec 4.2.1).
func (c *Client) DownloadCertificate(ctx context.Context, id int) (CertFiles, error) {
	path := fmt.Sprintf("/api/nginx/certificates/%d/download", id)
	var files CertFiles
	err := c.retry(ctx, "GET "+path, func(int) error {
		resp, err := c.do(ctx, request{method: http.MethodGet, path: path})
		if err != nil {
			return err
		}
		defer wipe(resp.body)
		files, err = parseCertZip(resp.body)
		return err
	})
	return files, err
}

func parseCertZip(data []byte) (CertFiles, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return CertFiles{}, fmt.Errorf("certificate download is not a zip: %w", err)
	}
	var files CertFiles
	for _, f := range zr.File {
		name := strings.ToLower(f.Name)
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		var dst *[]byte
		switch {
		case strings.HasPrefix(name, "cert"):
			dst = &files.Certificate
		case strings.HasPrefix(name, "chain"):
			dst = &files.Chain
		case strings.HasPrefix(name, "privkey"):
			dst = &files.PrivateKey
		default:
			continue // fullchain is not needed
		}
		rc, err := f.Open()
		if err != nil {
			files.Wipe()
			return CertFiles{}, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err != nil {
			files.Wipe()
			return CertFiles{}, err
		}
		*dst = b
	}
	if len(files.Certificate) == 0 || len(files.PrivateKey) == 0 {
		files.Wipe()
		return CertFiles{}, errors.New("certificate download is missing cert or privkey")
	}
	return files, nil
}

// DeleteCertificate deletes a certificate.
func (c *Client) DeleteCertificate(ctx context.Context, id int) error {
	return c.deleteObject(ctx, fmt.Sprintf("/api/nginx/certificates/%d", id))
}

// Hosts lists every host of a kind.
func (c *Client) Hosts(ctx context.Context, kind HostKind) ([]Host, error) {
	var out []Host
	err := c.getJSON(ctx, kind.Path, &out)
	return out, err
}

// CreateHost creates a host. After a transient failure, existing hosts
// are checked with same (by domain set, or port for streams) before the
// create is sent again.
func (c *Client) CreateHost(ctx context.Context, kind HostKind, payload map[string]any, same func(Host) bool) (Host, error) {
	var out Host
	err := c.create(ctx, kind.Path, payload, &out, func(ctx context.Context) (json.RawMessage, bool, error) {
		hosts, err := c.Hosts(ctx, kind)
		if err != nil {
			return nil, false, err
		}
		for _, h := range hosts {
			if same(h) {
				raw, err := json.Marshal(h.Fields)
				return raw, true, err
			}
		}
		return nil, false, nil
	})
	return out, err
}

// UpdateHost replaces a host's writable fields.
func (c *Client) UpdateHost(ctx context.Context, kind HostKind, id int, payload map[string]any) (Host, error) {
	var out Host
	err := c.sendJSON(ctx, http.MethodPut, fmt.Sprintf("%s/%d", kind.Path, id), payload, &out)
	return out, err
}

// DeleteHost deletes a host.
func (c *Client) DeleteHost(ctx context.Context, kind HostKind, id int) error {
	return c.deleteObject(ctx, fmt.Sprintf("%s/%d", kind.Path, id))
}

// ---- access lists ----

const accessListsPath = "/api/nginx/access-lists"

// AccessLists lists every access list with its users and client rules.
func (c *Client) AccessLists(ctx context.Context) ([]AccessList, error) {
	var out []AccessList
	err := c.getJSON(ctx, accessListsPath+"?expand=items,clients", &out)
	return out, err
}

// CreateAccessList creates an access list. After a transient failure the
// list is looked up by name before the create is sent again.
func (c *Client) CreateAccessList(ctx context.Context, payload map[string]any) (AccessList, error) {
	var out AccessList
	name, _ := payload["name"].(string)
	err := c.create(ctx, accessListsPath, payload, &out, func(ctx context.Context) (json.RawMessage, bool, error) {
		lists, err := c.AccessLists(ctx)
		if err != nil {
			return nil, false, err
		}
		var best *AccessList
		for i := range lists {
			if lists[i].Name == name && (best == nil || lists[i].ID < best.ID) {
				best = &lists[i]
			}
		}
		if best == nil {
			return nil, false, nil
		}
		raw, err := json.Marshal(best)
		return raw, true, err
	})
	return out, err
}

// UpdateAccessList replaces an access list's settings, users and clients.
// A user sent with an empty password keeps its current password.
func (c *Client) UpdateAccessList(ctx context.Context, id int, payload map[string]any) error {
	return c.sendJSON(ctx, http.MethodPut, fmt.Sprintf("%s/%d", accessListsPath, id), payload, nil)
}

// DeleteAccessList deletes an access list.
func (c *Client) DeleteAccessList(ctx context.Context, id int) error {
	return c.deleteObject(ctx, fmt.Sprintf("%s/%d", accessListsPath, id))
}

// SetHostEnabled enables or disables a host or stream with
// POST /{id}/enable or /{id}/disable. If a retried request finds the host
// already in that state, the first attempt succeeded.
func (c *Client) SetHostEnabled(ctx context.Context, kind HostKind, id int, enabled bool) error {
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	path := fmt.Sprintf("%s/%d/%s", kind.Path, id, verb)
	return c.retry(ctx, "POST "+path, func(attempt int) error {
		_, err := c.do(ctx, request{method: http.MethodPost, path: path})
		var ae *APIError
		if attempt > 1 && errors.As(err, &ae) && ae.Status == http.StatusBadRequest && strings.Contains(ae.Message, "already") {
			return nil
		}
		return err
	})
}
