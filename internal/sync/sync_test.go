package sync

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/config"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm/npmtest"
)

const proxyPath = "/api/nginx/proxy-hosts"

type env struct {
	t              *testing.T
	master, backup *npmtest.Fake
	cfg            *config.Config
	opts           RunOptions
}

func newEnv(t *testing.T) *env {
	t.Helper()
	m, b := npmtest.New(), npmtest.New()
	t.Cleanup(m.Close)
	t.Cleanup(b.Close)
	cfg := config.Default()
	cfg.Master = config.Instance{URL: m.URL, Username: "admin@example.com", Password: "changeme"}
	cfg.Backups = []config.Backup{{URL: b.URL, Username: "admin@example.com", Password: "changeme"}}
	return &env{t: t, master: m, backup: b, cfg: &cfg}
}

func (e *env) run() *RunResult {
	e.t.Helper()
	e.master.ResetLog()
	e.backup.ResetLog()
	s := New(e.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.NewClient = func(inst config.Instance) (*npm.Client, error) {
		c, err := npm.New(inst.URL, inst.Username, inst.Password, 5*time.Second, nil)
		if c != nil {
			c.Sleep = func(context.Context, time.Duration) error { return nil }
		}
		return c, err
	}
	return s.Run(context.Background(), "test", e.opts)
}

func (e *env) backupHosts() map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, h := range e.backup.HostList(proxyPath) {
		var d []string
		for _, x := range h["domain_names"].([]any) {
			d = append(d, x.(string))
		}
		out[DomainKey(d)] = h
	}
	return out
}

func num(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	}
	return -1
}

func objects(r *RunResult) map[string]ObjectResult {
	out := map[string]ObjectResult{}
	for _, o := range r.Backups[0].Objects {
		out[o.Type+":"+o.Name] = o
	}
	return out
}

func days(n int) time.Time { return time.Now().Add(time.Duration(n) * 24 * time.Hour) }

func TestRunFullSyncAndIdempotence(t *testing.T) {
	e := newEnv(t)
	cert := e.master.AddLetsEncrypt([]string{"example.com", "*.example.com"}, days(60))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"app.example.com"}, "forward_host": "10.0.0.5", "certificate_id": cert, "ssl_forced": true})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"off.example.com"}, "enabled": false})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"handmade.example.com"}})

	res := e.run()
	if !res.Succeeded() {
		t.Fatalf("run failed: %+v", res.Backups[0])
	}
	certs := e.backup.CertList()
	if len(certs) != 1 || certs[0].NiceName != "example.com, *.example.com [npm-sync:"+itoa(cert)+"]" || certs[0].KeyPEM == nil {
		t.Fatalf("backup certs = %+v", certs)
	}
	hosts := e.backupHosts()
	if len(hosts) != 2 || hosts["handmade.example.com"] != nil {
		t.Fatalf("backup hosts = %v", hosts)
	}
	app := hosts["app.example.com"]
	if num(app["certificate_id"]) != certs[0].ID || app["ssl_forced"] != true || app["forward_host"] != "10.0.0.5" {
		t.Fatalf("app host = %v", app)
	}
	if hosts["off.example.com"]["enabled"] != false {
		t.Fatal("disabled hosts are synced as disabled")
	}

	// A second run writes nothing and downloads no key material.
	res = e.run()
	if w := e.backup.Writes(); len(w) != 0 || !res.Succeeded() {
		t.Fatalf("second run wrote %v", w)
	}
	for _, r := range e.master.Requests {
		if strings.Contains(r, "download") {
			t.Fatal("unchanged certificates must not be downloaded")
		}
	}
}

func TestRunDeletesBeforeCreates(t *testing.T) {
	e := newEnv(t)
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com", "b.example.com"}})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"c.example.com"}, "forward_port": 9})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"c.example.com"}})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}})

	if res := e.run(); !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	w := e.backup.Writes()
	if len(w) != 3 || !strings.HasPrefix(w[0], "DELETE") || !strings.HasPrefix(w[1], "PUT") || !strings.HasPrefix(w[2], "POST") {
		t.Fatalf("writes = %v, want delete, update, create", w)
	}
}

func TestRunMaxDeletesCountsDomainChanges(t *testing.T) {
	e := newEnv(t)
	e.cfg.MaxDeletes = 1
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com", "a2.example.com"}})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"b.example.com", "b2.example.com"}})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"b.example.com"}})

	res := e.run()
	if !res.Backups[0].Aborted || !strings.Contains(res.Backups[0].Error, "maxDeletes") {
		t.Fatalf("expected abort: %+v", res.Backups[0])
	}
	if w := e.backup.Writes(); len(w) != 0 {
		t.Fatalf("nothing may be written after a failed safety check: %v", w)
	}
	e.cfg.MaxDeletes = 0 // no limit
	if res := e.run(); !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
}

