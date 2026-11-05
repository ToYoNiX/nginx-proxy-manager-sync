package sync

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/config"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
)

// HostSpec describes how one host type is synced.
type HostSpec struct {
	Kind npm.HostKind
	// Type is the object type shown in results.
	Type string
	// Feature is the config feature that switches this type on and off.
	Feature string
	// Fields are the writable fields copied from the master.
	Fields []string
	// TLSFields are switched off when a host is synced without TLS.
	TLSFields []string
	// ToggleEnabled is set for types whose enabled flag is not part of
	// the create/update payload but changed with POST /{id}/enable and
	// /{id}/disable.
	ToggleEnabled bool
	// Streams are matched by port and protocols and have no domains, so
	// excludeDomains does not apply (spec 3.2, 4.5).
	Stream bool
}

var tlsFields = []string{"ssl_forced", "http2_support", "hsts_enabled", "hsts_subdomains"}

// ProxyHostSpec is the proxy host type (spec 4.3).
var ProxyHostSpec = HostSpec{
	Kind:    npm.ProxyHosts,
	Type:    TypeProxyHost,
	Feature: config.FeatureProxyHosts,
	Fields: []string{
		"domain_names", "forward_scheme", "forward_host", "forward_port", "certificate_id",
		"ssl_forced", "hsts_enabled", "hsts_subdomains", "trust_forwarded_proto", "http2_support",
		"block_exploits", "caching_enabled", "allow_websocket_upgrade", "access_list_id",
		"advanced_config", "enabled", "meta", "locations",
	},
	TLSFields: tlsFields,
}

// RedirectionHostSpec is the redirection host type (spec 4.5).
var RedirectionHostSpec = HostSpec{
	Kind:    npm.RedirectionHosts,
	Type:    TypeRedirectionHost,
	Feature: config.FeatureRedirectionHosts,
	Fields: []string{
		"domain_names", "forward_http_code", "forward_scheme", "forward_domain_name", "preserve_path",
		"certificate_id", "ssl_forced", "hsts_enabled", "hsts_subdomains", "http2_support",
		"block_exploits", "advanced_config", "meta",
	},
	TLSFields:     tlsFields,
	ToggleEnabled: true,
}

// DeadHostSpec is the 404 host type (spec 4.5).
var DeadHostSpec = HostSpec{
	Kind:    npm.DeadHosts,
	Type:    TypeDeadHost,
	Feature: config.FeatureDeadHosts,
	Fields: []string{
		"domain_names", "certificate_id", "ssl_forced", "hsts_enabled", "hsts_subdomains",
		"http2_support", "advanced_config", "meta",
	},
	TLSFields:     tlsFields,
	ToggleEnabled: true,
}

// StreamSpec is the stream type (spec 4.5).
var StreamSpec = HostSpec{
	Kind:    npm.Streams,
	Type:    TypeStream,
	Feature: config.FeatureStreams,
	Fields: []string{
		"incoming_port", "forwarding_host", "forwarding_port", "tcp_forwarding", "udp_forwarding",
		"certificate_id", "meta",
	},
	ToggleEnabled: true,
	Stream:        true,
}

// statusMetaKeys are meta keys NPM writes itself; they are never copied or
// compared (spec 4.3).
var statusMetaKeys = map[string]bool{"nginx_online": true, "nginx_err": true}

// locationAutoKeys are location fields NPM fills in itself.
var locationAutoKeys = map[string]bool{"id": true}

// Key returns the identity used to match a master host with a backup host:
// the domain set (spec 4.3), or for streams the incoming port with the TCP
// and UDP flags (spec 4.5).
func (spec *HostSpec) Key(h npm.Host) string {
	if spec.Stream {
		return fmt.Sprintf("%d/tcp=%v/udp=%v", npm.IntField(h.Fields, "incoming_port"),
			truthy(h.Fields["tcp_forwarding"]), truthy(h.Fields["udp_forwarding"]))
	}
	return DomainKey(h.DomainNames)
}

