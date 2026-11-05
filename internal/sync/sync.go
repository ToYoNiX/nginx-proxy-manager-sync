// Package sync brings backup NPM instances in line with the master.
//
// Planning (certs.go, hosts.go) is pure: master state + backup state ->
// actions. This file executes the plan against the NPM API and records the
// result of every object (spec section 4).
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/config"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
)

// Syncer runs syncs. It is not safe for concurrent runs; the scheduler
// guarantees one run at a time.
type Syncer struct {
	cfg *config.Config
	log *slog.Logger
	// Now returns the current time; replaced in tests.
	Now func() time.Time
	// NewClient builds an NPM client; replaced in tests to shorten retries.
	NewClient func(inst config.Instance) (*npm.Client, error)
}

// New returns a Syncer for cfg.
func New(cfg *config.Config, log *slog.Logger) *Syncer {
	s := &Syncer{cfg: cfg, log: log, Now: time.Now}
	s.NewClient = func(inst config.Instance) (*npm.Client, error) {
		return npm.New(inst.URL, inst.Username, inst.Password, cfg.HTTPTimeout, log)
	}
	return s
}

// hostSpecs are the synced host types, in pass order.
var hostSpecs = []*HostSpec{&ProxyHostSpec, &RedirectionHostSpec, &DeadHostSpec, &StreamSpec}

// RunOptions changes one run.
type RunOptions struct {
	// ForcePasswords sends every configured access list password, for
	// changes the password hint cannot detect (spec 4.4).
	ForcePasswords bool
}

type masterState struct {
	version      npm.Version
	certsAllowed bool
	certsErr     string
	client       *npm.Client
	certs        []npm.Certificate
	hosts        map[*HostSpec][]npm.Host
	accessLists  []npm.AccessList
	alRead       bool
}

// Run performs one sync run.
func (s *Syncer) Run(ctx context.Context, trigger string, opts RunOptions) *RunResult {
	res := &RunResult{Trigger: trigger, DryRun: s.cfg.DryRun, ForcePasswords: opts.ForcePasswords, Started: s.Now(), MasterURL: s.cfg.Master.URL}
	defer func() { res.Finished = s.Now() }()

	log := s.log.With("run", trigger)
	if s.cfg.DryRun {
		log.Info("dry run: nothing will be written")
	}
	log.Info("sync run started", "master", s.cfg.Master.URL, "backups", len(s.cfg.Backups), "forcePasswords", opts.ForcePasswords)

	ms, err := s.readMaster(ctx)
	if ms != nil {
		// Custom certificate files from the master's listing are held for
		// this run only.
		defer npm.WipeFiles(ms.certs)
	}
	if err != nil {
		res.Error = "master: " + err.Error()
		log.Error("sync run aborted: cannot read the master", "master", s.cfg.Master.URL, "error", err)
		for _, b := range s.cfg.Backups {
			res.Backups = append(res.Backups, &BackupResult{URL: b.URL, Started: res.Started, Finished: s.Now(), Aborted: true, Error: res.Error})
		}
		return res
	}
	res.MasterVersion = ms.version.String()

	for i := range s.cfg.Backups {
		if ctx.Err() != nil {
			res.Backups = append(res.Backups, &BackupResult{URL: s.cfg.Backups[i].URL, Aborted: true, Error: "run cancelled"})
			continue
		}
		br := s.syncBackup(ctx, i, ms, opts)
		res.Backups = append(res.Backups, br)
	}

	for _, br := range res.Backups {
		attrs := []any{"backup", br.URL,
			"created", br.Count(ActionCreated), "updated", br.Count(ActionUpdated), "deleted", br.Count(ActionDeleted),
			"unchanged", br.Count(ActionUnchanged), "skipped", br.Count(ActionSkipped), "excluded", br.Count(ActionExcluded),
			"degraded", br.Count(ActionDegraded), "errors", countErrors(br)}
		switch {
		case br.Aborted:
			log.Error("backup run aborted", append(attrs, "reason", br.Error)...)
		case !br.Succeeded():
			log.Warn("backup run finished with errors", attrs...)
		default:
			log.Info("backup run finished", attrs...)
		}
	}
	log.Info("sync run finished", "success", res.Succeeded(), "duration", s.Now().Sub(res.Started).Round(time.Millisecond))
	return res
}