func TestRunEmptyMaster(t *testing.T) {
	e := newEnv(t)
	e.cfg.ExcludeDomains = []string{"*.lab.example.com"}
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"x.lab.example.com"}}) // excluded: does not count
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}})

	res := e.run()
	if !res.Backups[0].Aborted || !strings.Contains(res.Backups[0].Error, "no proxy hosts") {
		t.Fatalf("expected empty-master abort: %+v", res.Backups[0])
	}
	e.cfg.AllowEmptyMaster = true
	if res := e.run(); !res.Succeeded() || len(e.backup.HostList(proxyPath)) != 0 {
		t.Fatalf("allowEmptyMaster should let the run delete: %+v", res.Backups[0])
	}
}

func TestRunUnusableCopyIsRefreshedAndNeverReferenced(t *testing.T) {
	e := newEnv(t)
	cert := e.master.AddLetsEncrypt([]string{"example.com"}, days(60))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"app.example.com"}, "certificate_id": cert, "ssl_forced": true})
	// A copy from an earlier run whose upload failed: no files, expires_on
	// set to its creation time.
	broken := e.backup.AddRaw(npmtest.Cert{Provider: "other", NiceName: CertTag("example.com", cert), ExpiresOn: time.Now().Add(-time.Minute)})

	// The upload fails again: the host must be synced without TLS, not
	// pointed at the unusable copy.
	e.backup.Fault = func(method, path string, n int) (int, bool) {
		if strings.HasSuffix(path, "/upload") {
			return 400, false
		}
		return 0, false
	}
	res := e.run()
	app := e.backupHosts()["app.example.com"]
	if num(app["certificate_id"]) != 0 || app["ssl_forced"] != false {
		t.Fatalf("host must not reference an unusable copy: %v", app)
	}
	if o := objects(res)["proxy host:app.example.com"]; o.Action != ActionDegraded {
		t.Fatalf("host should be degraded: %+v", o)
	}
	if res.Backups[0].Succeeded() {
		t.Fatal("the failed upload is an error")
	}

	// Next run: the upload works, the same copy is reused and the host
	// moves onto it.
	e.backup.Fault = nil
	res = e.run()
	if !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	if certs := e.backup.CertList(); len(certs) != 1 || certs[0].ID != broken || certs[0].KeyPEM == nil {
		t.Fatalf("certs = %+v", certs)
	}
	app = e.backupHosts()["app.example.com"]
	if num(app["certificate_id"]) != broken || app["ssl_forced"] != true {
		t.Fatalf("host = %v", app)
	}
}

func TestRunRenewalRefreshesAndReloads(t *testing.T) {
	e := newEnv(t)
	cert := e.master.AddLetsEncrypt([]string{"example.com"}, days(30))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"off.example.com"}, "certificate_id": cert, "enabled": false})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"app.example.com"}, "certificate_id": cert})
	if res := e.run(); !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	copyID := e.backup.CertList()[0].ID

	e.master.Renew(cert, days(90))
	res := e.run()
	if !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	w := e.backup.Writes()
	appID := num(e.backupHosts()["app.example.com"]["id"])
	want := []string{"POST /api/nginx/certificates/" + itoa(copyID) + "/upload", "PUT /api/nginx/proxy-hosts/" + itoa(appID)}
	if strings.Join(w, ",") != strings.Join(want, ",") {
		t.Fatalf("writes = %v, want %v (re-save the enabled host only)", w, want)
	}
	if exp, _ := npm.ParseTime(e.backup.CertList()[0].ExpiresOn.Format(npmtest.TimeLayout)); exp.Before(days(80)) {
		t.Fatal("copy not refreshed")
	}

	// If a host write already reloaded nginx, no extra re-save.
	e.master.Renew(cert, days(120))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"new.example.com"}})
	e.run()
	for _, x := range e.backup.Writes() {
		if strings.HasPrefix(x, "PUT") {
			t.Fatalf("unexpected re-save: %v", e.backup.Writes())
		}
	}
}

func TestRunCleanupSkipsReferencedCertificates(t *testing.T) {
	e := newEnv(t)
	e.cfg.ExcludeDomains = []string{"own.example.com"}
	e.master.AddLetsEncrypt([]string{"other.example.com"}, days(60)) // the master is never empty
	cert := e.master.AddLetsEncrypt([]string{"example.com"}, days(60))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"app.example.com"}, "certificate_id": cert})
	e.run()
	copyID := e.backup.CertList()[1].ID
	// An excluded backup host uses the copy.
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"own.example.com"}, "certificate_id": copyID})

	// The master certificate goes away.
	for _, h := range e.master.HostList(proxyPath) {
		h["certificate_id"] = 0
	}
	delete(e.master.Certs, cert)
	res := e.run()
	if o := objects(res)["certificate:"+CertTag("example.com", cert)]; o.Action != ActionSkipped || o.IsError() {
		t.Fatalf("referenced copy must be kept: %+v", o)
	}
	if len(e.backup.CertList()) != 2 {
		t.Fatal("copy deleted")
	}
	if num(e.backupHosts()["app.example.com"]["certificate_id"]) != 0 {
		t.Fatal("synced host should have moved off the copy")
	}

	// Once nothing uses it, it is deleted.
	for _, h := range e.backup.HostList(proxyPath) {
		h["certificate_id"] = 0
	}
	e.run()
	if len(e.backup.CertList()) != 1 {
		t.Fatal("unused managed copy should be deleted")
	}
}