// Label returns a display name for a host.
func (spec *HostSpec) Label(h npm.Host) string {
	if spec.Stream {
		var protos []string
		if truthy(h.Fields["tcp_forwarding"]) {
			protos = append(protos, "tcp")
		}
		if truthy(h.Fields["udp_forwarding"]) {
			protos = append(protos, "udp")
		}
		return fmt.Sprintf("port %d (%s) -> %v:%d", npm.IntField(h.Fields, "incoming_port"), strings.Join(protos, "+"),
			h.Fields["forwarding_host"], npm.IntField(h.Fields, "forwarding_port"))
	}
	return strings.Join(h.DomainNames, ", ")
}

func (spec *HostSpec) excluded(ex Excluder, h npm.Host) (string, bool) {
	if spec.Stream {
		return "", false
	}
	return ex.Match(h.DomainNames)
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case json.Number:
		return t.String() != "0"
	case float64:
		return t != 0
	}
	return false
}

// HostOp is a planned host operation.
type HostOp int

// Host operations.
const (
	HostUnchanged HostOp = iota
	HostDelete
	HostUpdate
	HostCreate
	HostSkip     // master host that cannot be synced; a matching backup host is left alone
	HostExcluded // excluded by excludeDomains; left alone
)

// HostAction is the planned action for one host.
type HostAction struct {
	Spec     *HostSpec
	Op       HostOp
	BackupID int            // delete, update, unchanged, excluded (backup side)
	Enabled  bool           // whether the resulting host is enabled
	Domains  []string       // for collision messages
	Label    string         // display name
	Payload  map[string]any // create, update
	// PayloadChanged is false for an update that only toggles enabled.
	PayloadChanged bool
	// Toggle is set when enabled must be changed with the enable/disable
	// endpoints (ToggleEnabled types).
	Toggle bool
	// Degraded is set when the host is synced without TLS (spec 4.2.3).
	Degraded string
	// Message explains skips, exclusions and errors.
	Message string
	// Failed marks a skip that is reported as an error.
	Failed bool
	// CertificateID is the backup certificate the payload references.
	CertificateID int
}

// Name returns a display name for the host.
func (a HostAction) Name() string {
	if a.Label != "" {
		return a.Label
	}
	return strings.Join(a.Domains, ", ")
}

// HostEnv is everything host planning needs besides the hosts themselves.
type HostEnv struct {
	// CertMap maps master certificate ID to a usable backup copy ID.
	CertMap map[int]int
	// MasterCustom holds master certificates that cannot be copied.
	MasterCustom map[int]npm.Certificate
	// MasterCerts indexes every master certificate, for messages.
	MasterCerts map[int]npm.Certificate
	// CertificatesOff is set when the certificates feature is switched off
	// or certificate sync is refused for this backup.
	CertificatesOff bool
	// AccessListMap maps master access list ID to backup ID for lists
	// synced to this backup.
	AccessListMap map[int]int
	// AccessListProblem explains why a master access list is not synced,
	// by master ID; DefaultAccessListProblem is used for IDs not listed.
	AccessListProblem        map[int]string
	DefaultAccessListProblem string
	Excluder                 Excluder
}

