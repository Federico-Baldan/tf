package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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
	// a space-separated value must stay attached to its flag
	o = parseArgs("plan", []string{"-generate-config-out", "gen.tf"})
	if !reflect.DeepEqual(o.planArgs, []string{"-generate-config-out", "gen.tf"}) {
		t.Errorf("generate-config-out: %v", o.planArgs)
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

func diffOf(t *testing.T, js string) []diffLine {
	t.Helper()
	var ch ChangeRepr
	if err := json.Unmarshal([]byte(js), &ch); err != nil {
		t.Fatal(err)
	}
	return diffResource(&ch)
}

// terraform flags a sensitive collection once, on the collection itself; its
// elements must not leak.
func TestSensitiveCollectionNotLeaked(t *testing.T) {
	lines := diffOf(t, `{"actions":["create"],
		"after":{"input":{"m":{"a":"topsecret"},"l":["topsecret"],"s":"plain"}},
		"after_sensitive":{"input":{"m":true,"l":true}}}`)
	for _, l := range lines {
		if strings.Contains(l.new+l.old, "topsecret") {
			t.Errorf("leaked: %+v", l)
		}
	}
	var sawPlain bool
	for _, l := range lines {
		sawPlain = sawPlain || (l.key == "input.s" && l.new == `"plain"`)
	}
	if !sawPlain {
		t.Errorf("non-sensitive sibling lost: %+v", lines)
	}
}

// a whole object turning unknown is one line, not one "-" per element
func TestUnknownCollectionCollapsed(t *testing.T) {
	got := diffOf(t, `{"actions":["update"],
		"before":{"output":{"a":"one","b":"x"}},"after":{},
		"after_unknown":{"output":true}}`)
	if len(got) != 1 || got[0].sym != '~' || got[0].key != "output" || got[0].new != "(known after apply)" {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got[0].old, `"a":"one"`) {
		t.Errorf("old value lost: %q", got[0].old)
	}
}

func TestRenderVal(t *testing.T) {
	if got := renderVal("a&b<c>d"); got != `"a&b<c>d"` {
		t.Errorf("html-escaped: %s", got)
	}
	for _, prefix := range []string{"", "x", "xy"} { // shifts where the cut falls
		s := renderVal(prefix + strings.Repeat("è", 70))
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxDiffValueLen {
			t.Errorf("prefix %q: valid=%v runes=%d", prefix, utf8.ValidString(s), utf8.RuneCountInString(s))
		}
	}
}

func TestWrapWords(t *testing.T) {
	in := "  - provider registry.terraform.io/hashicorp/local: required by this configuration but no version is selected"
	lines := wrapWords(in, 40)
	if len(lines) < 3 {
		t.Fatalf("not wrapped: %q", lines)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "  ") || len([]rune(l)) > 40 {
			t.Errorf("bad line %q", l)
		}
	}
	if strings.Join(strings.Fields(strings.Join(lines, " ")), " ") != strings.Join(strings.Fields(in), " ") {
		t.Errorf("words changed: %q", lines)
	}
	if got := wrapWords("", 40); len(got) != 1 || got[0] != "" {
		t.Errorf("empty line: %q", got)
	}
}

// -refresh-only plans have drift but no resource changes
func TestDriftedRefreshOnly(t *testing.T) {
	var p Plan
	js := `{"applyable":true,"resource_changes":[],"resource_drift":[
		{"address":"a","change":{"actions":["delete"]}},
		{"address":"b","change":{"actions":["no-op"]}}]}`
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		t.Fatal(err)
	}
	if d := p.drifted(); !p.Applyable || len(d) != 1 || d[0].Address != "a" {
		t.Errorf("drifted %+v applyable %v", d, p.Applyable)
	}
}
