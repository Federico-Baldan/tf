package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
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
 {"address":"t.forgotten","change":{"actions":["forget"]}},
 {"address":"t.fcreate","change":{"actions":["forget","create"]}},
 {"address":"t.cbd","change":{"actions":["create","forget"]}},
 {"address":"t.read","change":{"actions":["read"]}}
],"action_invocations":[{"address":"action.x.y"}]}`

func TestKindAndCounts(t *testing.T) {
	var p Plan
	if err := json.Unmarshal([]byte(planFixture), &p); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"t.create": actCreate, "t.update": actUpdate, "t.replace": actReplace,
		"t.delete": actDelete, "t.noop": actNoop, "t.imp_only": actImport,
		"t.imp_update": actUpdate, "t.moved": actMove, "t.forgotten": actForget,
		// lifecycle { destroy = false } replaces: the old object is forgotten, not destroyed
		"t.fcreate": actCreate, "t.cbd": actCreate, "t.read": actRead,
	}
	for _, rc := range p.ResourceChanges {
		if got := kindOf(rc); got != want[rc.Address] {
			t.Errorf("%s: kind %q, want %q", rc.Address, got, want[rc.Address])
		}
	}
	c := countPlan(&p)
	// imp_update counts as a change; imp_only, moved and forgotten are state-only
	wantC := planCounts{add: 3, change: 2, destroy: 1, replace: 1, imports: 2, stateOnly: 3, reads: 1, invokes: 1}
	if c != wantC {
		t.Errorf("counts %+v, want %+v", c, wantC)
	}
	if c.total() != 11 { // 3+2+1+1 changes, 3 state-only, 1 action
		t.Errorf("total %d, want 11", c.total())
	}
	if c.hooks() != 9 { // replace emits delete + create, a deferred read one pair
		t.Errorf("hooks %d, want 9", c.hooks())
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
	fc := &ResourceChange{Address: "t.fc", Change: &ChangeRepr{Actions: []string{"forget", "create"}}}
	if got := noteOf(fc, actCreate); got != "old object forgotten, not destroyed" {
		t.Errorf("got %q", got)
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
	if got[0].where() != "variables.tf:0" {
		t.Errorf("where %q", got[0].where())
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
	for name, want := range map[string]bool{"db_password": true, "db_pwd": true, "api_token": true, "ssh_key": true, "env": false, "region": false} {
		if got := (pendingVar{name: name}).masked(); got != want {
			t.Errorf("%s: masked %v, want %v", name, got, want)
		}
	}
	// declared sensitive wins over the name
	if !(pendingVar{name: "region", sensitive: true}).masked() {
		t.Error("sensitive declaration must mask")
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

// the old value of a collection that turns unknown must not reveal a
// sensitive element inside it
func TestSensitiveInsideCollapsedCollection(t *testing.T) {
	got := diffOf(t, `{"actions":["update"],
		"before":{"input":{"pw":"TOPSECRET","b":"x"}},"after":{},
		"before_sensitive":{"input":{"pw":true}},
		"after_unknown":{"input":true}}`)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if strings.Contains(got[0].old, "TOPSECRET") || got[0].old != "(sensitive)" {
		t.Errorf("leaked: %+v", got[0])
	}
}

func TestVarEnv(t *testing.T) {
	o := parseArgs("apply", []string{"-var", "a=1", "-var=b=c=d", "-var-file=x.tfvars"})
	if want := []string{"TF_VAR_a=1", "TF_VAR_b=c=d"}; !reflect.DeepEqual(o.varEnv, want) {
		t.Errorf("varEnv %v, want %v", o.varEnv, want)
	}
	if o := parseArgs("plan", []string{"-invoke", "action.a.b"}); !reflect.DeepEqual(o.planArgs, []string{"-invoke", "action.a.b"}) {
		t.Errorf("-invoke value detached: %v", o.planArgs)
	}
}

func TestDeclSensitive(t *testing.T) {
	dir := t.TempDir()
	tf := `variable "plain" {
  type = string
}

