package sync

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
)

// TypeAccessList is the object type of access lists.
const TypeAccessList = "access list"

// PasswordHint computes NPM's hint for a password: its first character
// followed by one * for each remaining character (spec 4.4). NPM computes it
// in JavaScript, so characters and length are counted in UTF-16 code units.
func PasswordHint(password string) string {
	if password == "" {
		return "*" + strings.Repeat("*", 8)
	}
	units := utf16.Encode([]rune(password))
	first := string(utf16.Decode(units[:1]))
	if utf16.IsSurrogate(rune(units[0])) {
		first = "�" // a lone surrogate does not survive JSON
	}
	return first + strings.Repeat("*", len(units)-1)
}

// ALOp is a planned access list operation.
type ALOp int

// Access list operations.
const (
	ALUnchanged ALOp = iota
	ALCreate
	ALUpdate
	ALSkip // cannot be synced: duplicate name, missing or wrong password
)

// ALAction is the planned action for one master access list.
type ALAction struct {
	Op       ALOp
	Master   npm.AccessList
	BackupID int // update, unchanged, and skip when the backup has a match
	Payload  map[string]any
	Reason   string
}

// ALCleanup is an access list on the backup to delete during cleanup.
type ALCleanup struct {
	List   npm.AccessList
	Reason string
}

// ALPlan is the access list part of a backup's plan.
type ALPlan struct {
	Actions []ALAction
	// Errors are master access lists that are not synced because their
	// name is used more than once on the master.
	Errors []ObjectResult
	// Problems maps master access list ID to why it is not synced; hosts
	// that use it are skipped.
	Problems map[int]string
	// Warnings are configured users that do not exist on the master.
	Warnings []string
	Cleanup  []ALCleanup
}

// PlanAccessLists plans access lists for one backup (spec 4.4). passwords
// maps list name to username to password. With force, every configured
// password is sent.
func PlanAccessLists(master, backup []npm.AccessList, passwords map[string]map[string]string, force bool) ALPlan {
	plan := ALPlan{Problems: map[int]string{}}

	// Master lists by name; a name used more than once is not synced.
	byName := map[string][]npm.AccessList{}
	for _, m := range master {
		byName[m.Name] = append(byName[m.Name], m)
	}
	// Backup lists by name, lowest ID first.
	backupByName := map[string][]npm.AccessList{}
	sorted := append([]npm.AccessList(nil), backup...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, b := range sorted {
		backupByName[b.Name] = append(backupByName[b.Name], b)
	}

	matched := map[int]bool{}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		lists := byName[name]
		var match *npm.AccessList
		if bs := backupByName[name]; len(bs) > 0 {
			match = &bs[0]
			matched[match.ID] = true
		}

		if len(lists) > 1 {
			ids := make([]string, len(lists))
			for i, l := range lists {
				ids[i] = fmt.Sprintf("#%d", l.ID)
				plan.Problems[l.ID] = fmt.Sprintf("access list name %q is used by %d lists on the master", name, len(lists))
			}
			plan.Errors = append(plan.Errors, ObjectResult{Type: TypeAccessList, Name: name, Action: ActionError,
				Message: fmt.Sprintf("name used by more than one access list on the master (%s); none of them is synced, and hosts that use them are skipped", strings.Join(ids, ", "))})
			continue
		}

		m := lists[0]
		configured := passwords[m.Name]
		for user := range configured {
			if !hasUser(m, user) {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("access list %q: configured user %q does not exist on the master; ignored", m.Name, user))
			}
		}

		a := ALAction{Master: m}
		if match != nil {
			a.BackupID = match.ID
		}
		if problem := passwordProblem(m, configured); problem != "" {
			a.Op, a.Reason = ALSkip, problem
			plan.Problems[m.ID] = problem
			plan.Actions = append(plan.Actions, a)
			continue
		}

		a.Payload = alPayload(m, configured, match, force)
		switch {
		case match == nil:
			a.Op = ALCreate
		case force && len(m.Items) > 0:
			a.Op, a.Reason = ALUpdate, "sending every password (forcePasswords)"
		case !alEqual(m, *match, configured):
			a.Op = ALUpdate
		default:
			a.Op = ALUnchanged
		}
		plan.Actions = append(plan.Actions, a)
	}

	for _, b := range sorted {
		if matched[b.ID] {
			continue
		}
		reason := "not on master"
		if len(byName[b.Name]) > 0 {
			reason = fmt.Sprintf("duplicate of backup access list #%d with the same name", backupByName[b.Name][0].ID)
		}
		plan.Cleanup = append(plan.Cleanup, ALCleanup{List: b, Reason: reason})
	}
	return plan
}

func hasUser(l npm.AccessList, user string) bool {
	for _, it := range l.Items {
		if it.Username == user {
			return true
		}
	}
	return false
}

// passwordProblem checks that every user of the master list has a configured
// password whose hint matches the master's (spec 4.4).
func passwordProblem(m npm.AccessList, configured map[string]string) string {
	for _, it := range m.Items {
		pw, ok := configured[it.Username]
		if !ok || pw == "" {
			return fmt.Sprintf("no password configured for user %q of access list %q (set accessListPasswords)", it.Username, m.Name)
		}
		if it.Hint != "" && PasswordHint(pw) != it.Hint {
			return fmt.Sprintf("the configured password for user %q of access list %q does not match the master (first character or length differs)", it.Username, m.Name)
		}
	}
	return ""
}

// alPayload builds the create/update payload. A user's password is sent
// empty (NPM keeps the current one) when the backup already has the user with
// the configured password's hint, unless force is set.
func alPayload(m npm.AccessList, configured map[string]string, backup *npm.AccessList, force bool) map[string]any {
	items := make([]any, 0, len(m.Items))
	for _, it := range m.Items {
		pw := configured[it.Username]
		if !force && backup != nil && backupHint(*backup, it.Username) == PasswordHint(pw) {
			pw = ""
		}
		items = append(items, map[string]any{"username": it.Username, "password": pw})
	}
	clients := make([]any, 0, len(m.Clients))
	for _, c := range m.Clients {
		clients = append(clients, map[string]any{"address": c.Address, "directive": c.Directive})
	}
	return map[string]any{
		"name":        m.Name,
		"satisfy_any": bool(m.SatisfyAny),
		"pass_auth":   bool(m.PassAuth),
		"items":       items,
		"clients":     clients,
	}
}

func backupHint(b npm.AccessList, user string) string {
	for _, it := range b.Items {
		if it.Username == user {
			return it.Hint
		}
	}
	return "\x00missing"
}

// alEqual reports whether the backup list already matches the master: same
// settings, same client rules in the same order (nginx evaluates them in
// order), and the same users with the configured passwords' hints.
func alEqual(m, b npm.AccessList, configured map[string]string) bool {
	if m.SatisfyAny != b.SatisfyAny || m.PassAuth != b.PassAuth || len(m.Clients) != len(b.Clients) || len(m.Items) != len(b.Items) {
		return false
	}
	for i := range m.Clients {
		if m.Clients[i].Address != b.Clients[i].Address || m.Clients[i].Directive != b.Clients[i].Directive {
			return false
		}
	}
	for _, it := range m.Items {
		if backupHint(b, it.Username) != PasswordHint(configured[it.Username]) {
			return false
		}
	}
	return true
}