// PlanHosts plans one host type for one backup: delete, update or create
// each host so the backup follows the master (spec 4.3, 4.5). The result is
// in no particular order of operations; the executor runs deletes, then
// updates, then creates, each across all host types.
func PlanHosts(spec *HostSpec, master, backup []npm.Host, env HostEnv) []HostAction {
	var out []HostAction
	act := func(h npm.Host, a HostAction) HostAction {
		a.Spec, a.Domains, a.Label = spec, h.DomainNames, spec.Label(h)
		return a
	}

	// Backup hosts by key. If several share a key, the lowest ID is the
	// match and the others are unmatched.
	sortedBackup := append([]npm.Host(nil), backup...)
	sort.Slice(sortedBackup, func(i, j int) bool { return sortedBackup[i].ID < sortedBackup[j].ID })
	byKey := map[string]npm.Host{}
	excludedBackup := map[int]bool{}
	excludedKeys := map[string]bool{}
	for _, b := range sortedBackup {
		if p, ok := spec.excluded(env.Excluder, b); ok {
			excludedBackup[b.ID] = true
			excludedKeys[spec.Key(b)] = true
			out = append(out, act(b, HostAction{Op: HostExcluded, BackupID: b.ID, Enabled: b.Enabled(),
				Message: fmt.Sprintf("backup %s #%d excluded by pattern %q", spec.Kind.Name, b.ID, p)}))
			continue
		}
		k := spec.Key(b)
		if _, dup := byKey[k]; dup {
			continue // unmatched duplicate: deleted below
		}
		byKey[k] = b
	}

	// Master hosts that share a key (possible for streams) are not synced.
	sortedMaster := append([]npm.Host(nil), master...)
	sort.Slice(sortedMaster, func(i, j int) bool { return sortedMaster[i].ID < sortedMaster[j].ID })
	masterCount := map[string]int{}
	for _, m := range sortedMaster {
		masterCount[spec.Key(m)]++
	}

	matched := map[int]bool{}
	for _, m := range sortedMaster {
		k := spec.Key(m)
		b, hasMatch := byKey[k]

		if p, ok := spec.excluded(env.Excluder, m); ok {
			// A backup host with the same domains is excluded too and
			// was reported above.
			if !excludedKeys[k] {
				out = append(out, act(m, HostAction{Op: HostExcluded, Enabled: m.Enabled(),
					Message: fmt.Sprintf("master %s #%d excluded by pattern %q", spec.Kind.Name, m.ID, p)}))
			}
			continue
		}
		if hasMatch {
			matched[b.ID] = true
		}

		skip := ""
		if n := masterCount[k]; n > 1 {
			skip = fmt.Sprintf("%d %ss on the master share this %s; none of them is synced", n, spec.Kind.Name, keyName(spec))
		}
		payload, degraded, alSkip := desiredPayload(spec, m, env)
		if skip == "" {
			skip = alSkip
		}
		if skip != "" {
			a := act(m, HostAction{Op: HostSkip, Message: skip, Failed: true})
			if hasMatch {
				a.BackupID = b.ID
				a.Message += "; the matching backup " + spec.Kind.Name + " is left as it is"
			}
			out = append(out, a)
			continue
		}
		a := act(m, HostAction{Payload: payload, Degraded: degraded,
			Enabled: m.Enabled(), CertificateID: npm.IntField(payload, "certificate_id")})
		switch {
		case !hasMatch:
			a.Op, a.PayloadChanged = HostCreate, true
			// New hosts are created enabled; disable afterwards if needed.
			a.Toggle = spec.ToggleEnabled && !a.Enabled
		default:
			a.BackupID = b.ID
			a.PayloadChanged = !Equal(spec, payload, currentPayload(spec, b))
			a.Toggle = spec.ToggleEnabled && a.Enabled != b.Enabled()
			if a.PayloadChanged || a.Toggle {
				a.Op = HostUpdate
			} else {
				a.Op = HostUnchanged
			}
		}
		out = append(out, a)
	}

	for _, b := range sortedBackup {
		if excludedBackup[b.ID] || matched[b.ID] {
			continue
		}
		out = append(out, act(b, HostAction{Op: HostDelete, BackupID: b.ID, Enabled: b.Enabled(),
			Message: "not on master"}))
	}
	return out
}

func keyName(spec *HostSpec) string {
	if spec.Stream {
		return "incoming port and protocols"
	}
	return "domain set"
}

