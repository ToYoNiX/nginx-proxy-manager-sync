package npm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Version is an NPM release number, as returned by GET /api.
type Version struct {
	Major    int `json:"major"`
	Minor    int `json:"minor"`
	Revision int `json:"revision"`
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Revision) }

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Revision < o.Revision
}

// MinCertificateVersion is the first NPM release with the certificate
// download endpoint (spec 4.9).
var MinCertificateVersion = Version{Major: 2, Minor: 9, Revision: 9}

// Certificate providers.
const (
	ProviderLetsEncrypt = "letsencrypt"
	ProviderOther       = "other"
)

// Certificate is an NPM certificate as returned by the API.
type Certificate struct {
	ID          int            `json:"id"`
	Provider    string         `json:"provider"`
	NiceName    string         `json:"nice_name"`
	DomainNames []string       `json:"domain_names"`
	ExpiresOn   string         `json:"expires_on"`
	ModifiedOn  string         `json:"modified_on"`
	Meta        map[string]any `json:"meta"`

	// Files holds a custom certificate's files when NPM returned them in
	// the listing (NPM up to at least v2.16.0 does; a later release stops
	// returning the private key). Never serialised or logged; call
	// Files.Wipe when done.
	Files *CertFiles `json:"-"`
	// CertHash is a SHA-256 over the public files (certificate and chain)
	// of a custom certificate, to detect changes. A new key always comes
	// with a new certificate. NPM keeps returning the public files after
	// it stops returning the key. Empty if NPM returned no files.
	CertHash string `json:"-"`
}

// Expires parses ExpiresOn. NPM returns either an ISO 8601 timestamp or
// "YYYY-MM-DD HH:MM:SS" depending on the database; the latter is read as UTC.
func (c Certificate) Expires() (time.Time, error) {
	return ParseTime(c.ExpiresOn)
}

// HasFiles reports whether files were uploaded to a custom certificate:
// the certificate is present, and the key is present or not reported.
// (NPM releases after v2.16.0 stop returning certificate_key; a copy whose
// upload failed has neither.)
func (c Certificate) HasFiles() bool {
	if !truthy(c.Meta["certificate"]) {
		return false
	}
	key, reported := c.Meta["certificate_key"]
	return !reported || truthy(key)
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != ""
	default:
		return false
	}
}

// ParseTime parses the timestamp formats NPM uses.
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", s)
}

// HostKind identifies one of NPM's host types and its API path.
type HostKind struct {
	Name string // e.g. "proxy host"
	Path string // e.g. "/api/nginx/proxy-hosts"
}

// Host kinds.
var (
	ProxyHosts       = HostKind{Name: "proxy host", Path: "/api/nginx/proxy-hosts"}
	RedirectionHosts = HostKind{Name: "redirection host", Path: "/api/nginx/redirection-hosts"}
	DeadHosts        = HostKind{Name: "404 host", Path: "/api/nginx/dead-hosts"}
	Streams          = HostKind{Name: "stream", Path: "/api/nginx/streams"}
)

// Host is a host of any kind as returned by the API. Fields holds every
// field of the JSON object; numbers are json.Number.
type Host struct {
	ID          int
	DomainNames []string
	Fields      map[string]any
}

// UnmarshalJSON decodes a host, keeping every field.
func (h *Host) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return err
	}
	h.Fields = fields
	h.ID = IntField(fields, "id")
	h.DomainNames = nil
	if list, ok := fields["domain_names"].([]any); ok {
		for _, d := range list {
			if s, ok := d.(string); ok {
				h.DomainNames = append(h.DomainNames, s)
			}
		}
	}
	return nil
}

// Enabled reports the host's enabled flag (true if absent).
func (h Host) Enabled() bool {
	v, ok := h.Fields["enabled"]
	if !ok {
		return true
	}
	switch t := v.(type) {
	case bool:
		return t
	case json.Number:
		return t.String() != "0"
	default:
		return true
	}
}

// IntField reads an integer field that may be a json.Number, float or bool.
func IntField(m map[string]any, key string) int {
	switch t := m[key].(type) {
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			f, _ := t.Float64()
			return int(f)
		}
		return int(n)
	case float64:
		return int(t)
	case int:
		return t
	default:
		return 0
	}
}

// CertFiles holds certificate files in memory. Call Wipe when done so the
// private key does not linger longer than needed (spec section 9).
type CertFiles struct {
	Certificate []byte
	Chain       []byte
	PrivateKey  []byte
}

// Wipe zeroes the file contents.
func (f *CertFiles) Wipe() {
	for _, b := range [][]byte{f.Certificate, f.Chain, f.PrivateKey} {
		for i := range b {
			b[i] = 0
		}
	}
	f.Certificate, f.Chain, f.PrivateKey = nil, nil, nil
}

// Bool decodes JSON booleans that NPM may also send as 0/1.
type Bool bool

// UnmarshalJSON accepts true/false, 0/1 and "0"/"1".
func (b *Bool) UnmarshalJSON(data []byte) error {
	switch strings.Trim(string(data), `"`) {
	case "true", "1":
		*b = true
	default:
		*b = false
	}
	return nil
}

// AccessList is an NPM access list as returned by
// GET /api/nginx/access-lists?expand=items,clients.
type AccessList struct {
	ID         int                `json:"id"`
	Name       string             `json:"name"`
	SatisfyAny Bool               `json:"satisfy_any"`
	PassAuth   Bool               `json:"pass_auth"`
	Items      []AccessListItem   `json:"items"`
	Clients    []AccessListClient `json:"clients"`
}

// AccessListItem is a basic-auth user. NPM never returns the password; Hint
// is its first character followed by one * per remaining character.
type AccessListItem struct {
	Username string `json:"username"`
	Hint     string `json:"hint"`
}

// AccessListClient is an IP allow/deny rule.
type AccessListClient struct {
	Address   string `json:"address"`
	Directive string `json:"directive"`
}
