package sync

import (
	"strings"
	"testing"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/config"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/npm/npmtest"
)

func TestPasswordHint(t *testing.T) {
	cases := map[string]string{
		"s3cret": "s*****",
		"a":      "a",
		"":       "*********",
		"pässwd": "p*****",
		"😀ab":    "�***", // first UTF-16 unit is a lone surrogate in JS
	}
	for pw, want := range cases {
		if got := PasswordHint(pw); got != want {
			t.Errorf("PasswordHint(%q) = %q, want %q", pw, got, want)
		}
	}
}

func al(id int, name string, users ...string) npm.AccessList {
	l := npm.AccessList{ID: id, Name: name}
	for i := 0; i+1 < len(users); i += 2 {
		l.Items = append(l.Items, npm.AccessListItem{Username: users[i], Hint: users[i+1]})
	}
	return l
}

func alOps(p ALPlan) map[string]ALOp {
	out := map[string]ALOp{}
	for _, a := range p.Actions {
		out[a.Master.Name] = a.Op
	}
	return out
}

func items(a ALAction) map[string]string {
	out := map[string]string{}
	for _, it := range a.Payload["items"].([]any) {
		m := it.(map[string]any)
		out[m["username"].(string)] = m["password"].(string)
	}
	return out
}

func TestPlanAccessLists(t *testing.T) {
	pw := map[string]map[string]string{
		"new":      {"alice": "s3cret"},
		"same":     {"alice": "s3cret"},
		"changed":  {"alice": "longer-secret", "bob": "pw"},
		"missing":  {"alice": "s3cret"},
		"wrong":    {"alice": "x3cret!"},
		"clients":  {},
		"ignoreme": {"ghost": "boo"},
	}
	master := []npm.AccessList{
		al(1, "new", "alice", "s*****"),
		al(2, "same", "alice", "s*****"),
		al(3, "changed", "alice", "l************", "bob", "p*"),
		al(4, "missing", "alice", "s*****", "carol", "c***"),
		al(5, "wrong", "alice", "s*****"),
		al(6, "clients"),
		al(7, "ignoreme"),
	}
	master[5].Clients = []npm.AccessListClient{{Address: "10.0.0.0/8", Directive: "allow"}, {Address: "all", Directive: "deny"}}
	backup := []npm.AccessList{
		al(12, "same", "alice", "s*****"),
		al(13, "changed", "alice", "s*****", "bob", "p*"),
		al(16, "clients"),
		al(17, "ignoreme"),
		al(30, "not on master"),
		al(31, "same"), // duplicate on the backup
	}
	backup[2].Clients = []npm.AccessListClient{{Address: "all", Directive: "deny"}, {Address: "10.0.0.0/8", Directive: "allow"}}

	plan := PlanAccessLists(master, backup, pw, false)
	want := map[string]ALOp{"new": ALCreate, "same": ALUnchanged, "changed": ALUpdate, "missing": ALSkip, "wrong": ALSkip, "clients": ALUpdate, "ignoreme": ALUnchanged}
	got := alOps(plan)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: op = %v, want %v", k, got[k], v)
		}
	}
	for _, a := range plan.Actions {
		switch a.Master.Name {
		case "new":
			if items(a)["alice"] != "s3cret" {
				t.Error("a new list gets every password")
			}
		case "changed":
			// alice's hint changed: send it; bob's matches: keep it.
			if it := items(a); it["alice"] != "longer-secret" || it["bob"] != "" {
				t.Errorf("changed items = %v", it)
			}
			if a.BackupID != 13 {
				t.Errorf("backup ID = %d", a.BackupID)
			}
		case "missing":
			if !strings.Contains(a.Reason, `"carol"`) || plan.Problems[4] == "" {
				t.Errorf("missing password reason = %q", a.Reason)
			}
		case "wrong":
			if !strings.Contains(a.Reason, "does not match the master") {
				t.Errorf("hint mismatch reason = %q", a.Reason)
			}
		}
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "ghost") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
	var cleanup []int
	for _, c := range plan.Cleanup {
		cleanup = append(cleanup, c.List.ID)
	}
	if len(cleanup) != 2 || cleanup[0] != 30 || cleanup[1] != 31 {
		t.Errorf("cleanup = %v, want [30 31]", cleanup)
	}

	// forcePasswords sends every password.
	plan = PlanAccessLists(master, backup, pw, true)
	for _, a := range plan.Actions {
		if a.Master.Name == "same" {
			if a.Op != ALUpdate || items(a)["alice"] != "s3cret" {
				t.Errorf("force: %+v", a)
			}
		}
	}
}

func TestPlanAccessListsDuplicateMasterNames(t *testing.T) {
	master := []npm.AccessList{al(1, "dup"), al(2, "dup"), al(3, "ok")}
	backup := []npm.AccessList{al(10, "dup")}
	plan := PlanAccessLists(master, backup, nil, false)
	if len(plan.Errors) != 1 || plan.Problems[1] == "" || plan.Problems[2] == "" {
		t.Fatalf("duplicate master names: errors=%v problems=%v", plan.Errors, plan.Problems)
	}
	if len(plan.Cleanup) != 0 {
		t.Fatal("the backup's list with a duplicated master name must not be deleted")
	}
	if got := alOps(plan); got["ok"] != ALCreate || len(got) != 1 {
		t.Fatalf("ops = %v", got)
	}
}

// ---- executor ----