func countErrors(br *BackupResult) int {
	n := 0
	for _, o := range br.Objects {
		if o.IsError() {
			n++
		}
	}
	return n
}

func (s *Syncer) readMaster(ctx context.Context) (*masterState, error) {
	c, err := s.NewClient(s.cfg.Master)
	if err != nil {
		return nil, err
	}
	ms := &masterState{client: c, certsAllowed: true, hosts: map[*HostSpec][]npm.Host{}}
	if ms.version, err = c.Version(ctx); err != nil {
		return nil, fmt.Errorf("reading version: %w", err)
	}
	if ms.version.Less(npm.MinCertificateVersion) {
		ms.certsAllowed = false
		ms.certsErr = fmt.Sprintf("certificate sync refused: master runs NPM %s, older than the minimum %s", ms.version, npm.MinCertificateVersion)
		s.log.Error(ms.certsErr)
	}
	if err := c.Login(ctx); err != nil {
		return nil, err
	}
	if ms.certs, err = c.Certificates(ctx); err != nil {
		return ms, fmt.Errorf("reading certificates: %w", err)
	}
	for _, spec := range hostSpecs {
		hosts, err := c.Hosts(ctx, spec.Kind)
		if err != nil {
			return ms, fmt.Errorf("reading %ss: %w", spec.Kind.Name, err)
		}
		ms.hosts[spec] = hosts
	}
	for i := range s.cfg.Backups {
		if s.cfg.FeatureEnabled(i, config.FeatureAccessLists) {
			if ms.accessLists, err = c.AccessLists(ctx); err != nil {
				return ms, fmt.Errorf("reading access lists: %w", err)
			}
			ms.alRead = true
			break
		}
	}
	s.log.Debug("master read", "version", ms.version.String(), "certificates", len(ms.certs), "proxyHosts", len(ms.hosts[&ProxyHostSpec]))
	return ms, nil
}

// backupRun holds the state of one backup's run.
type backupRun struct {
	s       *Syncer
	i       int
	ms      *masterState
	c       *npm.Client
	res     *BackupResult
	log     *slog.Logger
	dry     bool
	stopped bool // continueOnError is false and an object failed

	certsOn bool
	alOn    bool
	hostsOn map[*HostSpec]bool
	opts    RunOptions

	// reloaded is set once a host write has made NPM reload nginx.
	reloaded bool
	// refreshed holds backup certificate IDs whose files were replaced.
	refreshed map[int]bool
}

var errStopped = errors.New("stopped after an error because continueOnError is false")

func (s *Syncer) syncBackup(ctx context.Context, i int, ms *masterState, opts RunOptions) *BackupResult {
	b := s.cfg.Backups[i]
	res := &BackupResult{URL: b.URL, Started: s.Now()}
	defer func() { res.Finished = s.Now() }()
	run := &backupRun{s: s, i: i, ms: ms, res: res, dry: s.cfg.DryRun, refreshed: map[int]bool{}, opts: opts,
		log: s.log.With("backup", b.URL), hostsOn: map[*HostSpec]bool{}}

	if err := run.sync(ctx); err != nil {
		res.Aborted = true
		res.Error = err.Error()
	}
	return res
}

