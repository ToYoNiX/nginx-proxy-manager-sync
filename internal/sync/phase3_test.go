package sync

import (
	"strings"
	"testing"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/config"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
)

const (
	redirPath  = "/api/nginx/redirection-hosts"
	deadPath   = "/api/nginx/dead-hosts"
	streamPath = "/api/nginx/streams"
)

func stream(t *testing.T, id int, fields string) npm.Host {
	t.Helper()
	return host(t, id, `"incoming_port":9000,"forwarding_host":"10.0.0.1","forwarding_port":9000,"tcp_forwarding":true,"udp_forwarding":false,`+fields)
}

// ---- planner ----

func TestPlanStreamsMatchByPortAndProtocols(t *testing.T) {
	master := []npm.Host{
		stream(t, 1, `"incoming_port":53,"tcp_forwarding":true,"udp_forwarding":true`),
		stream(t, 2, `"incoming_port":2222,"forwarding_port":22`),
		stream(t, 3, `"incoming_port":3000`),
	}
	backup := []npm.Host{
		stream(t, 11, `"incoming_port":53,"tcp_forwarding":false,"udp_forwarding":true`), // flags changed: delete + create
		stream(t, 12, `"incoming_port":2222,"forwarding_port":2200`),                     // same key: update
		stream(t, 13, `"incoming_port":3000`),                                            // unchanged
	}
	got := map[string]HostOp{}
	for _, a := range PlanHosts(&StreamSpec, master, backup, baseEnv()) {
		got[a.Name()+"#"+itoa(a.BackupID)] = a.Op
	}
	want := map[string]HostOp{
		"port 53 (tcp+udp) -> 10.0.0.1:9000#0": HostCreate,
		"port 53 (udp) -> 10.0.0.1:9000#11":    HostDelete,
		"port 2222 (tcp) -> 10.0.0.1:22#12":    HostUpdate,
		"port 3000 (tcp) -> 10.0.0.1:9000#13":  HostUnchanged,
	}
	if len(got) != len(want) {
		t.Fatalf("ops = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %v, want %v (all %v)", k, got[k], v, got)
		}
	}
}

func TestPlanStreamsDuplicateMasterKeys(t *testing.T) {
	master := []npm.Host{stream(t, 1, `"forwarding_host":"a"`), stream(t, 2, `"forwarding_host":"b"`)}
	backup := []npm.Host{stream(t, 10, `"forwarding_host":"old"`)}
	actions := PlanHosts(&StreamSpec, master, backup, baseEnv())
	for _, a := range actions {
		if a.Op != HostSkip || !a.Failed {
			t.Errorf("%s: %v, want a failed skip and the backup stream left alone", a.Name(), a.Op)
		}
	}
	if len(actions) != 2 || !strings.Contains(actions[0].Message, "share this incoming port") {
		t.Fatalf("%+v", actions)
	}
}

func TestPlanStreamsIgnoreExcludeDomains(t *testing.T) {
	env := baseEnv()
	env.Excluder = NewExcluder([]string{"*"})
	actions := PlanHosts(&StreamSpec, []npm.Host{stream(t, 1, `"incoming_port":1`)}, []npm.Host{stream(t, 2, `"incoming_port":2`)}, env)
	if got := ops(actions); len(got) != 2 {
		t.Fatalf("ops = %v", got)
	}
	for _, a := range actions {
		if a.Op == HostExcluded {
			t.Fatal("streams have no domains; excludeDomains must not apply")
		}
	}
}

func TestPlanStreamCertificateDegraded(t *testing.T) {
	a := PlanHosts(&StreamSpec, []npm.Host{stream(t, 1, `"certificate_id":2`)}, nil, baseEnv())[0]
	if a.Op != HostCreate || a.Degraded == "" || npm.IntField(a.Payload, "certificate_id") != 0 {
		t.Fatalf("%+v", a)
	}
	a = PlanHosts(&StreamSpec, []npm.Host{stream(t, 1, `"certificate_id":1`)}, nil, baseEnv())[0]
	if npm.IntField(a.Payload, "certificate_id") != 101 {
		t.Fatalf("certificate not rewritten: %+v", a.Payload)
	}
}

func TestPlanToggleEnabled(t *testing.T) {
	m := host(t, 1, `"domain_names":["r.example.com"],"forward_domain_name":"x.example.com","forward_http_code":301,"enabled":false`)
	b := host(t, 2, `"domain_names":["r.example.com"],"forward_domain_name":"x.example.com","forward_http_code":301,"enabled":true`)
	a := PlanHosts(&RedirectionHostSpec, []npm.Host{m}, []npm.Host{b}, baseEnv())[0]
	if a.Op != HostUpdate || a.PayloadChanged || !a.Toggle || a.Enabled {
		t.Fatalf("only enabled differs: %+v", a)
	}
	if _, ok := a.Payload["enabled"]; ok {
		t.Fatal("enabled is not part of the redirection host payload")
	}
	c := PlanHosts(&RedirectionHostSpec, []npm.Host{m}, nil, baseEnv())[0]
	if c.Op != HostCreate || !c.Toggle {
		t.Fatalf("a disabled master host is created, then disabled: %+v", c)
	}
}

// ---- executor ----