func TestRunAccessLists(t *testing.T) {
	e := newEnv(t)
	e.cfg.AccessListPasswords = map[string]map[string]string{"Admin panels": {"alice": "s3cret"}}
	alID := e.master.AddAccessList(npmtest.AccessList{Name: "Admin panels", Users: [][2]string{{"alice", "s3cret"}}, Clients: [][2]string{{"10.0.0.0/8", "allow"}}})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"admin.example.com"}, "access_list_id": alID})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"app.example.com"},
		"locations": []any{map[string]any{"path": "/admin", "forward_scheme": "http", "forward_host": "x", "forward_port": 80, "access_list_id": alID}}})
	stale := e.backup.AddAccessList(npmtest.AccessList{Name: "old list"})

	res := e.run()
	if !res.Succeeded() {
		t.Fatalf("%+v", res.Backups[0])
	}
	var copyID int
	for id, l := range e.backup.AccessLists {
		if l.Name == "Admin panels" {
			copyID = id
			if len(l.Users) != 1 || l.Users[0][1] != "s3cret" || len(l.Clients) != 1 {
				t.Fatalf("copy = %+v", l)
			}
		}
	}
	if _, ok := e.backup.AccessList(stale); ok {
		t.Error("access list not on master should be deleted")
	}
	hosts := e.backupHosts()
	if num(hosts["admin.example.com"]["access_list_id"]) != copyID {
		t.Fatalf("host access list not rewritten: %v", hosts["admin.example.com"])
	}
	loc := hosts["app.example.com"]["locations"].([]any)[0].(map[string]any)
	if num(loc["access_list_id"]) != copyID {
		t.Fatalf("location access list not rewritten: %v", loc)
	}

	// Nothing changed: nothing is written.
	e.run()
	if w := e.backup.Writes(); len(w) != 0 {
		t.Fatalf("second run wrote %v", w)
	}

	// A password change with a different length is detected by the hint.
	e.cfg.AccessListPasswords["Admin panels"]["alice"] = "n3w-secret"
	for _, l := range e.master.AccessLists {
		l.Users[0][1] = "n3w-secret"
	}
	e.run()
	if l, _ := e.backup.AccessList(copyID); l.Users[0][1] != "n3w-secret" {
		t.Fatalf("password not updated: %+v", l)
	}

	// Same first character and length: not detectable, until forced.
	e.cfg.AccessListPasswords["Admin panels"]["alice"] = "n3w-secreT"
	for _, l := range e.master.AccessLists {
		l.Users[0][1] = "n3w-secreT"
	}
	e.run()
	if l, _ := e.backup.AccessList(copyID); l.Users[0][1] != "n3w-secret" {
		t.Fatal("expected the limitation: same hint is not detected")
	}
	e.opts.ForcePasswords = true
	e.run()
	if l, _ := e.backup.AccessList(copyID); l.Users[0][1] != "n3w-secreT" {
		t.Fatal("forcePasswords should send the password")
	}
}

func TestRunAccessListMissingPasswordFailsClosed(t *testing.T) {
	e := newEnv(t)
	alID := e.master.AddAccessList(npmtest.AccessList{Name: "Admin panels", Users: [][2]string{{"alice", "s3cret"}}})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"admin.example.com"}, "access_list_id": alID, "forward_host": "new"})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"new.example.com"}, "access_list_id": alID})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"admin.example.com"}, "forward_host": "old"})

	res := e.run()
	if res.Succeeded() {
		t.Fatal("missing password is an error")
	}
	obj := objects(res)
	if o := obj["access list:Admin panels"]; o.Action != ActionSkipped || !strings.Contains(o.Message, `"alice"`) {
		t.Fatalf("list: %+v", o)
	}
	if o := obj["proxy host:admin.example.com"]; o.Action != ActionSkipped || !o.Failed {
		t.Fatalf("host: %+v", o)
	}
	if len(e.backup.AccessLists) != 0 || len(e.backup.Writes()) != 0 {
		t.Fatalf("nothing may be written: %v", e.backup.Writes())
	}
}

func TestRunAccessListCleanupKeepsReferenced(t *testing.T) {
	e := newEnv(t)
	e.cfg.ExcludeDomains = []string{"own.example.com"}
	e.master.AddAccessList(npmtest.AccessList{Name: "keep"})
	used := e.backup.AddAccessList(npmtest.AccessList{Name: "backup only"})
	e.backup.AddHost(proxyPath, map[string]any{"domain_names": []string{"own.example.com"}, "access_list_id": used})
	res := e.run()
	if o := objects(res)["access list:backup only"]; o.Action != ActionSkipped || o.IsError() {
		t.Fatalf("%+v", o)
	}
	if _, ok := e.backup.AccessList(used); !ok {
		t.Fatal("a list used by a backup host must not be deleted (NPM would silently remove the host's protection)")
	}
}

func TestRunAccessListsOff(t *testing.T) {
	e := newEnv(t)
	e.cfg.Features[config.FeatureAccessLists] = false
	alID := e.master.AddAccessList(npmtest.AccessList{Name: "x"})
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}, "access_list_id": alID})
	e.backup.AddAccessList(npmtest.AccessList{Name: "backup only"})
	res := e.run()
	if o := objects(res)["proxy host:a.example.com"]; o.Action != ActionSkipped || !strings.Contains(o.Message, "switched off") {
		t.Fatalf("%+v", o)
	}
	if len(e.backup.AccessLists) != 1 {
		t.Fatal("access lists must be left alone when the feature is off")
	}
}

func TestRunEmptyMasterAccessLists(t *testing.T) {
	e := newEnv(t)
	e.master.AddHost(proxyPath, map[string]any{"domain_names": []string{"a.example.com"}})
	e.backup.AddAccessList(npmtest.AccessList{Name: "x"})
	res := e.run()
	if !res.Backups[0].Aborted || !strings.Contains(res.Backups[0].Error, "no access lists") {
		t.Fatalf("%+v", res.Backups[0])
	}
}
