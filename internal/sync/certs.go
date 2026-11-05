package sync

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
)

var tagRE = regexp.MustCompile(`\s*\[npm-sync:(\d+)\]\s*$`)

// CertTag returns the nice_name for a backup copy of a master certificate
// (spec 4.2.2): "<master nice_name> [npm-sync:<master id>]".
func CertTag(masterNiceName string, masterID int) string {
	return fmt.Sprintf("%s [npm-sync:%d]", masterNiceName, masterID)
}

// ParseCertTag returns the master certificate ID from a backup copy's
// nice_name, if it carries an npm-sync tag.
func ParseCertTag(niceName string) (int, bool) {
	m := tagRE.FindStringSubmatch(niceName)
	if m == nil {
		return 0, false
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return id, true
}

// CertOp is what to do with one master certificate on a backup.
type CertOp int

// Certificate operations.
const (
	CertUnchanged CertOp = iota // usable copy with the master's expiry
	CertCreate                  // no copy yet: create and upload
	CertRefresh                 // copy exists but is not usable or outdated: upload again
)

func (o CertOp) String() string {
	switch o {
	case CertCreate:
		return "create"
	case CertRefresh:
		return "refresh"
	default:
		return "unchanged"
	}
}

// CertAction is the planned action for one master certificate that can be
// copied: Let's Encrypt certificates (files from the download endpoint) and
// custom certificates whose files NPM returned in its listing.
type CertAction struct {
	Op     CertOp
	Master npm.Certificate
	// Copy is the backup copy in use (lowest ID with the tag), if any.
	Copy   *npm.Certificate
	Reason string
}

// Custom reports whether the master certificate is a custom one, copied from
// the files in NPM's listing rather than downloaded.
func (a CertAction) Custom() bool { return a.Master.Provider != npm.ProviderLetsEncrypt }

// CertCleanup is a managed backup certificate to delete during cleanup.
type CertCleanup struct {
	Cert   npm.Certificate
	Reason string
}

// CertPlan is the certificate part of a backup's plan.
type CertPlan struct {
	Actions []CertAction
	// Errors are master certificates that cannot be copied (expired).
	Errors []ObjectResult
	// Kept are custom master certificates whose files NPM no longer
	// returns but that still have a usable copy: hosts keep using it, but it
	// cannot be refreshed.
	Kept []ObjectResult
	// Usable maps master certificate ID to the backup ID of a usable
	// copy, as the backup is before this run writes anything.
	Usable map[int]int
	// Cleanup lists managed copies to delete if no host references them.
	Cleanup []CertCleanup
	// MasterCustom is the set of master certificate IDs that are custom
	// and cannot be copied because NPM did not return their files.
	MasterCustom map[int]npm.Certificate
	// MasterByID indexes every master certificate.
	MasterByID map[int]npm.Certificate
}

// Usable reports whether a backup copy can be referenced by hosts: both
// files present and not expired (spec 4.2.2).
func Usable(c npm.Certificate, now time.Time) bool {
	if !c.HasFiles() {
		return false
	}
	exp, err := c.Expires()
	return err == nil && exp.After(now)
}

// sameExpiry compares two expires_on values as instants, falling back to the
// raw strings if either cannot be parsed.
func sameExpiry(a, b string) bool {
	ta, errA := npm.ParseTime(a)
	tb, errB := npm.ParseTime(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ta.Equal(tb)
}

// PlanCertificates plans certificate copies for one backup (spec 4.2).
// With enabled false nothing is created, refreshed or deleted, but Usable
// still maps every existing usable copy (spec 3.1).
func PlanCertificates(master, backup []npm.Certificate, now time.Time, enabled bool) CertPlan {
	plan := CertPlan{
		Usable:       map[int]int{},
		MasterCustom: map[int]npm.Certificate{},
		MasterByID:   map[int]npm.Certificate{},
	}

	// Managed copies by master ID, lowest backup ID first.
	copies := map[int][]npm.Certificate{}
	for _, c := range backup {
		if id, ok := ParseCertTag(c.NiceName); ok {
			copies[id] = append(copies[id], c)
		}
	}
	for id := range copies {
		sort.Slice(copies[id], func(i, j int) bool { return copies[id][i].ID < copies[id][j].ID })
	}

	for _, m := range master {
		plan.MasterByID[m.ID] = m

		var primary *npm.Certificate
		if cs := copies[m.ID]; len(cs) > 0 {
			c := cs[0]
			primary = &c
			if Usable(c, now) {
				plan.Usable[m.ID] = c.ID
			}
		}

		custom := m.Provider != npm.ProviderLetsEncrypt
		if custom && m.Files == nil {
			// NPM did not return the files (a release that hides the
			// private key). An existing usable copy keeps being used
			// until it expires; otherwise hosts are synced without TLS.
			if _, ok := plan.Usable[m.ID]; !ok {
				plan.MasterCustom[m.ID] = m
			} else if enabled {
				plan.Kept = append(plan.Kept, ObjectResult{Type: TypeCertificate, Name: m.NiceName, Action: ActionUnchanged,
					Message: fmt.Sprintf("NPM no longer returns the private key of custom certificate #%d, so the copy from an earlier run (#%d) is kept as is and cannot be refreshed; it expires %s", m.ID, primary.ID, primary.ExpiresOn)})
			}
			continue
		}
		if !enabled {
			continue
		}

		exp, err := m.Expires()
		if err == nil && !exp.After(now) {
			plan.Errors = append(plan.Errors, ObjectResult{
				Type:    TypeCertificate,
				Name:    m.NiceName,
				Action:  ActionError,
				Message: fmt.Sprintf("master certificate #%d expired on %s; NPM rejects expired uploads, so it is not copied", m.ID, m.ExpiresOn),
			})
			continue
		}

		a := CertAction{Master: m, Copy: primary}
		switch {
		case primary == nil:
			a.Op, a.Reason = CertCreate, "no copy on backup"
		case !Usable(*primary, now):
			a.Op, a.Reason = CertRefresh, "copy is not usable (missing files or expired)"
		case custom && primary.CertHash != "" && primary.CertHash != m.CertHash:
			a.Op, a.Reason = CertRefresh, "the certificate files changed on the master"
		case !sameExpiry(m.ExpiresOn, primary.ExpiresOn):
			a.Op, a.Reason = CertRefresh, fmt.Sprintf("master renewed (expires %s, copy expires %s)", m.ExpiresOn, primary.ExpiresOn)
		default:
			a.Op = CertUnchanged
		}
		plan.Actions = append(plan.Actions, a)
	}

	if !enabled {
		return plan
	}

	// Cleanup: copies whose master certificate no longer exists, and
	// duplicate copies.
	ids := make([]int, 0, len(copies))
	for id := range copies {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		cs := copies[id]
		if _, exists := plan.MasterByID[id]; !exists {
			for _, c := range cs {
				plan.Cleanup = append(plan.Cleanup, CertCleanup{Cert: c, Reason: fmt.Sprintf("master certificate #%d no longer exists", id)})
			}
			continue
		}
		for _, c := range cs[1:] {
			plan.Cleanup = append(plan.Cleanup, CertCleanup{Cert: c, Reason: fmt.Sprintf("duplicate copy of master certificate #%d (keeping #%d)", id, cs[0].ID)})
		}
	}
	return plan
}
