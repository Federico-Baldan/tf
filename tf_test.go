package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// planFixture mirrors real `terraform show -json` output for each kind of
// change (captured from terraform 1.17).
const planFixture = `{"resource_changes":[
 {"address":"t.create","change":{"actions":["create"]}},
 {"address":"t.update","change":{"actions":["update"]}},
 {"address":"t.replace","change":{"actions":["delete","create"]}},
 {"address":"t.delete","change":{"actions":["delete"]}},
 {"address":"t.noop","change":{"actions":["no-op"]}},
 {"address":"t.imp_only","change":{"actions":["no-op"],"importing":{"id":"1"}}},
 {"address":"t.imp_update","change":{"actions":["update"],"importing":{"id":"2"}}},
 {"address":"t.moved","previous_address":"t.old","change":{"actions":["no-op"]}},
 {"address":"t.forgotten","change":{"actions":["forget"]}}
]}`

func TestKindAndCounts(t *testing.T) {
	var p Plan
	if err := json.Unmarshal([]byte(planFixture), &p); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"t.create": actCreate, "t.update": actUpdate, "t.replace": actReplace,
		"t.delete": actDelete, "t.noop": actNoop, "t.imp_only": actImport,
		"t.imp_update": actUpdate, "t.moved": actMove, "t.forgotten": actForget,
	}
	for _, rc := range p.ResourceChanges {
		if got := kindOf(rc); got != want[rc.Address] {
			t.Errorf("%s: kind %q, want %q", rc.Address, got, want[rc.Address])
		}
	}
	c := countPlan(&p)
	// imp_update counts as a change; imp_only, moved and forgotten are state-only
	wantC := planCounts{add: 1, change: 2, destroy: 1, replace: 1, imports: 2, stateOnly: 3}
	if c != wantC {
		t.Errorf("counts %+v, want %+v", c, wantC)
	}
	if c.total() != 8 {
		t.Errorf("total %d, want 8", c.total())
	}
	if c.hooks() != 6 { // replace emits delete + create
		t.Errorf("hooks %d, want 6", c.hooks())
	}
}

func TestNoteOf(t *testing.T) {
	rc := &ResourceChange{Address: "t.b", PreviousAddress: "t.a", Change: &ChangeRepr{Importing: &Importing{ID: "42"}}}
	if got := noteOf(rc, actUpdate); got != "imported from 42, moved from t.a" {
		t.Errorf("got %q", got)
	}
	if got := noteOf(rc, actImport); got != "id 42, moved from t.a" {
		t.Errorf("got %q", got)
	}
	f := &ResourceChange{Address: "t.f", ActionReason: "delete_because_no_resource_config", Change: &ChangeRepr{}}
	if got := noteOf(f, actForget); got != "" {
		t.Errorf("forget should carry no delete reason, got %q", got)
	}
}

func TestUnsetVars(t *testing.T) {
	diag := func(name string) *Diagnostic {
		return &Diagnostic{
			Severity: "error", Summary: "No value for required variable",
			Detail: `The root module input variable "` + name + `" is not set, and has no default value.`,
			Range:  &DiagRange{Filename: "variables.tf"},
		}
	}
	other := &Diagnostic{Severity: "error", Summary: "Invalid expression"}
	warn := &Diagnostic{Severity: "warning", Summary: "Deprecated"}

	m := model{diags: []*Diagnostic{diag("env"), warn, diag("zones")}}
	got := m.unsetVars()
	if len(got) != 2 || got[0].name != "env" || got[1].name != "zones" {
		t.Fatalf("got %+v", got)
	}
	if got[0].where != "variables.tf:0" {
		t.Errorf("where %q", got[0].where)
	}

	// any other error means prompting can't fix it
	if m := (model{diags: []*Diagnostic{diag("env"), other}}); m.unsetVars() != nil {
		t.Error("mixed errors must not prompt")
	}
	// a variable we already supplied and that is still unset must not loop
	if m := (model{diags: []*Diagnostic{diag("env")}, vars: []string{"TF_VAR_env=x"}}); m.unsetVars() != nil {
		t.Error("already-supplied variable must not prompt again")
	}
}

func TestMasked(t *testing.T) {
	for name, want := range map[string]bool{"db_password": true, "api_token": true, "ssh_key": true, "env": false, "region": false} {
		if got := (pendingVar{name: name}).masked(); got != want {
			t.Errorf("%s: masked %v, want %v", name, got, want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	o := parseArgs("apply", []string{"-var", "a=1", "-parallelism=2", "-json", "-auto-approve", "-out", "x.tfplan"})
	if !o.autoApprove || o.planOut != "x.tfplan" {
		t.Errorf("flags not parsed: %+v", o)
	}
	if !reflect.DeepEqual(o.planArgs, []string{"-var", "a=1"}) {
		t.Errorf("planArgs %v", o.planArgs)
	}
	if !reflect.DeepEqual(o.shared, []string{"-parallelism=2"}) {
		t.Errorf("shared %v", o.shared)
	}
	if o := parseArgs("apply", []string{"saved.tfplan"}); o.planFileArg != "saved.tfplan" {
		t.Errorf("plan file arg: %+v", o)
	}
}

func TestDiffResource(t *testing.T) {
	var ch ChangeRepr
	err := json.Unmarshal([]byte(`{"actions":["update"],
		"before":{"a":"x","same":1},"after":{"a":"y","same":1,"n":"new"},
		"after_unknown":{"id":true}}`), &ch)
	if err != nil {
		t.Fatal(err)
	}
	got := diffResource(&ch)
	want := []diffLine{
		{sym: '~', key: "a", old: `"x"`, new: `"y"`},
		{sym: '+', key: "id", new: "(known after apply)"},
		{sym: '+', key: "n", new: `"new"`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}