// abort returns the error that aborts this backup's run. It is logged once,
// in the run summary.
func (r *backupRun) abort(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// record stores an object result and logs it.
func (r *backupRun) record(o ObjectResult) {
	o.DryRun = r.dry && o.Action != ActionUnchanged && o.Action != ActionExcluded && o.Action != ActionError && !o.Failed
	r.res.Objects = append(r.res.Objects, o)
	attrs := []any{"type", o.Type, "name", o.Name, "action", string(o.Action)}
	if o.Message != "" {
		attrs = append(attrs, "message", o.Message)
	}
	msg := string(o.Action)
	if o.DryRun {
		msg = "dry run: would be " + msg
	}
	switch {
	case o.IsError():
		r.log.Error(msg, attrs...)
	case o.Action == ActionDegraded:
		r.log.Warn(msg, attrs...)
	case o.Action == ActionUnchanged || o.Action == ActionExcluded:
		r.log.Debug(msg, attrs...)
	default:
		r.log.Info(msg, attrs...)
	}
}

// fail records an object error. With continueOnError false it returns
// errStopped, which aborts the backup's run.
func (r *backupRun) fail(o ObjectResult) error {
	o.Action = ActionError
	r.record(o)
	if !r.s.cfg.ContinueOnError {
		r.stopped = true
		return errStopped
	}
	return nil
}

func (r *backupRun) sync(ctx context.Context) error {
	cfg := r.s.cfg
	b := cfg.Backups[r.i]

	c, err := r.s.NewClient(b.Instance)
	if err != nil {
		return r.abort("%v", err)
	}
	r.c = c

	// Version (spec 4.9).
	v, err := c.Version(ctx)
	if err != nil {
		return r.abort("reading version: %v", err)
	}
	r.res.Version = v.String()
	if v != r.ms.version {
		r.log.Warn("backup runs a different NPM version than the master; field sets can differ between releases",
			"backupVersion", v.String(), "masterVersion", r.ms.version.String())
	}

	if err := c.Login(ctx); err != nil {
		return r.abort("%v", err)
	}

	r.certsOn = cfg.FeatureEnabled(r.i, config.FeatureCertificates)
	certsRefused := ""
	if r.certsOn && !r.ms.certsAllowed {
		certsRefused = r.ms.certsErr
	} else if r.certsOn && v.Less(npm.MinCertificateVersion) {
		certsRefused = fmt.Sprintf("certificate sync refused: backup runs NPM %s, older than the minimum %s", v, npm.MinCertificateVersion)
	}
	if certsRefused != "" {
		r.certsOn = false
		r.record(ObjectResult{Type: TypeCertificate, Name: "(all)", Action: ActionError, Message: certsRefused})
	}
	for _, spec := range hostSpecs {
		r.hostsOn[spec] = cfg.FeatureEnabled(r.i, spec.Feature)
	}
	r.alOn = cfg.FeatureEnabled(r.i, config.FeatureAccessLists) && r.ms.alRead

	// Read the backup.
	backupCerts, err := c.Certificates(ctx)
	if err != nil {
		return r.abort("reading certificates: %v", err)
	}
	npm.WipeFiles(backupCerts) // only the hashes are needed
	var backupLists []npm.AccessList
	if r.alOn {
		if backupLists, err = c.AccessLists(ctx); err != nil {
			return r.abort("reading access lists: %v", err)
		}
	}
	backupHosts := map[*HostSpec][]npm.Host{}
	for _, spec := range hostSpecs {
		if !r.hostsOn[spec] {
			continue
		}
		if backupHosts[spec], err = c.Hosts(ctx, spec.Kind); err != nil {
			return r.abort("reading %ss: %v", spec.Kind.Name, err)
		}
	}

	now := r.s.Now()
	excluder := NewExcluder(cfg.ExcludePatterns(r.i))
	certPlan := PlanCertificates(r.ms.certs, backupCerts, now, r.certsOn)

	var alPlan ALPlan
	if r.alOn {
		alPlan = PlanAccessLists(r.ms.accessLists, backupLists, cfg.AccessListPasswords, r.opts.ForcePasswords)
	}

	env := HostEnv{
		CertMap:                  certPlan.Usable,
		MasterCustom:             certPlan.MasterCustom,
		MasterCerts:              certPlan.MasterByID,
		CertificatesOff:          !r.certsOn,
		AccessListMap:            map[int]int{},
		AccessListProblem:        map[int]string{},
		DefaultAccessListProblem: "the access list was not synced",
		Excluder:                 excluder,
	}
	if !r.alOn {
		env.DefaultAccessListProblem = "access list sync (accessLists) is switched off"
	}

	// Safety checks (spec 4.6), before anything is written.
	prePlan := map[*HostSpec][]HostAction{}
	deletes := 0
	for _, spec := range hostSpecs {
		if !r.hostsOn[spec] {
			continue
		}
		prePlan[spec] = PlanHosts(spec, r.ms.hosts[spec], backupHosts[spec], env)
		for _, a := range prePlan[spec] {
			if a.Op == HostDelete {
				deletes++
			}
		}
	}
	deletes += len(certPlan.Cleanup) + len(alPlan.Cleanup)
	if err := r.checkEmptyMaster(certPlan, backupCerts, backupLists, backupHosts, excluder); err != nil {
		return err
	}
	if cfg.MaxDeletes > 0 && deletes > cfg.MaxDeletes {
		return r.abort("safety check: this run would delete %d objects, more than maxDeletes (%d); raise maxDeletes for one run or check the plan with a dry run first", deletes, cfg.MaxDeletes)
	}

	// Certificates (spec 4.2).
	for _, k := range certPlan.Kept {
		r.record(k)
	}
	for _, e := range certPlan.Errors {
		if err := r.fail(e); err != nil {
			return r.abort("%v", err)
		}
	}
	certMap := make(map[int]int, len(certPlan.Usable))
	maps.Copy(certMap, certPlan.Usable)
	for _, a := range certPlan.Actions {
		if err := r.syncCertificate(ctx, a, certMap); err != nil {
			return r.abort("%v", err)
		}
	}
	env.CertMap = certMap

	// Access lists (spec 4.4).
	if r.alOn {
		if err := r.syncAccessLists(ctx, alPlan, env); err != nil {
			return r.abort("%v", err)
		}
	}

	// Hosts: delete, update, create, each across all host types (spec 4.1).
	plans := map[*HostSpec][]HostAction{}
	for _, spec := range hostSpecs {
		if r.hostsOn[spec] {
			plans[spec] = PlanHosts(spec, r.ms.hosts[spec], backupHosts[spec], env)
		}
	}
	excludedBackup := excludedBackupHosts(plans)
	for _, op := range []HostOp{HostSkip, HostExcluded, HostUnchanged, HostDelete, HostUpdate, HostCreate} {
		for _, spec := range hostSpecs {
			for _, a := range plans[spec] {
				if a.Op != op {
					continue
				}
				if err := r.applyHost(ctx, a, excludedBackup); err != nil {
					return r.abort("%v", err)
				}
			}
		}
	}

	// Reload nginx for refreshed certificates (spec 4.2.1 step 4), then
	// clean up. Both need the backup's hosts as they are now.
	if r.dry {
		r.dryCleanup(certPlan, alPlan)
		return nil
	}
	refs, alRefs, hostsNow, err := r.readReferences(ctx)
	if err != nil {
		r.record(ObjectResult{Type: TypeCertificate, Name: "(cleanup)", Action: ActionError,
			Message: "cleanup skipped: cannot read the backup's hosts to check references: " + err.Error()})
		return nil
	}
	if err := r.reloadIfNeeded(ctx, hostsNow, excluder); err != nil {
		return r.abort("%v", err)
	}
	if err := r.cleanupCertificates(ctx, certPlan, refs); err != nil {
		return err
	}
	return r.cleanupAccessLists(ctx, alPlan, alRefs)
}

// checkEmptyMaster aborts if the master returns zero objects of a switched-on
// type while the backup has some (spec 4.6).
func (r *backupRun) checkEmptyMaster(cp CertPlan, backupCerts []npm.Certificate, backupLists []npm.AccessList, backupHosts map[*HostSpec][]npm.Host, ex Excluder) error {
	if r.s.cfg.AllowEmptyMaster {
		return nil
	}
	if r.alOn && len(r.ms.accessLists) == 0 && len(backupLists) > 0 {
		return r.abort("safety check: the master has no access lists but this backup has %d; set allowEmptyMaster to allow this", len(backupLists))
	}
	if r.certsOn && len(r.ms.certs) == 0 {
		managed := 0
		for _, c := range backupCerts {
			if _, ok := ParseCertTag(c.NiceName); ok {
				managed++
			}
		}
		if managed > 0 {
			return r.abort("safety check: the master has no certificates but this backup has %d managed copies; set allowEmptyMaster to allow this", managed)
		}
	}
	for _, spec := range hostSpecs {
		if !r.hostsOn[spec] {
			continue
		}
		if countNotExcluded(spec, r.ms.hosts[spec], ex) == 0 {
			if n := countNotExcluded(spec, backupHosts[spec], ex); n > 0 {
				return r.abort("safety check: the master has no %ss but this backup has %d; set allowEmptyMaster to allow this", spec.Kind.Name, n)
			}
		}
	}
	return nil
}

func countNotExcluded(spec *HostSpec, hosts []npm.Host, ex Excluder) int {
	n := 0
	for _, h := range hosts {
		if _, ok := spec.excluded(ex, h); !ok {
			n++
		}
	}
	return n
}

// syncCertificate creates or refreshes one copy (spec 4.2.1). certMap is
// updated with the usable copy, if any.
func (r *backupRun) syncCertificate(ctx context.Context, a CertAction, certMap map[int]int) error {
	m := a.Master
	o := ObjectResult{Type: TypeCertificate, Name: m.NiceName}
	if exp, err := m.Expires(); err == nil {
		o.ExpiresOn = &exp
	}

	if a.Op == CertUnchanged {
		o.Action = ActionUnchanged
		if t, err := npm.ParseTime(a.Copy.ModifiedOn); err == nil {
			o.LastCopied = &t // NPM updates modified_on on every upload
		}
		r.record(o)
		return nil
	}
	if r.dry {
		o.Message = a.Reason
		if a.Op == CertCreate {
			o.Action = ActionCreated
			certMap[m.ID] = -m.ID // placeholder: the copy that would be created
		} else {
			o.Action = ActionUpdated
			certMap[m.ID] = a.Copy.ID
		}
		r.record(o)
		return nil
	}

	var files npm.CertFiles
	if a.Custom() {
		// Custom certificates: the files come from the master's listing.
		// Each backup gets its own copy, wiped after the upload.
		files = npm.CertFiles{
			Certificate: append([]byte(nil), m.Files.Certificate...),
			Chain:       append([]byte(nil), m.Files.Chain...),
			PrivateKey:  append([]byte(nil), m.Files.PrivateKey...),
		}
	} else {
		var err error
		if files, err = r.ms.client.DownloadCertificate(ctx, m.ID); err != nil {
			o.Message = fmt.Sprintf("downloading master certificate #%d: %v", m.ID, err)
			return r.fail(o)
		}
	}
	defer files.Wipe()

	tag := CertTag(m.NiceName, m.ID)
	target := 0
	if a.Copy != nil {
		target = a.Copy.ID
	} else {
		created, err := r.c.CreateCustomCertificate(ctx, tag, func(certs []npm.Certificate) (npm.Certificate, bool) {
			var best *npm.Certificate
			for i := range certs {
				if id, ok := ParseCertTag(certs[i].NiceName); ok && id == m.ID && (best == nil || certs[i].ID < best.ID) {
					best = &certs[i]
				}
			}
			if best == nil {
				return npm.Certificate{}, false
			}
			return *best, true
		})
		if err != nil {
			o.Message = "creating copy: " + err.Error()
			return r.fail(o)
		}
		target = created.ID
	}

	if err := r.c.UploadCertificate(ctx, target, files); err != nil {
		o.Message = fmt.Sprintf("uploading to backup certificate #%d: %v", target, err)
		if a.Copy != nil {
			if id, ok := certMap[m.ID]; ok {
				o.Message += fmt.Sprintf("; hosts keep using the copy from an earlier run (#%d) while it is usable", id)
			}
		}
		return r.fail(o)
	}

	copied := r.s.Now()
	o.LastCopied = &copied
	certMap[m.ID] = target
	if a.Op == CertCreate {
		o.Action = ActionCreated
		o.Message = fmt.Sprintf("copied to backup certificate #%d", target)
	} else {
		o.Action = ActionUpdated
		o.Message = fmt.Sprintf("refreshed backup certificate #%d: %s", target, a.Reason)
		r.refreshed[target] = true
	}
	r.record(o)
	return nil
}

func excludedBackupHosts(plans map[*HostSpec][]HostAction) []HostAction {
	var out []HostAction
	for _, actions := range plans {
		for _, a := range actions {
			if a.Op == HostExcluded && a.BackupID > 0 {
				out = append(out, a)
			}
		}
	}
	return out
}

// applyHost executes one host action and records its result.
func (r *backupRun) applyHost(ctx context.Context, a HostAction, excluded []HostAction) error {
	o := ObjectResult{Type: a.Spec.Type, Name: a.Name(), Message: a.Message}
	switch a.Op {
	case HostSkip:
		o.Action, o.Failed = ActionSkipped, a.Failed
		r.record(o)
		return nil
	case HostExcluded:
		o.Action = ActionExcluded
		r.record(o)
		return nil
	case HostUnchanged:
		o.Action = ActionUnchanged
		if a.Degraded != "" {
			o.Action, o.Message = ActionDegraded, a.Degraded
		}
		r.record(o)
		return nil
	}

	if a.CertificateID < 0 { // dry run: copy that would be created
		o.Message = strings.TrimSpace(o.Message + " (would use the new certificate copy)")
	}

	var err error
	switch a.Op {
	case HostDelete:
		o.Action = ActionDeleted
		if !r.dry {
			err = r.c.DeleteHost(ctx, a.Spec.Kind, a.BackupID)
		}
	case HostUpdate:
		o.Action = ActionUpdated
		if !r.dry && a.PayloadChanged {
			_, err = r.c.UpdateHost(ctx, a.Spec.Kind, a.BackupID, a.Payload)
		}
		if err == nil && !r.dry && a.Toggle {
			err = r.c.SetHostEnabled(ctx, a.Spec.Kind, a.BackupID, a.Enabled)
		}
		if !a.PayloadChanged && a.Toggle {
			o.Message = strings.TrimSpace(o.Message + " " + map[bool]string{true: "enabled", false: "disabled"}[a.Enabled])
		}
	case HostCreate:
		o.Action = ActionCreated
		if !r.dry {
			want := a.Spec.Key(npm.Host{DomainNames: a.Domains, Fields: a.Payload})
			var created npm.Host
			created, err = r.c.CreateHost(ctx, a.Spec.Kind, a.Payload, func(h npm.Host) bool { return a.Spec.Key(h) == want })
			if err == nil && a.Toggle {
				err = r.c.SetHostEnabled(ctx, a.Spec.Kind, created.ID, false)
				if err != nil {
					err = fmt.Errorf("created, but disabling it failed: %w", err)
				}
			}
		}
	}
	if err != nil {
		o.Message = fmt.Sprintf("%s failed: %v", map[HostOp]string{HostDelete: "delete", HostUpdate: "update", HostCreate: "create"}[a.Op], err)
		if a.Op == HostCreate && strings.Contains(err.Error(), "already in use") {
			if holder := collision(a, excluded); holder != "" {
				o.Message += "; " + holder
			}
		}
		return r.fail(o)
	}
	if a.Op == HostDelete || a.Enabled || a.Toggle {
		r.reloaded = true
	}
	if a.Degraded != "" && a.Op != HostDelete {
		o.Message = fmt.Sprintf("%s; %s", o.Action, a.Degraded)
		o.Action = ActionDegraded
	}
	r.record(o)
	return nil
}

// collision names the excluded backup host that holds one of a's domains.
func collision(a HostAction, excluded []HostAction) string {
	want := map[string]bool{}
	for _, d := range a.Domains {
		want[strings.ToLower(d)] = true
	}
	for _, e := range excluded {
		for _, d := range e.Domains {
			if want[strings.ToLower(d)] {
				return fmt.Sprintf("the domain %s is held by excluded backup %s #%d (%s)", d, e.Spec.Kind.Name, e.BackupID, e.Name())
			}
		}
	}
	return ""
}

// readReferences reads every host type on the backup and returns the sets
// of referenced certificate and access list IDs (including access lists of
// proxy host locations), and the hosts of the synced types.
func (r *backupRun) readReferences(ctx context.Context) (certRefs, alRefs map[int]bool, synced map[*HostSpec][]npm.Host, err error) {
	certRefs, alRefs, synced = map[int]bool{}, map[int]bool{}, map[*HostSpec][]npm.Host{}
	for _, spec := range hostSpecs {
		kind := spec.Kind
		hosts, err := r.c.Hosts(ctx, kind)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("reading %ss: %w", kind.Name, err)
		}
		for _, h := range hosts {
			if id := npm.IntField(h.Fields, "certificate_id"); id > 0 {
				certRefs[id] = true
			}
			if id := npm.IntField(h.Fields, "access_list_id"); id > 0 {
				alRefs[id] = true
			}
			if locs, ok := h.Fields["locations"].([]any); ok {
				for _, l := range locs {
					if loc, ok := l.(map[string]any); ok {
						if id := npm.IntField(loc, "access_list_id"); id > 0 {
							alRefs[id] = true
						}
					}
				}
			}
		}
		synced[spec] = hosts
	}
	return certRefs, alRefs, synced, nil
}

