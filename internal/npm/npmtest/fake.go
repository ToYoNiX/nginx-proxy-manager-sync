// Package npmtest provides an in-memory fake of the NPM admin API for tests.
// It reproduces the responses and errors npm-sync relies on, as observed on
// NPM v2.16.0.
package npmtest

import (
	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TimeLayout is how NPM (SQLite) formats timestamps.
const TimeLayout = "2006-01-02 15:04:05"

// Cert is a certificate held by the fake.
type Cert struct {
	ID          int
	Provider    string
	NiceName    string
	DomainNames []string
	ExpiresOn   time.Time
	// Files of a Let's Encrypt certificate (served by download) or of an
	// uploaded custom certificate.
	CertPEM, ChainPEM, KeyPEM []byte
	Meta                      map[string]any
}

// Fake is an in-memory NPM instance.
type Fake struct {
	*httptest.Server

	mu      sync.Mutex
	Version [3]int
	// TwoFactor makes login answer with a 2FA challenge.
	TwoFactor bool
	// HideKeys omits meta.certificate_key from certificate responses, like
	// NPM releases after v2.16.0 (PR #5891).
	HideKeys bool
	Certs    map[int]*Cert
	// Hosts by API path ("/api/nginx/proxy-hosts", ...), by ID.
	Hosts map[string]map[int]map[string]any
	// AccessLists by ID.
	AccessLists map[int]*AccessList
	nextID      int
	tokens      map[string]bool
	// Requests logs "METHOD /path" of every request, in order.
	Requests []string
	// Fault, if set, is called before each request is handled. If it
	// returns a status, that status is answered instead. If it returns
	// -1, the connection is closed without an answer. If after is true,
	// the request is handled first and the fault replaces its answer.
	Fault  func(method, path string, n int) (status int, after bool)
	counts map[string]int
}

// HostPaths are the host collections the fake serves.
var HostPaths = []string{"/api/nginx/proxy-hosts", "/api/nginx/redirection-hosts", "/api/nginx/dead-hosts", "/api/nginx/streams"}

// New starts a fake NPM v2.16.0.
func New() *Fake {
	f := &Fake{
		Version:     [3]int{2, 16, 0},
		Certs:       map[int]*Cert{},
		Hosts:       map[string]map[int]map[string]any{},
		AccessLists: map[int]*AccessList{},
		tokens:      map[string]bool{},
		counts:      map[string]int{},
		nextID:      1,
	}
	for _, p := range HostPaths {
		f.Hosts[p] = map[int]map[string]any{}
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Fake) id() int {
	f.nextID++
	return f.nextID
}

// AddLetsEncrypt adds a Let's Encrypt certificate with real files that
// expire at notAfter, and returns its ID.
func (f *Fake) AddLetsEncrypt(domains []string, notAfter time.Time) int {
	certPEM, chainPEM, keyPEM := MakeCert(domains, notAfter)
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.id()
	f.Certs[id] = &Cert{ID: id, Provider: "letsencrypt", NiceName: strings.Join(domains, ", "), DomainNames: domains,
		ExpiresOn: notAfter.UTC().Truncate(time.Second), CertPEM: certPEM, ChainPEM: chainPEM, KeyPEM: keyPEM,
		Meta: map[string]any{"letsencrypt_agree": true}}
	return id
}

// Renew replaces a Let's Encrypt certificate's files with ones that expire
// at notAfter.
func (f *Fake) Renew(id int, notAfter time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.Certs[id]
	c.CertPEM, c.ChainPEM, c.KeyPEM = MakeCert(c.DomainNames, notAfter)
	c.ExpiresOn = notAfter.UTC().Truncate(time.Second)
}

// AddCustom adds a custom certificate with uploaded files, as a user
// would through NPM's UI, and returns its ID.
func (f *Fake) AddCustom(name string, notAfter time.Time) int {
	f.mu.Lock()
	id := f.id()
	f.Certs[id] = &Cert{ID: id, Provider: "other", NiceName: name, Meta: map[string]any{}}
	f.mu.Unlock()
	f.Reupload(id, notAfter)
	return id
}

// Reupload replaces a custom certificate's files with a new certificate
// and key that expire at notAfter.
func (f *Fake) Reupload(id int, notAfter time.Time) {
	certPEM, chainPEM, keyPEM := MakeCert([]string{"custom.example.com"}, notAfter)
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.Certs[id]
	c.CertPEM, c.ChainPEM, c.KeyPEM = certPEM, chainPEM, keyPEM
	c.ExpiresOn = notAfter.UTC().Truncate(time.Second)
	c.DomainNames = []string{"custom.example.com"}
	c.Meta = map[string]any{"certificate": string(certPEM), "certificate_key": string(keyPEM), "intermediate_certificate": string(chainPEM)}
}

// AddRaw adds a certificate as-is (for example a managed copy whose upload
// failed) and returns its ID.
func (f *Fake) AddRaw(c Cert) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == 0 {
		c.ID = f.id()
	} else if c.ID >= f.nextID {
		f.nextID = c.ID
	}
	if c.Meta == nil {
		c.Meta = map[string]any{}
	}
	f.Certs[c.ID] = &c
	return c.ID
}

// AddHost adds a host at path with the given fields, filling in NPM's
// defaults, and returns its ID.
func (f *Fake) AddHost(path string, fields map[string]any) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addHost(path, fields)
}

