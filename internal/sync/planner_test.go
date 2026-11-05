package sync

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
)

var now = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func ts(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

func host(t *testing.T, id int, fields string) npm.Host {
	t.Helper()
	var h npm.Host
	raw := `{"id":` + itoa(id) + `,"forward_scheme":"http","forward_host":"10.0.0.1","forward_port":80,"certificate_id":0,
		"ssl_forced":false,"hsts_enabled":false,"hsts_subdomains":false,"trust_forwarded_proto":false,"http2_support":false,
		"block_exploits":false,"caching_enabled":false,"allow_websocket_upgrade":false,"access_list_id":0,"advanced_config":"",
		"enabled":true,"meta":{"nginx_online":true,"nginx_err":null},"locations":[],` + fields + `}`
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		t.Fatalf("bad fixture %s: %v", raw, err)
	}
	return h
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func ops(actions []HostAction) map[string]HostOp {
	out := map[string]HostOp{}
	for _, a := range actions {
		key := a.Name()
		if _, dup := out[key]; dup {
			key += "#" + itoa(a.BackupID)
		}
		out[key] = a.Op
	}
	return out
}

func find(t *testing.T, actions []HostAction, name string, op HostOp) HostAction {
	t.Helper()
	for _, a := range actions {
		if a.Name() == name && a.Op == op {
			return a
		}
	}
	t.Fatalf("no %v action for %q in %+v", op, name, ops(actions))
	return HostAction{}
}

// ---- matching ----

func TestDomainKey(t *testing.T) {
	if DomainKey([]string{"B.example.com", "a.example.com", "a.example.com"}) != DomainKey([]string{"a.example.com", "b.EXAMPLE.com"}) {
		t.Fatal("domain sets should match case-insensitively, ignoring order and duplicates")
	}
	if DomainKey([]string{"a.example.com"}) == DomainKey([]string{"a.example.com", "b.example.com"}) {
		t.Fatal("different sets matched")
	}
}

func TestExcludePatterns(t *testing.T) {
	ex := NewExcluder([]string{"internal.example.com", "*.lab.example.com", "staging-*", "db?.example.com"})
	cases := map[string]bool{
		"internal.example.com":   true,
		"INTERNAL.Example.com":   true,
		"x.internal.example.com": false,
		"a.lab.example.com":      true,
		"a.b.lab.example.com":    true, // * matches dots too
		"lab.example.com":        false,
		"staging-api.example.io": true,
		"api-staging-x":          false,
		"db1.example.com":        true,
		"db12.example.com":       false,
		"example.com":            false,
	}
	for d, want := range cases {
		if _, got := ex.Match([]string{d}); got != want {
			t.Errorf("%s: excluded = %v, want %v", d, got, want)
		}
	}
	// Any domain of the host matching excludes the host.
	if _, ok := ex.Match([]string{"public.example.com", "internal.example.com"}); !ok {
		t.Error("a host is excluded when any of its domains matches")
	}
}

// ---- certificates ----

func TestCertTag(t *testing.T) {
	name := CertTag("example.com, *.example.com", 12)
	if name != "example.com, *.example.com [npm-sync:12]" {
		t.Fatalf("tag = %q", name)
	}
	if id, ok := ParseCertTag(name); !ok || id != 12 {
		t.Fatalf("parse = %d, %v", id, ok)
	}
	for _, n := range []string{"example.com", "[npm-sync:x]", "a [npm-sync:3] b"} {
		if _, ok := ParseCertTag(n); ok {
			t.Errorf("%q should not parse as a tag", n)
		}
	}
}

func copyOf(id, masterID int, expires time.Time, files bool) npm.Certificate {
	meta := map[string]any{}
	if files {
		meta["certificate"], meta["certificate_key"] = true, true
	}
	return npm.Certificate{ID: id, Provider: "other", NiceName: CertTag("example.com", masterID), ExpiresOn: ts(expires), Meta: meta}
}

func TestPlanCertificates(t *testing.T) {
	exp := now.Add(60 * 24 * time.Hour)
	master := []npm.Certificate{
		{ID: 1, Provider: "letsencrypt", NiceName: "new.example.com", ExpiresOn: ts(exp)},
		{ID: 2, Provider: "letsencrypt", NiceName: "same.example.com", ExpiresOn: ts(exp)},
		{ID: 3, Provider: "letsencrypt", NiceName: "renewed.example.com", ExpiresOn: ts(exp)},
		{ID: 4, Provider: "letsencrypt", NiceName: "broken.example.com", ExpiresOn: ts(exp)},
		{ID: 5, Provider: "letsencrypt", NiceName: "expired.example.com", ExpiresOn: ts(now.Add(-time.Hour))},
		{ID: 6, Provider: "other", NiceName: "custom", ExpiresOn: ts(exp)},
	}
	backup := []npm.Certificate{
		copyOf(20, 2, exp, true),
		copyOf(21, 2, exp, true), // duplicate: higher ID is cleaned up
		copyOf(30, 3, now.Add(10*24*time.Hour), true),
		copyOf(40, 4, now, false), // created but upload failed
		copyOf(90, 9, exp, true),  // master certificate gone
		{ID: 99, Provider: "other", NiceName: "hand made", ExpiresOn: ts(exp), Meta: map[string]any{"certificate": true, "certificate_key": true}},
	}
	plan := PlanCertificates(master, backup, now, true)

	got := map[int]CertOp{}
	for _, a := range plan.Actions {
		got[a.Master.ID] = a.Op
	}
	want := map[int]CertOp{1: CertCreate, 2: CertUnchanged, 3: CertRefresh, 4: CertRefresh}
	for id, op := range want {
		if got[id] != op {
			t.Errorf("master cert %d: op = %v, want %v", id, got[id], op)
		}
	}
	if _, ok := got[5]; ok || len(plan.Errors) != 1 || !strings.Contains(plan.Errors[0].Message, "expired") {
		t.Errorf("expired master certificate should be an error, not an action: %+v", plan.Errors)
	}
	if _, ok := got[6]; ok || plan.MasterCustom[6].ID != 6 {
		t.Error("custom master certificate should not be copied")
	}
	// Usable before this run: 2 (lowest ID copy) and 3 (still valid); not 4.
	if plan.Usable[2] != 20 || plan.Usable[3] != 30 {
		t.Errorf("usable = %v", plan.Usable)
	}
	if _, ok := plan.Usable[4]; ok {
		t.Error("a copy whose upload failed must never be usable")
	}
	var cleanup []int
	for _, c := range plan.Cleanup {
		cleanup = append(cleanup, c.Cert.ID)
	}
	if len(cleanup) != 2 || cleanup[0] != 21 || cleanup[1] != 90 {
		t.Errorf("cleanup = %v, want [21 90] (never the untagged 99)", cleanup)
	}
}

func TestPlanCertificatesOff(t *testing.T) {
	exp := now.Add(60 * 24 * time.Hour)
	master := []npm.Certificate{{ID: 1, Provider: "letsencrypt", NiceName: "a", ExpiresOn: ts(exp)}, {ID: 2, Provider: "letsencrypt", NiceName: "b", ExpiresOn: ts(exp)}}
	backup := []npm.Certificate{copyOf(10, 1, now.Add(time.Hour), true), copyOf(11, 7, exp, true)}
	plan := PlanCertificates(master, backup, now, false)
	if len(plan.Actions) != 0 || len(plan.Cleanup) != 0 || len(plan.Errors) != 0 {
		t.Fatalf("certificates off must not create, refresh or delete: %+v", plan)
	}
	if plan.Usable[1] != 10 {
		t.Fatalf("existing usable copy should still be used: %v", plan.Usable)
	}
}

// ---- hosts ----

func baseEnv() HostEnv {
	return HostEnv{
		CertMap:                  map[int]int{1: 101},
		MasterCustom:             map[int]npm.Certificate{2: {ID: 2, NiceName: "custom"}},
		MasterCerts:              map[int]npm.Certificate{1: {ID: 1, NiceName: "le"}, 2: {ID: 2, NiceName: "custom"}, 3: {ID: 3, NiceName: "le3"}},
		DefaultAccessListProblem: "the access list was not synced",
	}
}

func TestPlanHostsBasic(t *testing.T) {
	master := []npm.Host{
		host(t, 1, `"domain_names":["new.example.com"]`),
		host(t, 2, `"domain_names":["same.example.com"],"meta":{"nginx_online":true,"nginx_err":null,"letsencrypt_agree":true}`),
		host(t, 3, `"domain_names":["changed.example.com"],"forward_port":8080`),
		host(t, 4, `"domain_names":["a.example.com","b.example.com"]`),
	}
	backup := []npm.Host{
		// Status meta differs; user meta is the same -> unchanged.
		host(t, 11, `"domain_names":["SAME.example.com"],"meta":{"nginx_online":false,"nginx_err":"boom","letsencrypt_agree":true}`),
		host(t, 12, `"domain_names":["changed.example.com"],"forward_port":80`),
		host(t, 13, `"domain_names":["a.example.com"]`), // domain set changed on master
		host(t, 14, `"domain_names":["handmade.example.com"]`),
	}
	actions := PlanHosts(&ProxyHostSpec, master, backup, baseEnv())
	got := ops(actions)
	want := map[string]HostOp{
		"new.example.com":              HostCreate,
		"same.example.com":             HostUnchanged,
		"changed.example.com":          HostUpdate,
		"a.example.com, b.example.com": HostCreate,
		"a.example.com":                HostDelete,
		"handmade.example.com":         HostDelete,
	}
	if len(got) != len(want) {
		t.Fatalf("ops = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: op = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
	upd := find(t, actions, "changed.example.com", HostUpdate)
	if upd.BackupID != 12 || npm.IntField(upd.Payload, "forward_port") != 8080 {
		t.Errorf("update = %+v", upd)
	}
	if _, hasStatus := find(t, actions, "new.example.com", HostCreate).Payload["meta"].(map[string]any)["nginx_online"]; hasStatus {
		t.Error("status meta keys must not be sent")
	}
	if _, ok := find(t, actions, "new.example.com", HostCreate).Payload["id"]; ok {
		t.Error("read-only fields must not be sent")
	}
}

func TestPlanHostsCertificateRewriting(t *testing.T) {
	tls := `"ssl_forced":true,"http2_support":true,"hsts_enabled":true,"hsts_subdomains":true`
	master := []npm.Host{
		host(t, 1, `"domain_names":["le.example.com"],"certificate_id":1,`+tls),
		host(t, 2, `"domain_names":["custom.example.com"],"certificate_id":2,`+tls),
		host(t, 3, `"domain_names":["nocopy.example.com"],"certificate_id":3,`+tls),
	}
	actions := PlanHosts(&ProxyHostSpec, master, nil, baseEnv())

	le := find(t, actions, "le.example.com", HostCreate)
	if npm.IntField(le.Payload, "certificate_id") != 101 || le.Degraded != "" || le.Payload["ssl_forced"] != true {
		t.Errorf("certificate should be rewritten to the backup copy: %+v", le)
	}
	for _, name := range []string{"custom.example.com", "nocopy.example.com"} {
		a := find(t, actions, name, HostCreate)
		if a.Degraded == "" || npm.IntField(a.Payload, "certificate_id") != 0 {
			t.Errorf("%s: should be synced without TLS and degraded: %+v", name, a)
		}
		for _, f := range ProxyHostSpec.TLSFields {
			if a.Payload[f] != false {
				t.Errorf("%s: %s should be off", name, f)
			}
		}
	}
	if a := find(t, actions, "custom.example.com", HostCreate); !strings.Contains(a.Degraded, "custom certificate") {
		t.Errorf("reason = %q", a.Degraded)
	}

	// Comparison uses the rewritten ID: a backup host already pointing at
	// the copy is unchanged.
	backup := []npm.Host{host(t, 50, `"domain_names":["le.example.com"],"certificate_id":101,`+tls)}
	if got := ops(PlanHosts(&ProxyHostSpec, master[:1], backup, baseEnv())); got["le.example.com"] != HostUnchanged {
		t.Errorf("ops = %v", got)
	}
}

func TestPlanHostsAccessListFailClosed(t *testing.T) {
	master := []npm.Host{
		host(t, 1, `"domain_names":["admin.example.com"],"access_list_id":7`),
		host(t, 2, `"domain_names":["loc.example.com"],"locations":[{"path":"/admin","forward_scheme":"http","forward_host":"x","forward_port":80,"access_list_id":7}]`),
		host(t, 3, `"domain_names":["new-admin.example.com"],"access_list_id":7`),
	}
	backup := []npm.Host{
		host(t, 11, `"domain_names":["admin.example.com"],"forward_host":"old"`),
		host(t, 12, `"domain_names":["loc.example.com"]`),
	}
	actions := PlanHosts(&ProxyHostSpec, master, backup, baseEnv())
	for _, a := range actions {
		if a.Op != HostSkip {
			t.Errorf("%s: op = %v, want skip (and never delete the matching backup host)", a.Name(), a.Op)
		}
	}
	if a := find(t, actions, "admin.example.com", HostSkip); a.BackupID != 11 || !strings.Contains(a.Message, "the access list was not synced") {
		t.Errorf("skip = %+v", a)
	}

	// Once the list is synced, IDs are rewritten.
	env := baseEnv()
	env.AccessListMap = map[int]int{7: 70}
	actions = PlanHosts(&ProxyHostSpec, master, backup, env)
	if a := find(t, actions, "admin.example.com", HostUpdate); npm.IntField(a.Payload, "access_list_id") != 70 {
		t.Errorf("access list not rewritten: %+v", a.Payload)
	}
	loc := find(t, actions, "loc.example.com", HostUpdate).Payload["locations"].([]any)[0].(map[string]any)
	if npm.IntField(loc, "access_list_id") != 70 {
		t.Errorf("location access list not rewritten: %+v", loc)
	}
}

func TestPlanHostsExcluded(t *testing.T) {
	env := baseEnv()
	env.Excluder = NewExcluder([]string{"*.lab.example.com"})
	master := []npm.Host{
		host(t, 1, `"domain_names":["svc.lab.example.com"]`),
		host(t, 2, `"domain_names":["both.lab.example.com"],"forward_port":1`),
		host(t, 3, `"domain_names":["public.example.com"]`),
	}
	backup := []npm.Host{
		host(t, 11, `"domain_names":["own.lab.example.com"]`),
		host(t, 12, `"domain_names":["both.lab.example.com"],"forward_port":2`),
		host(t, 13, `"domain_names":["x.lab.example.com","public2.example.com"]`),
	}
	actions := PlanHosts(&ProxyHostSpec, master, backup, env)
	for _, a := range actions {
		switch a.Name() {
		case "public.example.com":
			if a.Op != HostCreate {
				t.Errorf("public: %v", a.Op)
			}
		default:
			if a.Op != HostExcluded {
				t.Errorf("%s: op = %v, want excluded (never updated or deleted)", a.Name(), a.Op)
			}
		}
	}
	if n := len(actions); n != 5 {
		t.Errorf("actions = %v", ops(actions))
	}
}

func TestPlanHostsDuplicateBackupHosts(t *testing.T) {
	master := []npm.Host{host(t, 1, `"domain_names":["a.example.com"]`)}
	backup := []npm.Host{host(t, 20, `"domain_names":["a.example.com"]`), host(t, 10, `"domain_names":["A.example.com"]`)}
	actions := PlanHosts(&ProxyHostSpec, master, backup, baseEnv())
	if a := find(t, actions, "a.example.com", HostUnchanged); a.BackupID != 10 {
		t.Errorf("lowest ID should match: %+v", a)
	}
	if a := find(t, actions, "a.example.com", HostDelete); a.BackupID != 20 {
		t.Errorf("other duplicates are unmatched: %+v", a)
	}
}

func TestEqualIgnoresNPMFilledFields(t *testing.T) {
	a := host(t, 1, `"domain_names":["b.example.com","a.example.com"],"locations":[{"path":"/","forward_scheme":"http","forward_host":"x","forward_port":80}]`)
	b := host(t, 2, `"domain_names":["a.example.com","B.example.com"],"modified_on":"x","locations":[{"id":5,"path":"/","forward_scheme":"http","forward_host":"x","forward_port":80}],"meta":{"nginx_online":false}`)
	if !Equal(&ProxyHostSpec, a.Fields, b.Fields) {
		t.Fatalf("\n%s\n%s", canonical(&ProxyHostSpec, a.Fields), canonical(&ProxyHostSpec, b.Fields))
	}
	c := host(t, 3, `"domain_names":["a.example.com","b.example.com"],"advanced_config":"x"`)
	if Equal(&ProxyHostSpec, a.Fields, c.Fields) {
		t.Fatal("different advanced_config compared equal")
	}
}