// reloadIfNeeded makes NPM reload nginx when certificate files were replaced
// and no host write in this run already did (spec 4.2.1 step 4).
func (r *backupRun) reloadIfNeeded(ctx context.Context, hosts map[*HostSpec][]npm.Host, ex Excluder) error {
	if len(r.refreshed) == 0 || r.reloaded {
		return nil
	}
	for _, spec := range hostSpecs {
		for _, h := range hosts[spec] {
			if !h.Enabled() || !r.refreshed[npm.IntField(h.Fields, "certificate_id")] {
				continue
			}
			if _, excluded := spec.excluded(ex, h); excluded {
				continue
			}
			name := strings.Join(h.DomainNames, ", ")
			if _, err := r.c.UpdateHost(ctx, spec.Kind, h.ID, currentPayload(spec, h)); err != nil {
				return r.fail(ObjectResult{Type: spec.Type, Name: name,
					Message: "re-saving host to reload nginx for refreshed certificates failed: " + err.Error()})
			}
			r.log.Info("re-saved host to reload nginx for refreshed certificates", "type", spec.Type, "name", name)
			return nil
		}
	}
	r.log.Debug("no enabled host uses a refreshed certificate; no reload needed")
	return nil
}

// cleanupCertificates deletes managed copies that are no longer needed,
// unless a host on the backup still references them (spec 4.1 step 6).
func (r *backupRun) cleanupCertificates(ctx context.Context, cp CertPlan, refs map[int]bool) error {
	for _, cl := range cp.Cleanup {
		o := ObjectResult{Type: TypeCertificate, Name: cl.Cert.NiceName}
		if refs[cl.Cert.ID] {
			o.Action = ActionSkipped
			o.Message = cl.Reason + ", but a host on the backup still uses it; retried on the next run"
			r.record(o)
			continue
		}
		if err := r.c.DeleteCertificate(ctx, cl.Cert.ID); err != nil {
			o.Message = "delete failed: " + err.Error()
			if ferr := r.fail(o); ferr != nil {
				return r.abort("%v", ferr)
			}
			continue
		}
		o.Action, o.Message = ActionDeleted, cl.Reason
		r.record(o)
	}
	return nil
}

