//go:build integration

// Integration test against two real NPM instances (spec 10.4). Run it with
// test/integration/run.sh, which starts them with docker compose.
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/config"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm/npmtest"
	npmsync "github.com/toyonix/nginx-proxy-manager-sync/internal/sync"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	masterURL       = env("NPM_MASTER_URL", "http://127.0.0.1:18181")
	backupURL       = env("NPM_BACKUP_URL", "http://127.0.0.1:18182")
	masterContainer = env("NPM_MASTER_CONTAINER", "npmsynctest-npm-master-1")
	user, pass      = "admin@example.com", "changeme123"
)

func client(t *testing.T, url string) *npm.Client {
	t.Helper()
	c, err := npm.New(url, user, pass, 60*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

func seedCert(t *testing.T, version int, domains ...string) int {
	t.Helper()
	args := append([]string{masterContainer, "90", fmt.Sprint(version)}, domains...)
	out, err := exec.Command("./seed-letsencrypt-cert.sh", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("seeding certificate: %v\n%s", err, out)
	}
	var id int
	fmt.Sscan(strings.TrimSpace(string(out)), &id)
	return id
}

func create(t *testing.T, c *npm.Client, fields map[string]any) {
	t.Helper()
	createKind(t, c, npm.ProxyHosts, fields)
}

func createKind(t *testing.T, c *npm.Client, kind npm.HostKind, fields map[string]any) int {
	t.Helper()
	h, err := c.CreateHost(context.Background(), kind, fields, func(npm.Host) bool { return false })
	if err != nil {
		t.Fatalf("creating %s: %v", kind.Name, err)
	}
	return h.ID
}

func runSync(t *testing.T) *npmsync.RunResult {
	t.Helper()
	cfg := config.Default()
	cfg.Master = config.Instance{URL: masterURL, Username: user, Password: pass}
	cfg.Backups = []config.Backup{{URL: backupURL, Username: user, Password: pass}}
	cfg.ExcludeDomains = []string{"*.lab.example.com"}
	cfg.AccessListPasswords = map[string]map[string]string{"Admin panels": {"alice": "s3cret"}}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return npmsync.New(&cfg, log).Run(context.Background(), "integration", npmsync.RunOptions{})
}

func TestBackupMatchesMaster(t *testing.T) {
	ctx := context.Background()
	m, b := client(t, masterURL), client(t, backupURL)

	certID := seedCert(t, 1, "example.com", "*.example.com")

	// A custom certificate uploaded by hand on the master.
	certPEM, chainPEM, keyPEM := npmtest.MakeCert([]string{"custom.example.com"}, time.Now().Add(200*24*time.Hour))
	custom, err := m.CreateCustomCertificate(ctx, "my custom", func([]npm.Certificate) (npm.Certificate, bool) { return npm.Certificate{}, false })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.UploadCertificate(ctx, custom.ID, npm.CertFiles{Certificate: certPEM, Chain: chainPEM, PrivateKey: keyPEM}); err != nil {
		t.Fatal(err)
	}
	create(t, m, map[string]any{"domain_names": []string{"custom.example.com"}, "forward_scheme": "http", "forward_host": "10.0.0.10",
		"forward_port": 80, "certificate_id": custom.ID, "ssl_forced": true})
	al, err := m.CreateAccessList(ctx, map[string]any{"name": "Admin panels", "satisfy_any": false, "pass_auth": false,
		"items": []any{map[string]any{"username": "alice", "password": "s3cret"}}, "clients": []any{map[string]any{"address": "10.0.0.0/8", "directive": "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	create(t, m, map[string]any{"domain_names": []string{"admin.example.com"}, "forward_scheme": "http", "forward_host": "10.0.0.9",
		"forward_port": 80, "access_list_id": al.ID})
	create(t, m, map[string]any{"domain_names": []string{"app.example.com", "www.example.com"}, "forward_scheme": "http",
		"forward_host": "10.0.0.5", "forward_port": 8080, "certificate_id": certID, "ssl_forced": true, "http2_support": true,
		"meta":      map[string]any{"letsencrypt_agree": false, "dns_challenge": false},
		"locations": []map[string]any{{"path": "/api", "forward_scheme": "http", "forward_host": "10.0.0.6", "forward_port": 9000}}})
	create(t, m, map[string]any{"domain_names": []string{"off.example.com"}, "forward_scheme": "http", "forward_host": "10.0.0.7",
		"forward_port": 80, "enabled": false})
	create(t, m, map[string]any{"domain_names": []string{"svc.lab.example.com"}, "forward_scheme": "http", "forward_host": "10.0.0.8", "forward_port": 80})
	create(t, b, map[string]any{"domain_names": []string{"handmade.example.com"}, "forward_scheme": "http", "forward_host": "1.2.3.4", "forward_port": 80})

	// Redirection hosts, 404 hosts and streams. old.example.com is a proxy host on the backup but a
	// redirection host on the master (a domain moving between types).
	create(t, b, map[string]any{"domain_names": []string{"old.example.com"}, "forward_scheme": "http", "forward_host": "1.2.3.4", "forward_port": 80})
	redir := createKind(t, m, npm.RedirectionHosts, map[string]any{"domain_names": []string{"old.example.com"}, "forward_scheme": "auto",
		"forward_http_code": 308, "forward_domain_name": "new.example.com", "preserve_path": true, "certificate_id": certID, "ssl_forced": true})
	if err := m.SetHostEnabled(ctx, npm.RedirectionHosts, redir, false); err != nil {
		t.Fatal(err)
	}
	createKind(t, m, npm.DeadHosts, map[string]any{"domain_names": []string{"gone.example.com"}, "advanced_config": "# gone"})
	createKind(t, m, npm.Streams, map[string]any{"incoming_port": 5432, "forwarding_host": "10.0.0.20", "forwarding_port": 5432, "tcp_forwarding": true, "udp_forwarding": false})
	createKind(t, m, npm.Streams, map[string]any{"incoming_port": 5353, "forwarding_host": "10.0.0.21", "forwarding_port": 53, "tcp_forwarding": true, "udp_forwarding": true})
	createKind(t, b, npm.Streams, map[string]any{"incoming_port": 7777, "forwarding_host": "1.2.3.4", "forwarding_port": 1, "tcp_forwarding": true, "udp_forwarding": false})
	create(t, b, map[string]any{"domain_names": []string{"own.lab.example.com"}, "forward_scheme": "http", "forward_host": "1.2.3.4", "forward_port": 80})

	res := runSync(t)
	if !res.Succeeded() {
		t.Fatalf("sync failed: %+v", res.Backups[0])
	}

	// Planning again must find nothing to do: every host matches.
	masterCerts, _ := m.Certificates(ctx)
	backupCerts, _ := b.Certificates(ctx)
	certPlan := npmsync.PlanCertificates(masterCerts, backupCerts, time.Now(), true)
	for _, a := range certPlan.Actions {
		if a.Op != npmsync.CertUnchanged {
			t.Errorf("certificate %s: %v", a.Master.NiceName, a.Op)
		}
	}
	if len(certPlan.Actions) != 2 || len(certPlan.MasterCustom) != 0 {
		t.Errorf("both the Let's Encrypt and the custom certificate should be copied: %+v", certPlan)
	}
	for _, c := range backupCerts {
		if id, _ := npmsync.ParseCertTag(c.NiceName); id == custom.ID && (c.Files == nil || string(c.Files.PrivateKey) != string(keyPEM)) {
			t.Error("the custom certificate's key was not copied")
		}
	}
	npm.WipeFiles(masterCerts)
	npm.WipeFiles(backupCerts)
	masterLists, _ := m.AccessLists(ctx)
	backupLists, _ := b.AccessLists(ctx)
	alPlan := npmsync.PlanAccessLists(masterLists, backupLists, map[string]map[string]string{"Admin panels": {"alice": "s3cret"}}, false)
	alMap := map[int]int{}
	for _, a := range alPlan.Actions {
		if a.Op != npmsync.ALUnchanged {
			t.Errorf("access list %s: %v %s", a.Master.Name, a.Op, a.Reason)
		}
		alMap[a.Master.ID] = a.BackupID
	}
	if len(backupLists) != 1 || len(backupLists[0].Items) != 1 || backupLists[0].Items[0].Hint != "s*****" || len(backupLists[0].Clients) != 1 {
		t.Errorf("backup access lists = %+v", backupLists)
	}
	masterHosts, _ := m.Hosts(ctx, npm.ProxyHosts)
	backupHosts, _ := b.Hosts(ctx, npm.ProxyHosts)
	actions := npmsync.PlanHosts(&npmsync.ProxyHostSpec, masterHosts, backupHosts, npmsync.HostEnv{
		CertMap:       certPlan.Usable,
		AccessListMap: alMap,
		Excluder:      npmsync.NewExcluder([]string{"*.lab.example.com"}),
	})
	names := map[string]npmsync.HostOp{}
	for _, a := range actions {
		names[a.Name()] = a.Op
		if a.Op != npmsync.HostUnchanged && a.Op != npmsync.HostExcluded {
			t.Errorf("%s: %v after sync", a.Name(), a.Op)
		}
	}
	if names["own.lab.example.com"] != npmsync.HostExcluded || names["svc.lab.example.com"] != npmsync.HostExcluded {
		t.Errorf("excluded hosts: %v", names)
	}
	if _, ok := names["handmade.example.com"]; ok {
		t.Error("hand-made backup host should have been deleted")
	}

	// Redirection hosts, 404 hosts and streams match too.
	for _, spec := range []*npmsync.HostSpec{&npmsync.RedirectionHostSpec, &npmsync.DeadHostSpec, &npmsync.StreamSpec} {
		mh, err := m.Hosts(ctx, spec.Kind)
		if err != nil {
			t.Fatal(err)
		}
		bh, _ := b.Hosts(ctx, spec.Kind)
		if len(mh) != len(bh) {
			t.Errorf("%s: master has %d, backup has %d", spec.Kind.Name, len(mh), len(bh))
		}
		for _, a := range npmsync.PlanHosts(spec, mh, bh, npmsync.HostEnv{CertMap: certPlan.Usable}) {
			if a.Op != npmsync.HostUnchanged {
				t.Errorf("%s %s: %v after sync", spec.Kind.Name, a.Name(), a.Op)
			}
		}
		for _, h := range bh {
			if spec == &npmsync.RedirectionHostSpec && h.Enabled() {
				t.Error("the disabled redirection host should be disabled on the backup")
			}
		}
	}

	// The backup's nginx serves the master's custom certificate.
	served, err := exec.Command("docker", "exec", env("NPM_BACKUP_CONTAINER", "npmsynctest-npm-backup-1"), "sh", "-c",
		"echo | openssl s_client -connect 127.0.0.1:443 -servername custom.example.com 2>/dev/null | openssl x509 -outform pem").Output()
	if err != nil || !strings.Contains(string(certPEM), strings.TrimSpace(string(served))) || len(served) == 0 {
		t.Errorf("backup does not serve the custom certificate (%v):\n%s", err, served)
	}

	// The backup's nginx accepted the configuration.
	for _, h := range backupHosts {
		if meta, _ := h.Fields["meta"].(map[string]any); meta["nginx_online"] == false {
			t.Errorf("%v: nginx_err = %v", h.DomainNames, meta["nginx_err"])
		}
	}

	// A renewal on the master is copied on the next run.
	seedCert(t, 2, "example.com", "*.example.com")
	res = runSync(t)
	if !res.Succeeded() {
		t.Fatalf("sync after renewal failed: %+v", res.Backups[0])
	}
	updated := false
	for _, o := range res.Backups[0].Objects {
		if o.Type == npmsync.TypeCertificate && o.Action == npmsync.ActionUpdated {
			updated = true
		}
	}
	if !updated {
		t.Errorf("renewed certificate was not refreshed: %+v", res.Backups[0].Objects)
	}
}
