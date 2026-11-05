package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// envSource looks up environment variables, resolving <NAME>_FILE (spec 6.2).
type envSource struct {
	vars map[string]string
}

func newEnvSource(environ []string) (*envSource, error) {
	vars := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			vars[k] = v
		}
	}
	return &envSource{vars: vars}, nil
}

// lookup returns the value of name, or the contents of the file named by
// name_FILE. Setting both is an error.
func (e *envSource) lookup(name string) (string, bool, error) {
	v, ok := e.vars[name]
	path, fok := e.vars[name+"_FILE"]
	if ok && fok {
		return "", false, fmt.Errorf("both %s and %s_FILE are set; use only one", name, name)
	}
	if fok {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", false, fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimRight(string(data), "\r\n"), true, nil
	}
	return v, ok, nil
}

func (e *envSource) str(name string, dst *string) error {
	v, ok, err := e.lookup(name)
	if err != nil || !ok {
		return err
	}
	*dst = v
	return nil
}

func (e *envSource) boolean(name string, dst *bool) error {
	v, ok, err := e.lookup(name)
	if err != nil || !ok {
		return err
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("%s: invalid boolean %q", name, v)
	}
	*dst = b
	return nil
}

func (e *envSource) integer(name string, dst *int) error {
	v, ok, err := e.lookup(name)
	if err != nil || !ok {
		return err
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("%s: invalid integer %q", name, v)
	}
	*dst = n
	return nil
}

func (e *envSource) duration(name string, dst *time.Duration) error {
	v, ok, err := e.lookup(name)
	if err != nil || !ok {
		return err
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("%s: invalid duration %q (for example 60s or 2m)", name, v)
	}
	*dst = d
	return nil
}

func (e *envSource) list(name string, dst *[]string) error {
	v, ok, err := e.lookup(name)
	if err != nil || !ok {
		return err
	}
	var out []string
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	*dst = out
	return nil
}