// desiredPayload builds the backup payload for a master host, rewriting
// certificate and access list IDs. It returns a non-empty skip reason if the
// host must not be synced (fail closed on access lists, spec 4.3 / 4.4).
func desiredPayload(spec *HostSpec, m npm.Host, env HostEnv) (payload map[string]any, degraded, skip string) {
	payload = pick(spec, m.Fields)

	if _, ok := payload["access_list_id"]; ok {
		id := npm.IntField(payload, "access_list_id")
		if id > 0 {
			bid, ok := env.AccessListMap[id]
			if !ok {
				return nil, "", "uses access list #" + fmt.Sprint(id) + " which is not synced to this backup: " + env.accessListProblem(id)
			}
			payload["access_list_id"] = bid
		}
	}
	if locs, ok := payload["locations"].([]any); ok {
		for _, l := range locs {
			loc, ok := l.(map[string]any)
			if !ok {
				continue
			}
			id := npm.IntField(loc, "access_list_id")
			if id <= 0 {
				continue
			}
			bid, ok := env.AccessListMap[id]
			if !ok {
				return nil, "", fmt.Sprintf("location %v uses access list #%d which is not synced to this backup: %s", loc["path"], id, env.accessListProblem(id))
			}
			loc["access_list_id"] = bid
		}
	}

	if _, ok := payload["certificate_id"]; ok {
		id := npm.IntField(payload, "certificate_id")
		if id > 0 {
			if bid, ok := env.CertMap[id]; ok {
				payload["certificate_id"] = bid
			} else {
				degraded = env.certProblem(id)
				payload["certificate_id"] = 0
				for _, f := range spec.TLSFields {
					if _, ok := payload[f]; ok {
						payload[f] = false
					}
				}
			}
		}
	}
	return payload, degraded, ""
}

func (env HostEnv) accessListProblem(id int) string {
	if p, ok := env.AccessListProblem[id]; ok {
		return p
	}
	if env.DefaultAccessListProblem != "" {
		return env.DefaultAccessListProblem
	}
	return "access list not synced"
}

func (env HostEnv) certProblem(id int) string {
	name := fmt.Sprintf("#%d", id)
	if c, ok := env.MasterCerts[id]; ok {
		name = fmt.Sprintf("#%d (%s)", id, c.NiceName)
	}
	switch {
	case env.MasterCustom[id].ID != 0:
		return "synced without TLS: certificate " + name + " is a custom certificate and NPM did not return its private key, so it cannot be copied"
	case env.CertificatesOff:
		return "synced without TLS: certificate sync is off and there is no usable copy of certificate " + name
	default:
		return "synced without TLS: no usable copy of certificate " + name + " on this backup"
	}
}

// pick copies the spec's writable fields from a host, dropping NPM status
// meta keys and NPM-filled location fields. The result is a deep copy.
func pick(spec *HostSpec, fields map[string]any) map[string]any {
	out := make(map[string]any, len(spec.Fields))
	for _, f := range spec.Fields {
		v, ok := fields[f]
		if !ok {
			continue
		}
		v = deepCopy(v)
		switch f {
		case "meta":
			if m, ok := v.(map[string]any); ok {
				for k := range statusMetaKeys {
					delete(m, k)
				}
			} else {
				v = map[string]any{}
			}
		case "locations":
			if list, ok := v.([]any); ok {
				for _, l := range list {
					if loc, ok := l.(map[string]any); ok {
						for k := range locationAutoKeys {
							delete(loc, k)
						}
					}
				}
			} else if v == nil {
				v = []any{}
			}
		}
		out[f] = v
	}
	return out
}

// currentPayload is a backup host's writable fields, normalised the same way.
func currentPayload(spec *HostSpec, h npm.Host) map[string]any {
	return pick(spec, h.Fields)
}

// Equal compares two payloads after normalisation: domain names sorted and
// lower-cased, meta filtered, NPM-filled location fields removed (spec 4.3).
func Equal(spec *HostSpec, a, b map[string]any) bool {
	return canonical(spec, a) == canonical(spec, b)
}

func canonical(spec *HostSpec, p map[string]any) string {
	c := pick(spec, p)
	if d, ok := c["domain_names"]; ok {
		c["domain_names"] = DomainKey(toStrings(d))
	}
	for k, v := range c {
		c[k] = normaliseScalars(v)
	}
	data, _ := json.Marshal(c) // map keys are sorted by encoding/json
	return string(data)
}

// normaliseScalars turns numbers into a canonical form so that json.Number
// and int compare equal.
func normaliseScalars(v any) any {
	switch t := v.(type) {
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case map[string]any:
		for k, x := range t {
			t[k] = normaliseScalars(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = normaliseScalars(x)
		}
		return t
	default:
		return v
	}
}

func toStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = deepCopy(x)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = deepCopy(x)
		}
		return s
	case []string:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = x
		}
		return s
	default:
		return v
	}
}
