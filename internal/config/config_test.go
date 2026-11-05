package config

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `
master:
  url: "http://npm-master.lan:81"
  username: "admin@example.com"
  password: "changeme"
backups:
  - url: "http://npm-backup.lan:81"
    username: "admin@example.com"
    password: "changeme"
`

func TestDefaults(t *testing.T) {
	cfg, err := Load(writeFile(t, minimal), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cron != "0 */12 * * *" || !cfg.RunOnStart || cfg.DryRun || !cfg.ContinueOnError || cfg.AllowEmptyMaster ||
		cfg.MaxDeletes != 10 || cfg.HTTPTimeout != 60*time.Second || cfg.Web.Port != 8080 || cfg.Log.Level != "info" || cfg.Log.Format != "console" {
		t.Fatalf("defaults = %+v", cfg)
	}
	for _, f := range AllFeatures {
		if !cfg.FeatureEnabled(0, f) {
			t.Errorf("feature %s should default to on", f)
		}
	}
}

func TestYAMLFull(t *testing.T) {
	cfg, err := Load(writeFile(t, minimal+`
    features:
      streams: false
    excludeDomains: ["backup-only.example.com"]
excludeDomains: ["internal.example.com"]
cron: ""
maxDeletes: 0
httpTimeout: 2m
features:
  deadHosts: false
accessListPasswords:
  "Admin panels":
    alice: "s3cret"
`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cron != "" || cfg.MaxDeletes != 0 || cfg.HTTPTimeout != 2*time.Minute {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.FeatureEnabled(0, FeatureStreams) || cfg.FeatureEnabled(0, FeatureDeadHosts) || !cfg.FeatureEnabled(0, FeatureProxyHosts) {
		t.Fatal("feature overrides not applied")
	}
	if got := strings.Join(cfg.ExcludePatterns(0), ","); got != "internal.example.com,backup-only.example.com" {
		t.Fatalf("exclude patterns = %s (global and per-backup lists are combined)", got)
	}
	if cfg.AccessListPasswords["Admin panels"]["alice"] != "s3cret" {
		t.Fatal("access list passwords not read")
	}
}

func TestEnvOverrides(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(secret, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"MASTER_PASSWORD=env-pass",
		"BACKUP1_URL=http://override.lan:81",
		"BACKUP2_URL=https://second.lan",
		"BACKUP2_USERNAME=bob",
		"BACKUP2_PASSWORD_FILE=" + secret,
		"BACKUP2_FEATURES_PROXY_HOSTS=false",
		"BACKUP2_EXCLUDE_DOMAINS=a.example.com, b.example.com",
		"EXCLUDE_DOMAINS=internal.example.com,*.lab.example.com",
		"CRON=*/5 * * * *",
		"DRY_RUN=true",
		"MAX_DELETES=3",
		"HTTP_TIMEOUT=90s",
		"FEATURES_CERTIFICATES=false",
		"WEB_PORT=0",
		"LOG_LEVEL=debug",
		"ACCESSLIST1_NAME=Admin panels",
		"ACCESSLIST1_USER1_USERNAME=alice",
		"ACCESSLIST1_USER1_PASSWORD_FILE=" + secret,
		"ACCESSLIST1_USER2_USERNAME=carol",
		"ACCESSLIST1_USER2_PASSWORD=pw2",
	}
	cfg, err := Load(writeFile(t, minimal), true, env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Master.Password != "env-pass" || cfg.Master.URL != "http://npm-master.lan:81" {
		t.Error("environment values override the file, other values are kept")
	}
	if len(cfg.Backups) != 2 || cfg.Backups[0].URL != "http://override.lan:81" || cfg.Backups[0].Username != "admin@example.com" {
		t.Fatalf("backups = %+v", cfg.Backups)
	}
	b := cfg.Backups[1]
	if b.Username != "bob" || b.Password != "from-file" || cfg.FeatureEnabled(1, FeatureProxyHosts) || strings.Join(b.ExcludeDomains, "|") != "a.example.com|b.example.com" {
		t.Fatalf("backup 2 = %+v", b)
	}
	if strings.Join(cfg.ExcludeDomains, "|") != "internal.example.com|*.lab.example.com" || cfg.Cron != "*/5 * * * *" || !cfg.DryRun ||
		cfg.MaxDeletes != 3 || cfg.HTTPTimeout != 90*time.Second || cfg.FeatureEnabled(0, FeatureCertificates) || cfg.Web.Port != 0 || cfg.Log.Level != "debug" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.AccessListPasswords["Admin panels"]["alice"] != "from-file" || cfg.AccessListPasswords["Admin panels"]["carol"] != "pw2" {
		t.Fatalf("access lists = %v", cfg.AccessListPasswords)
	}
}

func TestEnvOnlyWithoutFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), false, []string{
		"MASTER_URL=http://m:81", "BACKUP1_URL=http://b:81", "CRON=",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cron != "" {
		t.Fatal("CRON= should disable scheduled runs")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), true, nil); err == nil {
		t.Fatal("an explicitly given config file must exist")
	}
}

func TestEnvErrors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(f, []byte("x"), 0o600)
	cases := map[string][]string{
		"both MASTER_PASSWORD and MASTER_PASSWORD_FILE": {"MASTER_PASSWORD=a", "MASTER_PASSWORD_FILE=" + f},
		"numbering must have no gaps":                   {"BACKUP3_URL=http://x"},
		"unknown feature":                               {"FEATURES_WIDGETS=true"},
		"invalid boolean":                               {"DRY_RUN=maybe"},
		"invalid duration":                              {"HTTP_TIMEOUT=soon"},
	}
	for want, env := range cases {
		_, err := Load(writeFile(t, minimal), true, env)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want %q", env, err, want)
		}
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"missing master":                     `backups: [{url: "http://b:81"}]`,
		"no backups":                         `master: {url: "http://m:81"}`,
		"invalid expression":                 minimal + `cron: "every day"`,
		"scheme must be http or https":       `master: {url: "ftp://m"}` + "\nbackups: [{url: \"http://b:81\"}]",
		`unknown feature "widgets"`:          minimal + "features:\n  widgets: true",
		"excludeDomains: empty pattern":      minimal + `excludeDomains: ["", "a.example.com"]`,
		"field mistake not found":            minimal + "mistake: 1",
		`backups[0].features: unknown`:       `master: {url: "http://m:81"}` + "\nbackups: [{url: \"http://b:81\", features: {nope: true}}]",
		"log.format must be console or json": minimal + "log: {format: xml}",
	}
	for want, yml := range cases {
		_, err := Load(writeFile(t, yml), true, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestMaskedHidesPasswords(t *testing.T) {
	cfg, err := Load(writeFile(t, minimal+"accessListPasswords: {list: {alice: topsecret}}\nweb: {password: webpw}"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cfg.Masked()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"changeme", "topsecret", "webpw"} {
		if strings.Contains(out, secret) {
			t.Errorf("--print-config leaks %q:\n%s", secret, out)
		}
	}
	if cfg.Master.Password != "changeme" {
		t.Fatal("Masked must not modify the config")
	}
}

func TestSameInstance(t *testing.T) {
	cases := []struct {
		master, backup string
		same           bool
	}{
		{"http://npm.lan:81", "http://NPM.lan:81/", true},
		{"http://127.0.0.1:81", "http://localhost:81", true},
		{"https://npm.lan", "https://npm.lan:443", true},
		{"http://127.0.0.1:81", "http://127.0.0.1:82", false},
		{"http://npm.lan:81", "https://npm.lan:81", false},
		{"http://a.invalid:81", "http://b.invalid:81", false},
	}
	for _, c := range cases {
		cfg := &Config{Master: Instance{URL: c.master}, Backups: []Backup{{URL: c.backup}}}
		err := cfg.CheckSameInstance(context.Background(), nil)
		if c.master == "http://127.0.0.1:81" && c.backup == "http://localhost:81" {
			// Needs resolution of localhost.
			err = cfg.CheckSameInstance(context.Background(), netResolver())
		}
		if (err != nil) != c.same {
			t.Errorf("%s vs %s: err = %v, want same=%v", c.master, c.backup, err, c.same)
		}
	}
}

func netResolver() *net.Resolver { return net.DefaultResolver }