// hostDefaults are the fields NPM fills in for a new host of each type.
var hostDefaults = map[string]map[string]any{
	"/api/nginx/proxy-hosts": {
		"domain_names": []any{}, "forward_scheme": "http", "forward_host": "", "forward_port": 80,
		"access_list_id": 0, "certificate_id": 0, "ssl_forced": false, "caching_enabled": false,
		"block_exploits": false, "advanced_config": "", "allow_websocket_upgrade": false, "http2_support": false,
		"locations": []any{}, "hsts_enabled": false, "hsts_subdomains": false, "trust_forwarded_proto": false,
	},
	"/api/nginx/redirection-hosts": {
		"domain_names": []any{}, "forward_http_code": 301, "forward_scheme": "auto", "forward_domain_name": "",
		"preserve_path": true, "certificate_id": 0, "ssl_forced": false, "hsts_enabled": false, "hsts_subdomains": false,
		"http2_support": false, "block_exploits": false, "advanced_config": "",
	},
	"/api/nginx/dead-hosts": {
		"domain_names": []any{}, "certificate_id": 0, "ssl_forced": false, "hsts_enabled": false,
		"hsts_subdomains": false, "http2_support": false, "advanced_config": "",
	},
	"/api/nginx/streams": {
		"incoming_port": 0, "forwarding_host": "", "forwarding_port": 0, "tcp_forwarding": true,
		"udp_forwarding": false, "certificate_id": 0,
	},
}

// writable are the fields each create/update accepts (NPM 2.16 schemas,
// additionalProperties: false).
var writable = map[string]map[string]bool{}

func init() {
	for path, d := range hostDefaults {
		writable[path] = map[string]bool{"meta": true}
		for k := range d {
			writable[path][k] = true
		}
	}
	writable["/api/nginx/proxy-hosts"]["enabled"] = true
	writable["/api/nginx/streams"]["domain_names"] = true // create only, not stored
}

func (f *Fake) addHost(path string, fields map[string]any) int {
	id := f.id()
	h := map[string]any{
		"id": id, "created_on": "2026-01-01 00:00:00", "modified_on": "2026-01-01 00:00:00", "owner_user_id": 1,
		"meta": map[string]any{"nginx_online": true, "nginx_err": nil}, "enabled": true,
	}
	maps.Copy(h, hostDefaults[path])
	h = normalise(h)
	for k, v := range normalise(fields) {
		if k == "meta" {
			m := map[string]any{"nginx_online": true, "nginx_err": nil}
			maps.Copy(m, v.(map[string]any))
			v = m
		}
		h[k] = v
	}
	if path == "/api/nginx/streams" {
		delete(h, "domain_names")
	}
	h["id"] = float64(id)
	f.Hosts[path][id] = h
	return id
}

// checkFields rejects fields the NPM schema does not allow.
func checkFields(path string, body map[string]any) string {
	for k := range body {
		if !writable[path][k] {
			return fmt.Sprintf("data must NOT have additional properties (%s)", k)
		}
	}
	return ""
}

// HostList returns the hosts at path sorted by ID.
func (f *Fake) HostList(path string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hostList(path)
}

func (f *Fake) hostList(path string) []map[string]any {
	ids := make([]int, 0, len(f.Hosts[path]))
	for id := range f.Hosts[path] {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.Hosts[path][id])
	}
	return out
}

// CertList returns the certificates sorted by ID.
func (f *Fake) CertList() []*Cert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*Cert, 0, len(f.Certs))
	for _, c := range f.Certs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Writes returns the logged requests that are not GETs or logins.
func (f *Fake) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.Requests {
		if !strings.HasPrefix(r, "GET ") && r != "POST /api/tokens" {
			out = append(out, r)
		}
	}
	return out
}

// ResetLog clears the request log.
func (f *Fake) ResetLog() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = nil
	f.counts = map[string]int{}
}

