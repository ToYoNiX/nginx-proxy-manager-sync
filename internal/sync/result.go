package sync

import (
	"time"
)

// Action is what happened (or, in a dry run, would happen) to one object.
type Action string

// Actions shown in logs, the status API and the UI (spec 7.1).
const (
	ActionCreated   Action = "created"
	ActionUpdated   Action = "updated"
	ActionDeleted   Action = "deleted"
	ActionUnchanged Action = "unchanged"
	ActionSkipped   Action = "skipped"
	ActionExcluded  Action = "excluded"
	ActionDegraded  Action = "degraded"
	ActionError     Action = "error"
)

// Object types.
const (
	TypeCertificate     = "certificate"
	TypeProxyHost       = "proxy host"
	TypeRedirectionHost = "redirection host"
	TypeDeadHost        = "404 host"
	TypeStream          = "stream"
)

// ObjectResult is the outcome for one object on one backup.
type ObjectResult struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Action  Action `json:"action"`
	Message string `json:"message,omitempty"`
	// Failed marks a skipped object whose reason is reported as an error,
	// such as a host whose access list cannot be synced (spec 7.2).
	Failed bool `json:"failed,omitempty"`
	// DryRun is set when Action is what would have happened.
	DryRun bool `json:"dryRun,omitempty"`
	// Certificates only.
	ExpiresOn  *time.Time `json:"expiresOn,omitempty"`
	LastCopied *time.Time `json:"lastCopied,omitempty"`
}

// IsError reports whether the object counts against readiness (spec 7.2).
func (o ObjectResult) IsError() bool { return o.Action == ActionError || o.Failed }

// BackupResult is the outcome of one run for one backup.
type BackupResult struct {
	URL      string         `json:"url"`
	Version  string         `json:"version,omitempty"`
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished"`
	Aborted  bool           `json:"aborted"`
	Error    string         `json:"error,omitempty"`
	Objects  []ObjectResult `json:"objects"`
}

// Succeeded reports whether the run succeeded for this backup: it was not
// aborted and no object ended in state error (spec 7.2).
func (b *BackupResult) Succeeded() bool {
	if b.Aborted || b.Error != "" {
		return false
	}
	for _, o := range b.Objects {
		if o.IsError() {
			return false
		}
	}
	return true
}

// Count returns how many objects ended with the given action.
func (b *BackupResult) Count(a Action) int {
	n := 0
	for _, o := range b.Objects {
		if o.Action == a {
			n++
		}
	}
	return n
}

// RunResult is the outcome of one run.
type RunResult struct {
	Trigger        string          `json:"trigger"`
	DryRun         bool            `json:"dryRun"`
	ForcePasswords bool            `json:"forcePasswords,omitempty"`
	Started        time.Time       `json:"started"`
	Finished       time.Time       `json:"finished"`
	MasterURL      string          `json:"masterUrl"`
	MasterVersion  string          `json:"masterVersion,omitempty"`
	Error          string          `json:"error,omitempty"`
	Backups        []*BackupResult `json:"backups"`
}

// Succeeded reports whether every backup succeeded.
func (r *RunResult) Succeeded() bool {
	if r.Error != "" {
		return false
	}
	for _, b := range r.Backups {
		if !b.Succeeded() {
			return false
		}
	}
	return len(r.Backups) > 0
}