// dryCleanup reports what cleanup would do.
func (r *backupRun) dryCleanup(cp CertPlan, ap ALPlan) {
	for _, cl := range cp.Cleanup {
		r.record(ObjectResult{Type: TypeCertificate, Name: cl.Cert.NiceName, Action: ActionDeleted,
			Message: cl.Reason + " (unless a host still uses it)"})
	}
	for _, cl := range ap.Cleanup {
		r.record(ObjectResult{Type: TypeAccessList, Name: cl.List.Name, Action: ActionDeleted,
			Message: cl.Reason + " (unless a host still uses it)"})
	}
}

// syncAccessLists creates and updates access lists and fills env's access
// list mapping and problems for host planning.
func (r *backupRun) syncAccessLists(ctx context.Context, plan ALPlan, env HostEnv) error {
	for _, w := range plan.Warnings {
		r.log.Warn(w)
	}
	maps.Copy(env.AccessListProblem, plan.Problems)
	for _, e := range plan.Errors {
		if err := r.fail(e); err != nil {
			return err
		}
	}
	for _, a := range plan.Actions {
		o := ObjectResult{Type: TypeAccessList, Name: a.Master.Name, Message: a.Reason}
		switch a.Op {
		case ALSkip:
			o.Action, o.Failed = ActionSkipped, true
			if a.BackupID > 0 {
				o.Message += "; the backup's list with this name is left as it is"
			}
			r.record(o)
			continue
		case ALUnchanged:
			env.AccessListMap[a.Master.ID] = a.BackupID
			o.Action = ActionUnchanged
			r.record(o)
			continue
		}

		var err error
		id := a.BackupID
		if a.Op == ALCreate {
			o.Action = ActionCreated
			if r.dry {
				id = -a.Master.ID
			} else {
				var created npm.AccessList
				created, err = r.c.CreateAccessList(ctx, a.Payload)
				id = created.ID
			}
		} else {
			o.Action = ActionUpdated
			if !r.dry {
				err = r.c.UpdateAccessList(ctx, a.BackupID, a.Payload)
			}
		}
		if err != nil {
			env.AccessListProblem[a.Master.ID] = "syncing the access list failed: " + err.Error()
			o.Message = fmt.Sprintf("%s failed: %v", map[ALOp]string{ALCreate: "create", ALUpdate: "update"}[a.Op], err)
			if ferr := r.fail(o); ferr != nil {
				return ferr
			}
			continue
		}
		env.AccessListMap[a.Master.ID] = id
		r.record(o)
	}
	return nil
}

// cleanupAccessLists deletes access lists that are not on the master,
// unless a host on the backup still uses them: NPM would silently remove
// the protection from those hosts (spec 4.1 step 6, 4.4).
func (r *backupRun) cleanupAccessLists(ctx context.Context, plan ALPlan, refs map[int]bool) error {
	for _, cl := range plan.Cleanup {
		o := ObjectResult{Type: TypeAccessList, Name: cl.List.Name}
		if refs[cl.List.ID] {
			o.Action = ActionSkipped
			o.Message = cl.Reason + ", but a host on the backup still uses it; retried on the next run"
			r.record(o)
			continue
		}
		if err := r.c.DeleteAccessList(ctx, cl.List.ID); err != nil {
			o.Message = "delete failed: " + err.Error()
			if ferr := r.fail(o); ferr != nil {
				return r.abort("%v", ferr)
			}
			continue
		}
		o.Action, o.Message = ActionDeleted, cl.Reason
		r.record(o)
	}
	return nil
}