func normalise(v map[string]any) map[string]any {
	data, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": msg}})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	key := r.Method + " " + r.URL.Path
	f.Requests = append(f.Requests, key)
	f.counts[key]++
	n := f.counts[key]
	fault := f.Fault
	f.mu.Unlock()

	if fault != nil {
		if status, after := fault(r.Method, r.URL.Path, n); status != 0 {
			if after {
				rec := httptest.NewRecorder()
				f.handle(rec, r)
			}
			if status < 0 {
				hj, ok := w.(http.Hijacker)
				if ok {
					conn, _, _ := hj.Hijack()
					conn.Close()
					return
				}
			}
			writeErr(w, status, http.StatusText(status))
			return
		}
	}
	f.handle(w, r)
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/api/" && r.Method == http.MethodGet:
		f.mu.Lock()
		v := f.Version
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"status": "OK", "setup": true, "version": map[string]int{"major": v[0], "minor": v[1], "revision": v[2]}})
		return
	case path == "/api/tokens" && r.Method == http.MethodPost:
		f.login(w, r)
		return
	}

	if !f.authorised(r) {
		writeErr(w, http.StatusUnauthorized, "Token has expired")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if path == accessListsPath || strings.HasPrefix(path, accessListsPath+"/") {
		f.accessLists(w, r, strings.TrimPrefix(path, accessListsPath))
		return
	}
	if after, ok := strings.CutPrefix(path, "/api/nginx/certificates"); ok {
		f.certificates(w, r, after)
		return
	}
	for _, hp := range HostPaths {
		if path == hp || strings.HasPrefix(path, hp+"/") {
			f.hosts(w, r, hp, strings.TrimPrefix(path, hp))
			return
		}
	}
	writeErr(w, 404, "Not Found")
}

