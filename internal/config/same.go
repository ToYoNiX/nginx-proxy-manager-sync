package config

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// CheckSameInstance refuses a configuration in which a backup URL resolves
// to the master URL (spec 4.6): same scheme, port and path, and either the
// same host name or host names that resolve to a shared address.
func (c *Config) CheckSameInstance(ctx context.Context, resolver *net.Resolver) error {
	m, err := endpointOf(ctx, resolver, c.Master.URL)
	if err != nil {
		return err
	}
	for i, b := range c.Backups {
		e, err := endpointOf(ctx, resolver, b.URL)
		if err != nil {
			return err
		}
		if m.same(e) {
			return fmt.Errorf("backups[%d].url %q points to the same NPM instance as master.url %q", i, b.URL, c.Master.URL)
		}
	}
	return nil
}

type endpoint struct {
	scheme, host, port, path string
	addrs                    []string
}

func endpointOf(ctx context.Context, resolver *net.Resolver, raw string) (endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return endpoint{}, err
	}
	e := endpoint{
		scheme: strings.ToLower(u.Scheme),
		host:   strings.ToLower(u.Hostname()),
		port:   u.Port(),
		path:   strings.TrimRight(u.EscapedPath(), "/"),
	}
	if e.port == "" {
		e.port = map[string]string{"http": "80", "https": "443"}[e.scheme]
	}
	if ip := net.ParseIP(e.host); ip != nil {
		e.addrs = []string{ip.String()}
		return e, nil
	}
	if resolver != nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		// Resolution failures are not fatal: the instance may not be up
		// yet. The host names are still compared.
		if ips, err := resolver.LookupIP(ctx, "ip", e.host); err == nil {
			for _, ip := range ips {
				e.addrs = append(e.addrs, ip.String())
			}
			sort.Strings(e.addrs)
		}
	}
	return e, nil
}

func (a endpoint) same(b endpoint) bool {
	if a.scheme != b.scheme || a.port != b.port || a.path != b.path {
		return false
	}
	if a.host == b.host {
		return true
	}
	for _, x := range a.addrs {
		if slices.Contains(b.addrs, x) {
			return true
		}
	}
	return false
}
