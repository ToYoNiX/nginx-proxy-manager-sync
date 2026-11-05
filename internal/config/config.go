// Package config loads, merges and validates npm-sync's configuration
// (spec section 6): a YAML file, overridden by environment variables, each of
// which may also be given as <NAME>_FILE for Docker secrets.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// DefaultPath is where the config file is looked for when neither --config
// nor NPM_SYNC_CONFIG is set.
const DefaultPath = "/config/config.yaml"

// Feature names, as used in YAML and (upper-snake-cased) in env variables.
const (
	FeatureCertificates     = "certificates"
	FeatureProxyHosts       = "proxyHosts"
	FeatureAccessLists      = "accessLists"
	FeatureRedirectionHosts = "redirectionHosts"
	FeatureDeadHosts        = "deadHosts"
	FeatureStreams          = "streams"
)

// AllFeatures lists every feature name in a stable order.
var AllFeatures = []string{
	FeatureCertificates,
	FeatureProxyHosts,
	FeatureAccessLists,
	FeatureRedirectionHosts,
	FeatureDeadHosts,
	FeatureStreams,
}

// Instance is one NPM instance (the master or a backup).
type Instance struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Backup is a backup instance with its optional per-backup overrides.
type Backup struct {
	Instance       `yaml:",inline"`
	Features       map[string]bool `yaml:"features,omitempty"`
	ExcludeDomains []string        `yaml:"excludeDomains,omitempty"`
}

// Web configures the web UI and HTTP API.
type Web struct {
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Metrics  bool   `yaml:"metrics"`
}

// Log configures logging.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Config is the effective configuration.
type Config struct {
	Master              Instance                     `yaml:"master"`
	Backups             []Backup                     `yaml:"backups"`
	ExcludeDomains      []string                     `yaml:"excludeDomains"`
	Cron                string                       `yaml:"cron"`
	RunOnStart          bool                         `yaml:"runOnStart"`
	DryRun              bool                         `yaml:"dryRun"`
	ContinueOnError     bool                         `yaml:"continueOnError"`
	AllowEmptyMaster    bool                         `yaml:"allowEmptyMaster"`
	MaxDeletes          int                          `yaml:"maxDeletes"`
	HTTPTimeout         time.Duration                `yaml:"httpTimeout"`
	Features            map[string]bool              `yaml:"features"`
	AccessListPasswords map[string]map[string]string `yaml:"accessListPasswords,omitempty"`
	Web                 Web                          `yaml:"web"`
	Log                 Log                          `yaml:"log"`
}

// Default returns the configuration used for every setting that is not set
// in the file or the environment.
func Default() Config {
	features := make(map[string]bool, len(AllFeatures))
	for _, f := range AllFeatures {
		features[f] = true
	}
	return Config{
		Cron:            "0 */12 * * *",
		RunOnStart:      true,
		ContinueOnError: true,
		MaxDeletes:      10,
		HTTPTimeout:     60 * time.Second,
		Features:        features,
		Web:             Web{Port: 8080},
		Log:             Log{Level: "info", Format: "console"},
	}
}

