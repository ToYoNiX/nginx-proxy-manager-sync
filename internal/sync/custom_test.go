package sync

import (
	"strings"
	"testing"
	"time"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
)

func TestRunCustomCertificates(t *testing.T) {
	e := newEnv(t)
	cert := e.master.AddCustom("my custom", days(200))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"custom.example.com"}, "certificate_id": cert, "ssl_forced": true})

	res := e.run()
	if !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	certs := e.backup.CertList()
	mc := e.master.Certs[cert]
	if len(certs) != 1 || certs[0].NiceName != CertTag("my custom", cert) ||
		string(certs[0].KeyPEM) != string(mc.KeyPEM) || string(certs[0].CertPEM) != string(mc.CertPEM) || string(certs[0].ChainPEM) != string(mc.ChainPEM) {
		t.Fatalf("copy does not match the master's files: %+v", certs)
	}
	h := e.backupHosts()["custom.example.com"]
	if num(h["certificate_id"]) != certs[0].ID || h["ssl_forced"] != true {
		t.Fatalf("host = %v", h)
	}
	for _, r := range e.master.Requests {
		if strings.Contains(r, "download") {
			t.Fatal("custom certificates are copied from the listing, not downloaded")
		}
	}

	// Nothing changed: nothing is written.
	e.run()
	if w := e.backup.Writes(); len(w) != 0 {
		t.Fatalf("second run wrote %v", w)
	}

	// Re-uploaded on the master with the same expiry: detected from the
	// files, refreshed, and nginx reloaded.
	exp := e.master.Certs[cert].ExpiresOn
	e.master.Reupload(cert, exp)
	e.run()
	w := e.backup.Writes()
	if len(w) != 2 || !strings.HasSuffix(w[0], "/upload") || !strings.HasPrefix(w[1], "PUT /api/nginx/proxy-hosts/") {
		t.Fatalf("writes = %v", w)
	}
	if string(e.backup.CertList()[0].KeyPEM) != string(e.master.Certs[cert].KeyPEM) {
		t.Fatal("new key not copied")
	}
}

func TestRunCustomCertificateExpired(t *testing.T) {
	e := newEnv(t)
	cert := e.master.AddCustom("old", days(10))
	e.master.Certs[cert].ExpiresOn = time.Now().Add(-time.Hour)
	res := e.run()
	if o := objects(res)["certificate:old"]; o.Action != ActionError || !strings.Contains(o.Message, "expired") {
		t.Fatalf("%+v", o)
	}
}

// After NPM stops returning private keys (the release after v2.16.0).
func TestRunCustomCertificateKeyHidden(t *testing.T) {
	e := newEnv(t)
	cert := e.master.AddCustom("my custom", days(200))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"custom.example.com"}, "certificate_id": cert, "ssl_forced": true})

	// Without an earlier copy: synced without TLS, degraded (no error).
	e.master.HideKeys = true
	res := e.run()
	if !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	if o := objects(res)["proxy host:custom.example.com"]; o.Action != ActionDegraded || !strings.Contains(o.Message, "did not return its private key") {
		t.Fatalf("%+v", o)
	}

	// With a copy from an earlier run: the copy keeps being used, and is
	// neither refreshed nor deleted.
	e.master.HideKeys = false
	e.run()
	copyID := e.backup.CertList()[0].ID
	e.master.HideKeys = true
	e.master.Reupload(cert, days(300)) // cannot be followed any more
	res = e.run()
	if !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	if w := e.backup.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
	if o := objects(res)["certificate:my custom"]; !strings.Contains(o.Message, "kept as is") {
		t.Fatalf("%+v", o)
	}
	if num(e.backupHosts()["custom.example.com"]["certificate_id"]) != copyID {
		t.Fatal("host should keep using the copy")
	}
}

// A backup on an NPM release that hides certificate_key must still see its
// copies as usable, or every run would re-upload them.
func TestRunBackupHidesKeys(t *testing.T) {
	e := newEnv(t)
	e.backup.HideKeys = true
	le := e.master.AddLetsEncrypt([]string{"example.com"}, days(60))
	custom := e.master.AddCustom("my custom", days(200))
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}, "certificate_id": le})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"b.example.com"}, "certificate_id": custom})
	if res := e.run(); !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	e.run()
	if w := e.backup.Writes(); len(w) != 0 {
		t.Fatalf("second run wrote %v", w)
	}
	// Change detection still works from the public files.
	e.master.Reupload(custom, e.master.Certs[custom].ExpiresOn)
	e.run()
	if w := e.backup.Writes(); len(w) == 0 || !strings.HasSuffix(w[0], "/upload") {
		t.Fatalf("writes = %v", w)
	}
}

func TestPlanCustomCertificates(t *testing.T) {
	exp := now.Add(100 * 24 * time.Hour)
	files := &npm.CertFiles{Certificate: []byte("c"), PrivateKey: []byte("k")}
	master := []npm.Certificate{
		{ID: 1, Provider: "other", NiceName: "copyable", ExpiresOn: ts(exp), Files: files, CertHash: "h1"},
		{ID: 2, Provider: "other", NiceName: "changed", ExpiresOn: ts(exp), Files: files, CertHash: "new"},
		{ID: 3, Provider: "other", NiceName: "hidden, no copy", ExpiresOn: ts(exp)},
		{ID: 4, Provider: "other", NiceName: "hidden, copy", ExpiresOn: ts(exp)},
	}
	withHash := func(c npm.Certificate, h string) npm.Certificate { c.CertHash = h; return c }
	backup := []npm.Certificate{
		withHash(copyOf(20, 2, exp, true), "old"),
		withHash(copyOf(40, 4, exp, true), "x"),
	}
	plan := PlanCertificates(master, backup, now, true)
	got := map[int]CertOp{}
	for _, a := range plan.Actions {
		got[a.Master.ID] = a.Op
	}
	if got[1] != CertCreate || got[2] != CertRefresh || len(got) != 2 {
		t.Fatalf("ops = %v", got)
	}
	if _, ok := plan.MasterCustom[3]; !ok {
		t.Error("a custom certificate without files or copy cannot be copied")
	}
	if _, ok := plan.MasterCustom[4]; ok || plan.Usable[4] != 40 || len(plan.Kept) != 1 {
		t.Errorf("an existing usable copy should be kept: usable=%v kept=%v", plan.Usable, plan.Kept)
	}
	if len(plan.Cleanup) != 0 {
		t.Errorf("copies of existing master certificates are never cleaned up: %v", plan.Cleanup)
	}
}

func TestHasFiles(t *testing.T) {
	cases := []struct {
		meta map[string]any
		want bool
	}{
		{map[string]any{}, false}, // upload failed
		{map[string]any{"certificate": true, "certificate_key": true}, true},
		{map[string]any{"certificate": true}, true}, // NPM hides the key
		{map[string]any{"certificate": true, "certificate_key": false}, false},
	}
	for _, c := range cases {
		if got := (npm.Certificate{Meta: c.meta}).HasFiles(); got != c.want {
			t.Errorf("%v: %v", c.meta, got)
		}
	}
}