func (f *Fake) login(w http.ResponseWriter, r *http.Request) {
	var body struct{ Identity, Secret string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Identity != "admin@example.com" || body.Secret != "changeme" {
		writeErr(w, 401, "Invalid email or password")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.TwoFactor {
		writeJSON(w, 200, map[string]any{"requires_2fa": true, "challenge_token": "challenge"})
		return
	}
	tok := fmt.Sprintf("tok-%d", len(f.tokens)+1)
	f.tokens[tok] = true
	writeJSON(w, 200, map[string]any{"token": tok, "expires": "2099-01-01T00:00:00.000Z"})
}

// ExpireTokens invalidates every issued token, so the next request gets 401.
func (f *Fake) ExpireTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = map[string]bool{}
}

func (f *Fake) authorised(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[tok]
}

func (f *Fake) certJSON(c *Cert) map[string]any {
	meta := map[string]any{}
	for k, v := range c.Meta {
		if k == "certificate_key" && f.HideKeys {
			continue
		}
		meta[k] = v
	}
	return map[string]any{
		"id": c.ID, "created_on": "2026-01-01 00:00:00", "modified_on": "2026-01-01 00:00:00", "owner_user_id": 1,
		"provider": c.Provider, "nice_name": c.NiceName, "domain_names": orEmpty(c.DomainNames),
		"expires_on": c.ExpiresOn.UTC().Format(TimeLayout), "meta": meta,
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (f *Fake) certificates(w http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case rest == "" && r.Method == http.MethodGet:
		ids := make([]int, 0, len(f.Certs))
		for id := range f.Certs {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		out := make([]any, 0, len(ids))
		for _, id := range ids {
			out = append(out, f.certJSON(f.Certs[id]))
		}
		writeJSON(w, 200, out)
	case rest == "" && r.Method == http.MethodPost:
		var body struct {
			Provider string `json:"provider"`
			NiceName string `json:"nice_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := f.id()
		// NPM sets expires_on to the creation time for a new custom cert.
		c := &Cert{ID: id, Provider: body.Provider, NiceName: body.NiceName, ExpiresOn: time.Now().UTC().Truncate(time.Second), Meta: map[string]any{}}
		f.Certs[id] = c
		writeJSON(w, 201, f.certJSON(c))
	default:
		parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
		id, err := strconv.Atoi(parts[0])
		c, ok := f.Certs[id]
		if err != nil || !ok {
			writeErr(w, 404, fmt.Sprintf("Not Found - %s", parts[0]))
			return
		}
		switch {
		case len(parts) == 1 && r.Method == http.MethodDelete:
			delete(f.Certs, id)
			writeJSON(w, 200, true)
		case len(parts) == 2 && parts[1] == "download" && r.Method == http.MethodGet:
			if c.Provider != "letsencrypt" {
				writeErr(w, 400, "Only Let'sEncrypt certificates can be downloaded")
				return
			}
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			for name, data := range map[string][]byte{"cert3.pem": c.CertPEM, "chain3.pem": c.ChainPEM, "fullchain3.pem": append(append([]byte{}, c.CertPEM...), c.ChainPEM...), "privkey3.pem": c.KeyPEM} {
				fw, _ := zw.Create(name)
				_, _ = fw.Write(data)
			}
			_ = zw.Close()
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(buf.Bytes())
		case len(parts) == 2 && parts[1] == "upload" && r.Method == http.MethodPost:
			f.upload(w, r, c)
		default:
			writeErr(w, 404, "Not Found")
		}
	}
}

func (f *Fake) upload(w http.ResponseWriter, r *http.Request, c *Cert) {
	if c.Provider != "other" {
		writeErr(w, 400, "Cannot upload certificates for this type of provider")
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeErr(w, 400, "No files were uploaded")
		return
	}
	read := func(field string) []byte {
		fh, _, err := r.FormFile(field)
		if err != nil {
			return nil
		}
		defer fh.Close()
		b, _ := io.ReadAll(fh)
		return b
	}
	certPEM, keyPEM, chainPEM := read("certificate"), read("certificate_key"), read("intermediate_certificate")
	block, _ := pem.Decode(certPEM)
	if block == nil {
		writeErr(w, 400, "Certificate file was not provided")
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		writeErr(w, 400, "Certificate is not valid")
		return
	}
	if time.Now().After(cert.NotAfter) {
		writeErr(w, 400, "Certificate has expired")
		return
	}
	c.CertPEM, c.KeyPEM, c.ChainPEM = certPEM, keyPEM, chainPEM
	c.ExpiresOn = cert.NotAfter.UTC()
	c.DomainNames = []string{cert.Subject.CommonName}
	// NPM 2.16 returns the full file contents in the certificate list.
	c.Meta = map[string]any{"certificate": string(certPEM), "certificate_key": string(keyPEM)}
	if chainPEM != nil {
		c.Meta["intermediate_certificate"] = string(chainPEM)
	}
	writeJSON(w, 200, c.Meta)
}

func (f *Fake) hosts(w http.ResponseWriter, r *http.Request, hp, rest string) {
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, f.hostList(hp))
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if msg := checkFields(hp, body); msg != "" {
				writeErr(w, 400, msg)
				return
			}
			if d := f.inUse(body["domain_names"], 0); hp != "/api/nginx/streams" && d != "" {
				writeErr(w, 400, d+" is already in use")
				return
			}
			id := f.addHost(hp, body)
			writeJSON(w, 201, f.Hosts[hp][id])
		default:
			writeErr(w, 405, "Method Not Allowed")
		}
		return
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	id, err := strconv.Atoi(parts[0])
	h, ok := f.Hosts[hp][id]
	if err != nil || !ok {
		writeErr(w, 404, fmt.Sprintf("Not Found - %s", rest))
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost && (parts[1] == "enable" || parts[1] == "disable") {
		want := parts[1] == "enable"
		if h["enabled"] == want {
			writeErr(w, 400, "Host is already "+parts[1]+"d")
			return
		}
		h["enabled"] = want
		writeJSON(w, 200, true)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if msg := checkFields(hp, body); msg != "" || (hp != "/api/nginx/proxy-hosts" && body["enabled"] != nil) {
			writeErr(w, 400, "data must NOT have additional properties")
			return
		}
		if d := f.inUse(body["domain_names"], id); hp != "/api/nginx/streams" && d != "" {
			writeErr(w, 400, d+" is already in use")
			return
		}
		for k, v := range body {
			if k == "meta" {
				m := map[string]any{"nginx_online": true, "nginx_err": nil}
				maps.Copy(m, v.(map[string]any))
				v = m
			}
			h[k] = v
		}
		writeJSON(w, 200, h)
	case http.MethodDelete:
		delete(f.Hosts[hp], id)
		writeJSON(w, 200, true)
	default:
		writeErr(w, 405, "Method Not Allowed")
	}
}

// inUse reports a domain already used by another proxy, redirection or dead
// host, like NPM's check.
func (f *Fake) inUse(domains any, exceptID int) string {
	list, _ := domains.([]any)
	for _, hp := range HostPaths[:3] {
		for id, h := range f.Hosts[hp] {
			if id == exceptID {
				continue
			}
			for _, d := range h["domain_names"].([]any) {
				for _, want := range list {
					if strings.EqualFold(d.(string), want.(string)) {
						return want.(string)
					}
				}
			}
		}
	}
	return ""
}

// MakeCert returns a leaf certificate for domains signed by a throwaway CA,
// the CA certificate (chain) and the leaf's private key, all PEM encoded.
func MakeCert(domains []string, notAfter time.Time) (certPEM, chainPEM, keyPEM []byte) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "npm-sync test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

const accessListsPath = "/api/nginx/access-lists"

// AccessList is an access list held by the fake. Passwords are stored in
// plain text, as NPM does, so hints can be computed.
type AccessList struct {
	ID         int
	Name       string
	SatisfyAny bool
	PassAuth   bool
	Users      [][2]string // username, password
	Clients    [][2]string // address, directive
}

// AddAccessList adds an access list and returns its ID.
func (f *Fake) AddAccessList(l AccessList) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	l.ID = f.id()
	f.AccessLists[l.ID] = &l
	return l.ID
}

// AccessList returns a copy of the access list with the given ID.
func (f *Fake) AccessList(id int) (AccessList, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.AccessLists[id]
	if !ok {
		return AccessList{}, false
	}
	return *l, true
}

// hint reproduces NPM's maskItems.
func hint(pw string) string {
	if pw == "" {
		return "*********"
	}
	r := []rune(pw)
	return string(r[0]) + strings.Repeat("*", len(r)-1)
}

func (f *Fake) accessListJSON(l *AccessList) map[string]any {
	items := []any{}
	for _, u := range l.Users {
		items = append(items, map[string]any{"username": u[0], "password": "", "hint": hint(u[1])})
	}
	clients := []any{}
	for _, c := range l.Clients {
		clients = append(clients, map[string]any{"address": c[0], "directive": c[1]})
	}
	return map[string]any{"id": l.ID, "name": l.Name, "satisfy_any": l.SatisfyAny, "pass_auth": l.PassAuth,
		"items": items, "clients": clients, "proxy_host_count": 0}
}

type alBody struct {
	Name       *string `json:"name"`
	SatisfyAny bool    `json:"satisfy_any"`
	PassAuth   bool    `json:"pass_auth"`
	Items      []struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"items"`
	Clients []struct {
		Address   string `json:"address"`
		Directive string `json:"directive"`
	} `json:"clients"`
}

func (f *Fake) accessLists(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			ids := make([]int, 0, len(f.AccessLists))
			for id := range f.AccessLists {
				ids = append(ids, id)
			}
			sort.Ints(ids)
			out := []any{}
			for _, id := range ids {
				out = append(out, f.accessListJSON(f.AccessLists[id]))
			}
			writeJSON(w, 200, out)
		case http.MethodPost:
			var b alBody
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b.Name == nil {
				writeErr(w, 400, "data must have required property 'name'")
				return
			}
			l := &AccessList{ID: f.id(), Name: *b.Name, SatisfyAny: b.SatisfyAny, PassAuth: b.PassAuth}
			for _, it := range b.Items {
				l.Users = append(l.Users, [2]string{it.Username, it.Password})
			}
			for _, c := range b.Clients {
				l.Clients = append(l.Clients, [2]string{c.Address, c.Directive})
			}
			f.AccessLists[l.ID] = l
			writeJSON(w, 201, f.accessListJSON(l))
		}
		return
	}
	id, err := strconv.Atoi(strings.TrimPrefix(rest, "/"))
	l, ok := f.AccessLists[id]
	if err != nil || !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var b alBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Name != nil && *b.Name != "" {
			l.Name, l.SatisfyAny, l.PassAuth = *b.Name, b.SatisfyAny, b.PassAuth
		}
		if b.Items != nil {
			// Users sent with an empty password keep their password;
			// users not sent are removed.
			old := map[string]string{}
			for _, u := range l.Users {
				old[u[0]] = u[1]
			}
			l.Users = nil
			for _, it := range b.Items {
				pw := it.Password
				if pw == "" {
					p, existed := old[it.Username]
					if !existed {
						continue // NPM keeps nothing: the user is not created
					}
					pw = p
				}
				l.Users = append(l.Users, [2]string{it.Username, pw})
			}
		}
		if b.Clients != nil {
			l.Clients = nil
			for _, c := range b.Clients {
				l.Clients = append(l.Clients, [2]string{c.Address, c.Directive})
			}
		}
		writeJSON(w, 200, f.accessListJSON(l))
	case http.MethodDelete:
		delete(f.AccessLists, id)
		// Like NPM: hosts that used the list silently lose it.
		for _, h := range f.Hosts["/api/nginx/proxy-hosts"] {
			if v, ok := h["access_list_id"].(float64); ok && int(v) == id {
				h["access_list_id"] = float64(0)
			}
		}
		writeJSON(w, 200, true)
	}
}