// features applies <prefix>FEATURES_<NAME> variables to dst, creating the map
// if needed. Unknown feature names are an error.
func (e *envSource) features(prefix string, dst *map[string]bool) error {
	envNames := make(map[string]string, len(AllFeatures))
	for _, f := range AllFeatures {
		envNames[prefix+"FEATURES_"+upperSnake(f)] = f
	}
	for _, key := range e.keysWithPrefix(prefix + "FEATURES_") {
		base := strings.TrimSuffix(key, "_FILE")
		if _, known := envNames[base]; !known {
			return fmt.Errorf("%s: unknown feature (valid: %s)", key, strings.Join(AllFeatures, ", "))
		}
	}
	for name, feature := range envNames {
		var b bool
		v, ok, err := e.lookup(name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if b, err = strconv.ParseBool(strings.TrimSpace(v)); err != nil {
			return fmt.Errorf("%s: invalid boolean %q", name, v)
		}
		if *dst == nil {
			*dst = map[string]bool{}
		}
		(*dst)[feature] = b
	}
	return nil
}

func (e *envSource) keysWithPrefix(prefix string) []string {
	var keys []string
	for k := range e.vars {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// indices returns the sorted distinct numbers n found in variables matching
// re, whose first submatch is n. Numbering must start at 1 and have no gaps.
func (e *envSource) indices(re *regexp.Regexp, what string) ([]int, error) {
	seen := map[int]bool{}
	for k := range e.vars {
		if m := re.FindStringSubmatch(k); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil || n < 1 {
				return nil, fmt.Errorf("%s: numbering starts at 1", k)
			}
			seen[n] = true
		}
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	for i, n := range out {
		if n != i+1 {
			return nil, fmt.Errorf("%s%d_* is set but %s%d_* is not; numbering must have no gaps", what, n, what, i+1)
		}
	}
	return out, nil
}

var (
	backupVarRE     = regexp.MustCompile(`^BACKUP(\d+)_`)
	accessListVarRE = regexp.MustCompile(`^ACCESSLIST(\d+)_`)
)

func applyEnv(cfg *Config, e *envSource) error {
	steps := []func() error{
		func() error { return e.str("MASTER_URL", &cfg.Master.URL) },
		func() error { return e.str("MASTER_USERNAME", &cfg.Master.Username) },
		func() error { return e.str("MASTER_PASSWORD", &cfg.Master.Password) },
		func() error { return e.list("EXCLUDE_DOMAINS", &cfg.ExcludeDomains) },
		func() error { return e.str("CRON", &cfg.Cron) },
		func() error { return e.boolean("RUN_ON_START", &cfg.RunOnStart) },
		func() error { return e.boolean("DRY_RUN", &cfg.DryRun) },
		func() error { return e.boolean("CONTINUE_ON_ERROR", &cfg.ContinueOnError) },
		func() error { return e.boolean("ALLOW_EMPTY_MASTER", &cfg.AllowEmptyMaster) },
		func() error { return e.integer("MAX_DELETES", &cfg.MaxDeletes) },
		func() error { return e.duration("HTTP_TIMEOUT", &cfg.HTTPTimeout) },
		func() error { return e.features("", &cfg.Features) },
		func() error { return e.integer("WEB_PORT", &cfg.Web.Port) },
		func() error { return e.str("WEB_USERNAME", &cfg.Web.Username) },
		func() error { return e.str("WEB_PASSWORD", &cfg.Web.Password) },
		func() error { return e.boolean("WEB_METRICS", &cfg.Web.Metrics) },
		func() error { return e.str("LOG_LEVEL", &cfg.Log.Level) },
		func() error { return e.str("LOG_FORMAT", &cfg.Log.Format) },
		func() error { return applyBackupEnv(cfg, e) },
		func() error { return applyAccessListEnv(cfg, e) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// applyBackupEnv applies BACKUP<n>_* variables. BACKUP<n> overrides the n-th
// backup from the file, or adds it if the file has fewer.
func applyBackupEnv(cfg *Config, e *envSource) error {
	ns, err := e.indices(backupVarRE, "BACKUP")
	if err != nil {
		return err
	}
	for _, n := range ns {
		if n > len(cfg.Backups)+1 {
			return fmt.Errorf("BACKUP%d_* is set but there are only %d backups configured", n, len(cfg.Backups))
		}
		if n == len(cfg.Backups)+1 {
			cfg.Backups = append(cfg.Backups, Backup{})
		}
		b := &cfg.Backups[n-1]
		p := fmt.Sprintf("BACKUP%d_", n)
		for _, step := range []func() error{
			func() error { return e.str(p+"URL", &b.URL) },
			func() error { return e.str(p+"USERNAME", &b.Username) },
			func() error { return e.str(p+"PASSWORD", &b.Password) },
			func() error { return e.list(p+"EXCLUDE_DOMAINS", &b.ExcludeDomains) },
			func() error { return e.features(p, &b.Features) },
		} {
			if err := step(); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyAccessListEnv applies ACCESSLIST<n>_NAME and
// ACCESSLIST<n>_USER<m>_USERNAME / _PASSWORD (spec 4.4).
func applyAccessListEnv(cfg *Config, e *envSource) error {
	ns, err := e.indices(accessListVarRE, "ACCESSLIST")
	if err != nil {
		return err
	}
	for _, n := range ns {
		p := fmt.Sprintf("ACCESSLIST%d_", n)
		var name string
		if err := e.str(p+"NAME", &name); err != nil {
			return err
		}
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%sNAME is required", p)
		}
		userRE := regexp.MustCompile(`^` + p + `USER(\d+)_`)
		ms, err := e.indices(userRE, p+"USER")
		if err != nil {
			return err
		}
		for _, m := range ms {
			up := fmt.Sprintf("%sUSER%d_", p, m)
			var user, pass string
			if err := e.str(up+"USERNAME", &user); err != nil {
				return err
			}
			if err := e.str(up+"PASSWORD", &pass); err != nil {
				return err
			}
			if user == "" {
				return fmt.Errorf("%sUSERNAME is required", up)
			}
			if cfg.AccessListPasswords == nil {
				cfg.AccessListPasswords = map[string]map[string]string{}
			}
			if cfg.AccessListPasswords[name] == nil {
				cfg.AccessListPasswords[name] = map[string]string{}
			}
			cfg.AccessListPasswords[name][user] = pass
		}
	}
	return nil
}

// upperSnake turns "proxyHosts" into "PROXY_HOSTS".
func upperSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' && i > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}