func TestRunDomainMovesBetweenHostTypes(t *testing.T) {
	e := newEnv(t)
	// On the master, a.example.com moved from a proxy host to a
	// redirection host, and b.example.com from a 404 host to a proxy host.
	e.master.AddHost(redirPath, map[string]any{"domain_names": []string{"a.example.com"}, "forward_domain_name": "new.example.com"})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"b.example.com"}})
	e.master.AddHost(deadPath, map[string]any{"domain_names": []string{"c.example.com"}}) // the master is never empty
	e.backup.AddHost(deadPath, map[string]any{"domain_names": []string{"c.example.com"}})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}})
	e.backup.AddHost(deadPath, map[string]any{"domain_names": []string{"b.example.com"}})

	res := e.run()
	if !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	w := e.backup.Writes()
	if len(w) != 4 || !strings.HasPrefix(w[0], "DELETE") || !strings.HasPrefix(w[1], "DELETE") || !strings.HasPrefix(w[2], "POST") || !strings.HasPrefix(w[3], "POST") {
		t.Fatalf("writes = %v, want both deletes before any create", w)
	}
	if len(e.backup.HostList(redirPath)) != 1 || len(e.backup.HostList(proxyPath)) != 1 || len(e.backup.HostList(deadPath)) != 1 {
		t.Fatal("backup does not match")
	}
}

func TestRunRedirectionAndDeadHostsWithEnabledState(t *testing.T) {
	e := newEnv(t)
	cert := e.master.AddLetsEncrypt([]string{"example.com"}, days(60))
	r := e.master.AddHost(redirPath, map[string]any{"domain_names": []string{"old.example.com"}, "forward_domain_name": "new.example.com",
		"forward_http_code": 308, "preserve_path": false, "certificate_id": cert, "ssl_forced": true, "enabled": false})
	e.master.AddHost(deadPath, map[string]any{"domain_names": []string{"gone.example.com"}, "advanced_config": "# x"})

	res := e.run()
	if !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	rh := e.backup.HostList(redirPath)
	if len(rh) != 1 || rh[0]["enabled"] != false || num(rh[0]["forward_http_code"]) != 308 || num(rh[0]["certificate_id"]) != e.backup.CertList()[0].ID {
		t.Fatalf("redirection host = %v", rh)
	}
	if dh := e.backup.HostList(deadPath); len(dh) != 1 || dh[0]["advanced_config"] != "# x" {
		t.Fatalf("404 host = %v", dh)
	}
	e.run()
	if w := e.backup.Writes(); len(w) != 0 {
		t.Fatalf("second run wrote %v", w)
	}

	// Enabling it on the master only calls the enable endpoint.
	e.master.Hosts[redirPath][r]["enabled"] = true
	e.run()
	if w := e.backup.Writes(); len(w) != 1 || !strings.HasSuffix(w[0], "/enable") {
		t.Fatalf("writes = %v", w)
	}
}

func TestRunStreams(t *testing.T) {
	e := newEnv(t)
	cert := e.master.AddLetsEncrypt([]string{"example.com"}, days(30))
	e.master.AddHost(streamPath, map[string]any{"incoming_port": 5432, "forwarding_host": "db", "forwarding_port": 5432, "tcp_forwarding": true})
	e.master.AddHost(streamPath, map[string]any{"incoming_port": 853, "forwarding_host": "dns", "forwarding_port": 53, "certificate_id": cert})
	e.master.AddHost(streamPath, map[string]any{"incoming_port": 9999, "forwarding_host": "x", "forwarding_port": 1, "enabled": false})
	e.backup.AddHost(streamPath, map[string]any{"incoming_port": 7777, "forwarding_host": "handmade", "forwarding_port": 1})

	if res := e.run(); !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	got := map[int]map[string]any{}
	for _, s := range e.backup.HostList(streamPath) {
		got[num(s["incoming_port"])] = s
	}
	if len(got) != 3 || got[7777] != nil || num(got[853]["certificate_id"]) != e.backup.CertList()[0].ID || got[9999]["enabled"] != false {
		t.Fatalf("streams = %v", got)
	}
	e.run()
	if w := e.backup.Writes(); len(w) != 0 {
		t.Fatalf("second run wrote %v", w)
	}

	// A renewal with only a stream using the certificate re-saves the stream.
	e.master.Renew(cert, days(90))
	e.run()
	w := e.backup.Writes()
	if len(w) != 2 || !strings.HasSuffix(w[0], "/upload") || !strings.HasPrefix(w[1], "PUT /api/nginx/streams/") {
		t.Fatalf("writes = %v", w)
	}
}

func TestRunStreamsFeatureOff(t *testing.T) {
	e := newEnv(t)
	e.cfg.Features[config.FeatureStreams] = false
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}})
	e.master.AddHost(streamPath, map[string]any{"incoming_port": 1, "forwarding_host": "x", "forwarding_port": 1})
	e.backup.AddHost(streamPath, map[string]any{"incoming_port": 2, "forwarding_host": "y", "forwarding_port": 2})
	if res := e.run(); !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	if s := e.backup.HostList(streamPath); len(s) != 1 || num(s[0]["incoming_port"]) != 2 {
		t.Fatalf("a switched-off type must be left alone: %v", s)
	}
}

func TestRunCollisionAcrossTypesWithExcludedHost(t *testing.T) {
	e := newEnv(t)
	e.cfg.ExcludeDomains = []string{"*.lab.example.com"}
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"shared.example.com"}})
	e.backup.AddHost(deadPath, map[string]any{"domain_names": []string{"x.lab.example.com", "shared.example.com"}})
	res := e.run()
	if o := objects(res)["proxy host:shared.example.com"]; o.Action != ActionError || !strings.Contains(o.Message, "excluded backup 404 host") {
		t.Fatalf("%+v", o)
	}
}

func TestRunEmptyMasterPerType(t *testing.T) {
	e := newEnv(t)
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}})
	e.backup.AddHost(streamPath, map[string]any{"incoming_port": 2, "forwarding_host": "y", "forwarding_port": 2})
	res := e.run()
	if !res.Backups[0].Aborted || !strings.Contains(res.Backups[0].Error, "no streams") {
		t.Fatalf("%+v", res.Backups[0])
	}
}
