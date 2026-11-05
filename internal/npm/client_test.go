package npm_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm/npmtest"
)

func newClient(t *testing.T, f *npmtest.Fake) *npm.Client {
	t.Helper()
	c, err := npm.New(f.URL, "admin@example.com", "changeme", 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVersion(t *testing.T) {
	f := npmtest.New()
	defer f.Close()
	c, _ := npm.New(f.URL, "", "", time.Second, nil)
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != (npm.Version{Major: 2, Minor: 16}) {
		t.Fatalf("version = %v", v)
	}
	if !(npm.Version{Major: 2, Minor: 9, Revision: 8}).Less(npm.MinCertificateVersion) || npm.MinCertificateVersion.Less(npm.Version{Major: 2, Minor: 9, Revision: 9}) {
		t.Fatal("version comparison is wrong")
	}
}

func TestLoginTwoFactor(t *testing.T) {
	f := npmtest.New()
	defer f.Close()
	f.TwoFactor = true
	c, _ := npm.New(f.URL, "admin@example.com", "changeme", time.Second, nil)
	err := c.Login(context.Background())
	if !errors.Is(err, npm.ErrTwoFactor) {
		t.Fatalf("err = %v, want ErrTwoFactor", err)
	}
	if !strings.Contains(err.Error(), "use a dedicated NPM user without 2FA") {
		t.Fatalf("message = %q", err)
	}
	if n := strings.Count(strings.Join(f.Requests, "\n"), "POST /api/tokens"); n != 1 {
		t.Fatalf("2FA login attempted %d times, want 1 (no retry)", n)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	f := npmtest.New()
	defer f.Close()
	c, _ := npm.New(f.URL, "admin@example.com", "nope", time.Second, nil)
	if err := c.Login(context.Background()); !npm.IsStatus(err, 401) {
		t.Fatalf("err = %v, want 401", err)
	}
}

func TestReloginOn401(t *testing.T) {
	f := npmtest.New()
	defer f.Close()
	c := newClient(t, f)
	f.ExpireTokens()
	if _, err := c.Hosts(context.Background(), npm.ProxyHosts); err != nil {
		t.Fatalf("expected re-login and retry, got %v", err)
	}
	want := []string{"POST /api/tokens", "GET /api/nginx/proxy-hosts", "POST /api/tokens", "GET /api/nginx/proxy-hosts"}
	if strings.Join(f.Requests, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v", f.Requests)
	}
}

func TestRetryTransient(t *testing.T) {
	for _, status := range []int{502, 503, 504, -1} {
		f := npmtest.New()
		c := newClient(t, f)
		f.Fault = func(method, path string, n int) (int, bool) {
			if method == "GET" && n <= 2 {
				return status, false
			}
			return 0, false
		}
		if _, err := c.Hosts(context.Background(), npm.ProxyHosts); err != nil {
			t.Fatalf("status %d: expected success on third attempt, got %v", status, err)
		}
		f.Close()
	}
}

func TestRetryGivesUpAfterThreeAttempts(t *testing.T) {
	f := npmtest.New()
	defer f.Close()
	c := newClient(t, f)
	var waits []time.Duration
	c.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	f.Fault = func(method, path string, n int) (int, bool) { return 503, false }
	_, err := c.Hosts(context.Background(), npm.ProxyHosts)
	if !npm.IsStatus(err, 503) {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Count(strings.Join(f.Requests, "\n"), "GET /api/nginx/proxy-hosts"); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if len(waits) != 2 || waits[0] < time.Second || waits[0] > 1200*time.Millisecond || waits[1] < 2*time.Second || waits[1] > 2400*time.Millisecond {
		t.Fatalf("waits = %v, want ~1s then ~2s with <=20%% jitter", waits)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	f := npmtest.New()
	defer f.Close()
	c := newClient(t, f)
	f.AddHost("/api/nginx/proxy-hosts", map[string]any{"domain_names": []string{"a.example.com"}})
	_, err := c.CreateHost(context.Background(), npm.ProxyHosts, map[string]any{"domain_names": []any{"a.example.com"}}, sameDomains("a.example.com"))
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.Writes()); n != 1 {
		t.Fatalf("writes = %v, want one POST", f.Writes())
	}
}

func TestDeleteRetry404IsSuccess(t *testing.T) {
	f := npmtest.New()
	defer f.Close()
	c := newClient(t, f)
	id := f.AddHost("/api/nginx/proxy-hosts", map[string]any{"domain_names": []string{"a.example.com"}})
	// The first DELETE succeeds on NPM but the answer is lost.
	f.Fault = func(method, path string, n int) (int, bool) {
		if method == "DELETE" && n == 1 {
			return 504, true
		}
		return 0, false
	}
	if err := c.DeleteHost(context.Background(), npm.ProxyHosts, id); err != nil {
		t.Fatalf("err = %v, want success (404 on retry)", err)
	}
	// A 404 on the first attempt is still an error.
	f.Fault = nil
	if err := c.DeleteHost(context.Background(), npm.ProxyHosts, 999); !npm.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("err = %v, want 404", err)
	}
}

func TestCreateRetryFindsExisting(t *testing.T) {
	f := npmtest.New()
	defer f.Close()
	c := newClient(t, f)
	f.Fault = func(method, path string, n int) (int, bool) {
		if method == "POST" && path == "/api/nginx/proxy-hosts" && n == 1 {
			return 504, true // created, but the answer is lost
		}
		return 0, false
	}
	h, err := c.CreateHost(context.Background(), npm.ProxyHosts, map[string]any{"domain_names": []any{"a.example.com"}, "forward_host": "x"}, sameDomains("a.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if h.ID == 0 || len(f.HostList("/api/nginx/proxy-hosts")) != 1 {
		t.Fatalf("host = %+v, hosts = %v", h, f.HostList("/api/nginx/proxy-hosts"))
	}
	if n := strings.Count(strings.Join(f.Requests, "\n"), "POST /api/nginx/proxy-hosts"); n != 1 {
		t.Fatalf("create sent %d times, want 1", n)
	}
}

func TestCreateRetrySendsAgainIfMissing(t *testing.T) {
	f := npmtest.New()
	defer f.Close()
	c := newClient(t, f)
	f.Fault = func(method, path string, n int) (int, bool) {
		if method == "POST" && path == "/api/nginx/proxy-hosts" && n == 1 {
			return 503, false // not created
		}
		return 0, false
	}
	if _, err := c.CreateHost(context.Background(), npm.ProxyHosts, map[string]any{"domain_names": []any{"a.example.com"}}, sameDomains("a.example.com")); err != nil {
		t.Fatal(err)
	}
	if n := len(f.HostList("/api/nginx/proxy-hosts")); n != 1 {
		t.Fatalf("hosts = %d", n)
	}
}

func TestCertificateDownloadUploadAndMasking(t *testing.T) {
	master, backup := npmtest.New(), npmtest.New()
	defer master.Close()
	defer backup.Close()
	mc, bc := newClient(t, master), newClient(t, backup)
	ctx := context.Background()

	exp := time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Second)
	id := master.AddLetsEncrypt([]string{"example.com", "*.example.com"}, exp)
	files, err := mc.DownloadCertificate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(files.Certificate) == 0 || len(files.Chain) == 0 || len(files.PrivateKey) == 0 {
		t.Fatal("missing files")
	}

	cert, err := bc.CreateCustomCertificate(ctx, "copy [npm-sync:1]", func([]npm.Certificate) (npm.Certificate, bool) { return npm.Certificate{}, false })
	if err != nil {
		t.Fatal(err)
	}
	if err := bc.UploadCertificate(ctx, cert.ID, files); err != nil {
		t.Fatal(err)
	}
	files.Wipe()

	certs, err := bc.Certificates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := certs[0]
	if !got.HasFiles() {
		t.Fatal("uploaded copy should report files")
	}
	for k, v := range got.Meta {
		if _, isString := v.(string); isString {
			t.Fatalf("meta.%s still holds file contents", k)
		}
	}
	e, _ := got.Expires()
	if !e.Equal(exp) {
		t.Fatalf("expires = %v, want %v", e, exp)
	}

	if _, err := mc.DownloadCertificate(ctx, master.AddCustom("custom", exp)); err == nil {
		t.Fatal("downloading a custom certificate should fail")
	}
}

func sameDomains(d string) func(npm.Host) bool {
	return func(h npm.Host) bool { return strings.Join(h.DomainNames, ",") == d }
}