func TestRunUntaggedCertificatesAreLeftAlone(t *testing.T) {
	e := newEnv(t)
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}})
	e.backup.AddCustom("hand made", days(30))
	if res := e.run(); !res.Succeeded() || len(e.backup.CertList()) != 1 {
		t.Fatalf("untagged certificate touched: %+v", e.backup.Writes())
	}
}

func TestRunHostWithUnsyncedAccessListIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"admin.example.com"}, "access_list_id": 3, "forward_host": "new"})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"admin.example.com"}, "forward_host": "old"})
	res := e.run()
	o := objects(res)["proxy host:admin.example.com"]
	if o.Action != ActionSkipped || !o.IsError() || res.Succeeded() {
		t.Fatalf("result = %+v", o)
	}
	if w := e.backup.Writes(); len(w) != 0 {
		t.Fatalf("matching backup host must be left alone: %v", w)
	}
}

func TestRunCollisionWithExcludedHost(t *testing.T) {
	e := newEnv(t)
	e.cfg.Backups[0].ExcludeDomains = []string{"*.lab.example.com"}
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"shared.example.com"}})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"x.lab.example.com", "shared.example.com"}})
	res := e.run()
	o := objects(res)["proxy host:shared.example.com"]
	if o.Action != ActionError || !strings.Contains(o.Message, "excluded backup proxy host") || !strings.Contains(o.Message, "x.lab.example.com") {
		t.Fatalf("result = %+v", o)
	}
}

func TestRunContinueOnErrorFalse(t *testing.T) {
	e := newEnv(t)
	e.cfg.ContinueOnError = false
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"b.example.com"}})
	e.backup.Fault = func(method, path string, n int) (int, bool) {
		if method == "POST" && path == proxyPath {
			return 400, false
		}
		return 0, false
	}
	res := e.run()
	if !res.Backups[0].Aborted {
		t.Fatalf("expected abort: %+v", res.Backups[0])
	}
	if n := strings.Count(strings.Join(e.backup.Writes(), ","), "POST"); n != 1 {
		t.Fatalf("run should stop after the first failure, writes = %v", e.backup.Writes())
	}
}

func TestRunDryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.cfg.DryRun = true
	cert := e.master.AddLetsEncrypt([]string{"example.com"}, days(60))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}, "certificate_id": cert})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"z.example.com"}})
	res := e.run()
	if w := e.backup.Writes(); len(w) != 0 {
		t.Fatalf("dry run wrote %v", w)
	}
	obj := objects(res)
	if o := obj["proxy host:a.example.com"]; o.Action != ActionCreated || !o.DryRun {
		t.Fatalf("a: %+v", o)
	}
	if o := obj["proxy host:z.example.com"]; o.Action != ActionDeleted || !o.DryRun {
		t.Fatalf("z: %+v", o)
	}
}

func TestRunTwoFactorBackup(t *testing.T) {
	e := newEnv(t)
	e.backup.TwoFactor = true
	res := e.run()
	if !res.Backups[0].Aborted || !strings.Contains(res.Backups[0].Error, "2FA") {
		t.Fatalf("%+v", res.Backups[0])
	}
}

func TestRunOldBackupRefusesCertificates(t *testing.T) {
	e := newEnv(t)
	e.backup.Version = [3]int{2, 9, 8}
	cert := e.master.AddLetsEncrypt([]string{"example.com"}, days(60))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}, "certificate_id": cert})
	res := e.run()
	if len(e.backup.CertList()) != 0 {
		t.Fatal("no certificate sync on a backup older than the minimum")
	}
	if o := objects(res)["proxy host:a.example.com"]; o.Action != ActionDegraded {
		t.Fatalf("host: %+v", o)
	}
	if res.Backups[0].Succeeded() {
		t.Fatal("refusal is reported as an error")
	}
}

func TestRunCertificatesFeatureOff(t *testing.T) {
	e := newEnv(t)
	e.cfg.Backups[0].Features = map[string]bool{config.FeatureCertificates: false}
	cert := e.master.AddLetsEncrypt([]string{"example.com"}, days(60))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}, "certificate_id": cert, "ssl_forced": true})
	// An existing usable copy is still used.
	cp := e.backup.AddRaw(npmtest.Cert{Provider: "other", NiceName: CertTag("example.com", cert), ExpiresOn: days(10),
		Meta: map[string]any{"certificate": "x", "certificate_key": "y"}})
	e.backup.AddRaw(npmtest.Cert{Provider: "other", NiceName: CertTag("gone", 999), ExpiresOn: days(10)})
	res := e.run()
	if !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	if num(e.backupHosts()["a.example.com"]["certificate_id"]) != cp {
		t.Fatal("host should use the existing copy")
	}
	if len(e.backup.CertList()) != 2 {
		t.Fatal("no certificate may be created or deleted when certificates is off")
	}
}