// Load reads the config file at path (if it exists, or always when required
// is true), applies environment overrides from environ and validates the
// result.
func Load(path string, required bool, environ []string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := decodeYAML(data, &cfg); err != nil {
			return nil, fmt.Errorf("config file %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && !required:
		// Configuration may come entirely from the environment.
	default:
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	env, err := newEnvSource(environ)
	if err != nil {
		return nil, err
	}
	if err := applyEnv(&cfg, env); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func decodeYAML(data []byte, cfg *Config) error {
	// Keep the default feature map when the file sets only some features.
	defaults := cfg.Features
	cfg.Features = nil

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	merged := defaults
	maps.Copy(merged, cfg.Features)
	cfg.Features = merged
	return nil
}

// Validate checks the configuration and returns a message that names the
// first problem found (spec 6.3).
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if c.Master.URL == "" {
		add("master.url is required (missing master)")
	} else if err := checkURL(c.Master.URL); err != nil {
		add("master.url: %v", err)
	}
	if len(c.Backups) == 0 {
		add("at least one backup is required (no backups configured)")
	}
	for i, b := range c.Backups {
		name := fmt.Sprintf("backups[%d]", i)
		if b.URL == "" {
			add("%s.url is required", name)
		} else if err := checkURL(b.URL); err != nil {
			add("%s.url: %v", name, err)
		}
		for f := range b.Features {
			if !isFeature(f) {
				add("%s.features: unknown feature %q (valid: %s)", name, f, strings.Join(AllFeatures, ", "))
			}
		}
		for _, p := range b.ExcludeDomains {
			if strings.TrimSpace(p) == "" {
				add("%s.excludeDomains: empty pattern", name)
			}
		}
	}
	for f := range c.Features {
		if !isFeature(f) {
			add("features: unknown feature %q (valid: %s)", f, strings.Join(AllFeatures, ", "))
		}
	}
	for _, p := range c.ExcludeDomains {
		if strings.TrimSpace(p) == "" {
			add("excludeDomains: empty pattern")
		}
	}
	if c.Cron != "" {
		if _, err := cron.ParseStandard(c.Cron); err != nil {
			add("cron: invalid expression %q: %v", c.Cron, err)
		}
	}
	if c.MaxDeletes < 0 {
		add("maxDeletes must be 0 (no limit) or more")
	}
	if c.HTTPTimeout <= 0 {
		add("httpTimeout must be positive")
	}
	if c.Web.Port < 0 || c.Web.Port > 65535 {
		add("web.port must be between 0 and 65535")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be one of debug, info, warn, error")
	}
	switch strings.ToLower(c.Log.Format) {
	case "console", "json":
	default:
		add("log.format must be console or json")
	}
	for list, users := range c.AccessListPasswords {
		if strings.TrimSpace(list) == "" {
			add("accessListPasswords: empty access list name")
		}
		for user := range users {
			if strings.TrimSpace(user) == "" {
				add("accessListPasswords[%q]: empty username", list)
			}
		}
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// ValidationError lists every configuration problem found.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid configuration:\n  - " + strings.Join(e.Problems, "\n  - ")
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid URL %q: missing host", raw)
	}
	return nil
}

func isFeature(name string) bool {
	return slices.Contains(AllFeatures, name)
}

// FeatureEnabled reports whether a feature is switched on for backup i,
// taking the per-backup override into account (spec section 3).
func (c *Config) FeatureEnabled(i int, feature string) bool {
	if v, ok := c.Backups[i].Features[feature]; ok {
		return v
	}
	if v, ok := c.Features[feature]; ok {
		return v
	}
	return true
}

// ExcludePatterns returns the global and per-backup patterns for backup i,
// combined (spec 3.2).
func (c *Config) ExcludePatterns(i int) []string {
	out := make([]string, 0, len(c.ExcludeDomains)+len(c.Backups[i].ExcludeDomains))
	out = append(out, c.ExcludeDomains...)
	out = append(out, c.Backups[i].ExcludeDomains...)
	return out
}

// Masked returns the configuration as YAML with every password masked, for
// --print-config.
func (c *Config) Masked() (string, error) {
	m := *c
	m.Master.Password = mask(m.Master.Password)
	m.Backups = make([]Backup, len(c.Backups))
	for i, b := range c.Backups {
		b.Password = mask(b.Password)
		m.Backups[i] = b
	}
	m.Web.Password = mask(m.Web.Password)
	if c.AccessListPasswords != nil {
		m.AccessListPasswords = make(map[string]map[string]string, len(c.AccessListPasswords))
		for list, users := range c.AccessListPasswords {
			mu := make(map[string]string, len(users))
			for u, p := range users {
				mu[u] = mask(p)
			}
			m.AccessListPasswords[list] = mu
		}
	}
	out, err := yaml.Marshal(&m)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	return "********"
}