variable "hidden" {
  type = string
  validation {
    condition     = length(var.hidden) > 0
    error_message = "x"
  }
  sensitive = true
}

variable "inline" { sensitive = true }
`
	if err := os.WriteFile(filepath.Join(dir, "variables.tf"), []byte(tf), 0o600); err != nil {
		t.Fatal(err)
	}
	for line, want := range map[int]bool{1: false, 5: true, 14: true} {
		if got := declSensitive(dir, "variables.tf", line); got != want {
			t.Errorf("line %d: got %v, want %v", line, got, want)
		}
	}
	if declSensitive(dir, "missing.tf", 1) {
		t.Error("unreadable file must not claim sensitive")
	}
}

// the plan's warnings survive into the recap after apply, without duplicates
func TestRecapKeepsPlanWarnings(t *testing.T) {
	w := &Diagnostic{Severity: "warning", Summary: "Deprecated", Detail: "old"}
	dup := &Diagnostic{Severity: "warning", Summary: "Deprecated", Detail: "old"}
	e := &Diagnostic{Severity: "error", Summary: "Boom"}
	m := model{width: 80, carried: []*Diagnostic{w}, diags: []*Diagnostic{dup, e}}
	m.appendDiagRecap()
	var warnings, errs int
	for _, l := range m.final {
		warnings += strings.Count(l, "Warning:")
		errs += strings.Count(l, "Error:")
	}
	if warnings != 1 || errs != 1 {
		t.Errorf("warnings=%d errors=%d, lines=%q", warnings, errs, m.final)
	}
}

// enter/space on a blank separator row used to open node 0 (or panic with no nodes)
func TestToggleOnBlankRow(t *testing.T) {
	m := model{rows: []row{{kind: rowBlank}, {kind: rowSection, nodeIdx: -1}}}
	m.toggleAt(0)
	m.toggleAt(1)
	m.nodes = []*node{{addr: "a"}}
	m.rows = []row{{kind: rowBlank}, {kind: rowNode, nodeIdx: 0}}
	m.plan = &Plan{}
	m.toggleAt(0)
	if m.nodes[0].expanded {
		t.Error("blank row toggled node 0")
	}
}

func TestStartErrShown(t *testing.T) {
	m := model{}
	m.appendStartErr(os.ErrNotExist)
	if len(m.final) != 1 {
		t.Fatalf("final %q", m.final)
	}
	m = model{diags: []*Diagnostic{{Severity: "error"}}}
	m.appendStartErr(os.ErrNotExist)
	if len(m.final) != 0 {
		t.Error("must not add noise when terraform produced diagnostics")
	}
}

// keys with dots must not be taken for nesting: "app" being unknown must not
// hide a change to "app.kubernetes.io/name"
func TestDottedKeys(t *testing.T) {
	got := diffOf(t, `{"actions":["update"],
		"before":{"labels":{"app":"a","app.kubernetes.io/name":"old"}},
		"after":{"labels":{"app.kubernetes.io/name":"new"}},
		"after_unknown":{"labels":{"app":true}}}`)
	if len(got) != 2 ||
		got[0].key != "labels.app" || got[0].new != "(known after apply)" ||
		got[1].key != "labels.app.kubernetes.io/name" || got[1].old != `"old"` || got[1].new != `"new"` {
		t.Fatalf("got %+v", got)
	}
}

// a sensitive flag on the root covers everything below it
func TestRootSensitive(t *testing.T) {
	for _, l := range diffOf(t, `{"actions":["update"],"before":{"a":"OLDSECRET"},"after":{"a":"NEWSECRET"},
		"before_sensitive":true,"after_sensitive":true}`) {
		if strings.Contains(l.old+l.new, "SECRET") {
			t.Errorf("leaked: %+v", l)
		}
	}
}

// replace_paths steps map onto the same path form as the diff keys
func TestForcesReplacementPath(t *testing.T) {
	got := diffOf(t, `{"actions":["delete","create"],
		"before":{"tags":{"a.b":"x"},"l":[{"n":1}]},"after":{"tags":{"a.b":"y"},"l":[{"n":2}]},
		"replace_paths":[["tags","a.b"],["l",0,"n"]]}`)
	if len(got) != 2 || !got[0].forces || !got[1].forces || got[1].key != "tags.a.b" {
		t.Fatalf("got %+v", got)
	}
}

func TestClean(t *testing.T) {
	if got := clean("a\x1b]0;PWNED\x07b\u009bc"); strings.ContainsAny(got, "\x1b\x07\u009b") || !strings.Contains(got, "PWNED") {
		t.Errorf("got %q", got)
	}
	if clean("ok\nline") != "ok\nline" {
		t.Error("newlines must survive (diagnostics are split on them)")
	}
	// JSON already escapes ESC; DEL and the C1 range (U+009B is CSI to some terminals) it does not
	if got := renderVal("k\x1b[31m\u009b31m\x7f"); strings.ContainsAny(got, "\x1b\u009b\x7f") {
		t.Errorf("value not sanitised: %q", got)
	}
}

func TestDeclinedExitCodes(t *testing.T) {
	for _, tc := range []struct {
		sub  string
		want int
	}{{"apply", 1}, {"destroy", 1}, {"plan", 0}} {
		m := model{stage: stageReview, cfg: config{sub: tc.sub}, plan: &Plan{}}
		next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
		if got := next.(model).exitCode; got != tc.want {
			t.Errorf("%s: q exits %d, want %d", tc.sub, got, tc.want)
		}
	}
}

// the confirm prompt needs an explicit y; enter must not apply
func TestEnterDoesNotConfirm(t *testing.T) {
	m := model{stage: stageReview, confirm: true, cfg: config{sub: "apply"}, plan: &Plan{}}
	next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if got := next.(model); got.stage != stageReview || got.confirm {
		t.Errorf("enter changed stage to %v (confirm=%v)", got.stage, got.confirm)
	}
}

// a plan that ends normally after we asked it to stop is still an interrupted run
func TestInterruptedPlanThatFinishes(t *testing.T) {
	m := model{stage: stagePlanning, interrupted: true, cfg: config{sub: "apply"}}
	next, cmd := m.handleExit(exitMsg{phase: "plan", code: 0})
	got := next.(model)
	if got.exitCode != 1 || !got.quitting || cmd == nil {
		t.Errorf("exit=%d quitting=%v", got.exitCode, got.quitting)
	}
}

// prompts follow declaration order, not terraform's random report order
func TestUnsetVarsDeclarationOrder(t *testing.T) {
	mk := func(name string, line int) *Diagnostic {
		return &Diagnostic{
			Severity: "error", Summary: "No value for required variable",
			Detail: `The root module input variable "` + name + `" is not set, and has no default value.`,
			Range: &DiagRange{Filename: "variables.tf", Start: struct {
				Line int `json:"line"`
			}{line}},
		}
	}
	m := model{diags: []*Diagnostic{mk("c", 30), mk("a", 2), mk("b", 10)}}
	var names []string
	for _, v := range m.unsetVars() {
		names = append(names, v.name)
	}
	if !reflect.DeepEqual(names, []string{"a", "b", "c"}) {
		t.Errorf("order %v", names)
	}
}

func TestStateFlagsReachApply(t *testing.T) {
	o := parseArgs("apply", []string{"-state=a.tfstate", "-state-out", "b.tfstate", "-backup=c", "-input=false", "-refresh-only"})
	if want := []string{"-state=a.tfstate", "-state-out", "b.tfstate", "-backup=c"}; !reflect.DeepEqual(o.shared, want) {
		t.Errorf("shared %v, want %v", o.shared, want)
	}
	if !o.noInput || !o.refreshOnly {
		t.Errorf("noInput=%v refreshOnly=%v", o.noInput, o.refreshOnly)
	}
}
