package main

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

type stage int

const (
	stagePlanning stage = iota
	stagePrompt         // asking for required variables the plan failed on
	stageShowing
	stageReview
	stageApplying
	stageDone
)

type taskStatus int

const (
	taskRunning taskStatus = iota
	taskDone
	taskFailed
)

type task struct {
	action  string // hook action: create/update/delete/read/refresh/...
	status  taskStatus
	start   time.Time
	doneAt  time.Time
	elapsed float64 // from terraform, seconds
	note    string  // e.g. provisioner name
}

const doneLinger = 1500 * time.Millisecond

type config struct {
	tfBin    string
	sub      string // plan | apply | destroy
	o        opts
	planPath string
	tmpDir   string // private dir holding our temporary plan file; removed on exit
	keepPlan bool
	skipPlan bool // apply was given an existing plan file
}

// tree node in the review stage
type node struct {
	addr     string
	action   string
	reason   string
	expanded bool
	detail   []diffLine
}

type rowKind int

const (
	rowBlank rowKind = iota
	rowSection
	rowNode
	rowDetail
)

type row struct {
	kind     rowKind
	note     string // dim suffix on node rows: reason, import id, previous address
	nodeIdx  int
	action   string // for node and section rows
	keyW     int    // key column width shared by the node's rows
	diffLine        // text for section/node rows; sym/key/old/new for details
}

// pendingVar is a required variable terraform reported as unset.
type pendingVar struct {
	name      string
	file      string // where it is declared
	line      int
	sensitive bool // declared sensitive = true
}

func (v pendingVar) where() string {
	if v.file == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", clean(v.file), v.line)
}

type model struct {
	cfg    config
	events chan tea.Msg
	proc   *procHolder

	stage  stage
	width  int
	height int
	spin   int

	// progress stages (plan refresh + apply)
	tasks       map[string]*task
	order       []string
	startedAt   time.Time
	completed   int
	failed      int
	total       int // apply only: planned change count
	refreshed   int
	plannedSeen int
	diags       []*Diagnostic
	carried     []*Diagnostic // plan-phase warnings kept for the final recap
	summary     *ChangeSummary
	outputs     map[string]*OutputMeta

	// variable prompt: queue of unset variables, the text typed so far, and
	// the TF_VAR_* entries collected for the next plan run
	queue []pendingVar
	input string
	vars  []string

	// review stage
	plan    *Plan
	counts  planCounts
	nodes   []*node
	rows    []row
	cursor  int
	offset  int
	confirm bool

	// lifecycle
	interrupted bool
	quitting    bool
	exitCode    int
	final       []string // recap printed after the TUI exits
}

func newModel(cfg config) model {
	m := model{
		cfg:     cfg,
		events:  make(chan tea.Msg, 1024),
		proc:    &procHolder{},
		tasks:   map[string]*task{},
		outputs: map[string]*OutputMeta{},
		width:   100,
		height:  30,
	}
	if cfg.skipPlan {
		m.stage = stageShowing
	} else {
		m.stage = stagePlanning
	}
	m.startedAt = time.Now()
	return m
}

func tickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Init() tea.Cmd {
	if m.cfg.skipPlan {
		return tea.Batch(runShowCmd(m.cfg.tfBin, m.cfg.o.globals, m.cfg.planPath), tickCmd())
	}
	return tea.Batch(m.startPlanCmd(), waitFor(m.events), tickCmd())
}

func (m model) startPlanCmd() tea.Cmd {
	args := append([]string{}, m.cfg.o.globals...)
	args = append(args, "plan", "-input=false", "-json", "-out="+m.cfg.planPath)
	if m.cfg.sub == "destroy" {
		args = append(args, "-destroy")
	}
	args = append(args, m.cfg.o.shared...)
	args = append(args, m.cfg.o.planArgs...)
	env := m.vars
	return func() tea.Msg {
		if err := startTF(m.cfg.tfBin, args, "plan", env, m.events, m.proc); err != nil {
			return exitMsg{phase: "plan", code: 1, err: err}
		}
		return nil
	}
}

func (m model) startApplyCmd() tea.Cmd {
	args := append([]string{}, m.cfg.o.globals...)
	args = append(args, "apply", "-input=false", "-json")
	args = append(args, m.cfg.o.shared...)
	args = append(args, m.cfg.planPath)
	// ephemeral variables must be given again when a saved plan is applied;
	// the same values are ignored for ordinary variables
	env := append(append([]string{}, m.cfg.o.varEnv...), m.vars...)
	return func() tea.Msg {
		if err := startTF(m.cfg.tfBin, args, "apply", env, m.events, m.proc); err != nil {
			return exitMsg{phase: "apply", code: 1, err: err}
		}
		return nil
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.width = msg.Width
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}
		m.clampScroll()
		return m, nil

	case tickMsg:
		m.spin++
		m.reapTasks()
		// the prompt keeps the chain alive so the retry needs no new tick
		if m.stage == stagePlanning || m.stage == stagePrompt || m.stage == stageShowing || m.stage == stageApplying {
			return m, tickCmd()
		}
		return m, nil

	case evMsg:
		m.handleEvent(msg.ev)
		return m, waitFor(m.events)

	case exitMsg:
		return m.handleExit(msg)

	case showMsg:
		return m.handleShow(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleEvent(ev *Event) {
	switch {
	case ev.Type == "diagnostic" && ev.Diagnostic != nil:
		m.diags = append(m.diags, ev.Diagnostic)
	case ev.Type == "change_summary" && ev.Changes != nil:
		m.summary = ev.Changes
	case ev.Type == "planned_change":
		m.plannedSeen++
	case ev.Type == "outputs":
		maps.Copy(m.outputs, ev.Outputs)
	case ev.Hook != nil && ev.Hook.Resource != nil:
		addr := ev.Hook.Resource.Addr
		switch {
		case strings.HasSuffix(ev.Type, "_start"):
			action := ev.Hook.Action
			if action == "" {
				if strings.HasPrefix(ev.Type, "refresh") {
					action = "refresh"
				} else if strings.HasPrefix(ev.Type, "provision") {
					action = "provision"
				}
			}
			if strings.HasPrefix(ev.Type, "provision") {
				if t, ok := m.tasks[addr]; ok {
					t.note = "provisioning"
				}
				return
			}
			if _, ok := m.tasks[addr]; !ok {
				m.order = append(m.order, addr)
			}
			m.tasks[addr] = &task{action: action, status: taskRunning, start: time.Now()}
		case strings.HasSuffix(ev.Type, "_complete"):
			if strings.HasPrefix(ev.Type, "provision") {
				if t, ok := m.tasks[addr]; ok {
					t.note = ""
				}
				return
			}
			if t, ok := m.tasks[addr]; ok {
				t.status = taskDone
				t.doneAt = time.Now()
				t.elapsed = ev.Hook.ElapsedSeconds
			}
			if ev.Type == "refresh_complete" {
				m.refreshed++
			}
			if ev.Type == "apply_complete" && m.stage == stageApplying {
				m.completed++
			}
		case strings.HasSuffix(ev.Type, "_errored"):
			if t, ok := m.tasks[addr]; ok {
				t.status = taskFailed
				t.doneAt = time.Now()
				t.elapsed = ev.Hook.ElapsedSeconds
			} else {
				m.order = append(m.order, addr)
				m.tasks[addr] = &task{action: ev.Hook.Action, status: taskFailed, start: time.Now(), doneAt: time.Now()}
			}
			if m.stage == stageApplying && ev.Type == "apply_errored" {
				m.failed++ // provision_errored is followed by apply_errored for the same resource
			}
		}
	}
}

func (m *model) reapTasks() {
	now := time.Now()
	keep := m.order[:0]
	for _, addr := range m.order {
		t := m.tasks[addr]
		if t.status == taskDone && now.Sub(t.doneAt) > doneLinger {
			delete(m.tasks, addr)
			continue
		}
		keep = append(keep, addr)
	}
	m.order = keep
}

func (m model) handleExit(msg exitMsg) (tea.Model, tea.Cmd) {
	switch msg.phase {
	case "plan":
		if msg.code != 0 || m.interrupted {
			if q := m.unsetVars(); !m.interrupted && !m.cfg.o.noInput && len(q) > 0 {
				m.queue, m.input, m.stage = q, "", stagePrompt
				return m, nil
			}
			m.exitCode = 1
			if m.interrupted {
				m.final = append(m.final, stYellow.Render("Plan interrupted."))
			} else {
				m.final = append(m.final, stRedBold.Render("✗ Plan failed."))
			}
			m.appendStartErr(msg.err)
			m.appendDiagRecap()
			m.quitting = true
			m.stage = stageDone
			return m, tea.Quit
		}
		m.stage = stageShowing
		return m, runShowCmd(m.cfg.tfBin, m.cfg.o.globals, m.cfg.planPath)

	case "apply":
		m.stage = stageDone
		m.quitting = true
		elapsed := time.Since(m.startedAt)
		if msg.code != 0 {
			m.exitCode = 1
			if m.interrupted {
				m.final = append(m.final, stYellow.Render(fmt.Sprintf("Apply interrupted after %s. %d completed, %d failed.", fmtDur(elapsed), m.completed, m.failed)))
			} else {
				m.final = append(m.final, stRedBold.Render(fmt.Sprintf("✗ Apply failed after %s. %d completed, %d failed.", fmtDur(elapsed), m.completed, m.failed)))
			}
		} else {
			if m.cfg.sub == "destroy" {
				n := m.completed
				if s := m.summary; s != nil {
					n = s.Remove
				}
				m.final = append(m.final, stGreen.Render(fmt.Sprintf("✓ Destroy complete in %s — %s destroyed.", fmtDur(elapsed), plural(n, "resource"))))
			} else {
				if s := m.summary; s != nil {
					extra := ""
					if s.Import > 0 {
						extra = fmt.Sprintf(", %d imported", s.Import)
					}
					if s.ActionInvocation > 0 {
						extra += fmt.Sprintf(", %d invoked", s.ActionInvocation)
					}
					m.final = append(m.final, stGreen.Render(fmt.Sprintf("✓ Apply complete in %s — %d added, %d changed, %d destroyed%s.", fmtDur(elapsed), s.Add, s.Change, s.Remove, extra)))
				} else {
					m.final = append(m.final, stGreen.Render(fmt.Sprintf("✓ Apply complete in %s — %s.", fmtDur(elapsed), plural(m.completed, "resource"))))
				}
				m.appendOutputsRecap()
			}
		}
		m.appendStartErr(msg.err)
		m.appendDiagRecap()
		return m, tea.Quit
	}
	return m, nil
}

// appendStartErr explains a failure that produced no diagnostics because
// terraform never ran (bad TF_BIN, not executable...).
func (m *model) appendStartErr(err error) {
	var ee *exec.ExitError
	if err != nil && len(m.diags) == 0 && !errors.As(err, &ee) {
		m.final = append(m.final, stRed.Render(clean(err.Error())))
	}
}

func (m model) handleShow(msg showMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.exitCode = 1
		m.final = append(m.final, stRedBold.Render("✗ Failed to read plan: "+clean(msg.err.Error())))
		m.quitting = true
		m.stage = stageDone
		return m, tea.Quit
	}
	m.plan = msg.plan
	m.counts = countPlan(msg.plan)
	m.buildTree()

	outputChanges := 0
	for _, ch := range m.plan.OutputChanges {
		if actionOf(ch.Actions) != actNoop {
			outputChanges++
		}
	}

	// applyable also covers what the counts can't see: refresh-only drift, actions
	if m.counts.total() == 0 && outputChanges == 0 && !m.plan.Applyable {
		m.final = append(m.final, stGreen.Render("✓ No changes. Infrastructure matches the configuration."))
		m.appendDiagRecap()
		m.quitting = true
		m.stage = stageDone
		return m, tea.Quit
	}

	if (m.cfg.sub == "apply" || m.cfg.sub == "destroy") && m.cfg.o.autoApprove {
		return m.beginApply()
	}
	m.stage = stageReview
	return m, nil
}

func (m model) beginApply() (tea.Model, tea.Cmd) {
	m.stage = stageApplying
	m.tasks = map[string]*task{}
	m.order = nil
	m.completed, m.failed = 0, 0
	m.total = m.counts.hooks()
	m.startedAt = time.Now()
	for _, d := range m.diags { // plan-phase warnings (deprecations...) would otherwise be lost
		if d.Severity == "warning" {
			m.carried = append(m.carried, d)
		}
	}
	m.diags = nil
	return m, tea.Batch(m.startApplyCmd(), waitFor(m.events), tickCmd())
}

// ---- review tree ----

func (m *model) buildTree() {
	m.nodes = nil
	add := func(rc *ResourceChange, action string) {
		detail := diffResource(rc.Change)
		if action == actForget {
			detail = nil // the resource stays; red "-" lines would read as a delete
		}
		m.nodes = append(m.nodes, &node{
			addr:   clean(rc.Address),
			action: action,
			reason: clean(noteOf(rc, action)),
			detail: detail,
		})
	}
	byAction := map[string][]*ResourceChange{}
	for _, rc := range m.plan.ResourceChanges {
		a := kindOf(rc)
		if a == actNoop {
			continue
		}
		byAction[a] = append(byAction[a], rc)
	}
	for _, a := range []string{actImport, actCreate, actUpdate, actReplace, actDelete, actForget, actMove, actRead} {
		rcs := byAction[a]
		sort.Slice(rcs, func(i, j int) bool { return rcs[i].Address < rcs[j].Address })
		for _, rc := range rcs {
			add(rc, a)
		}
	}
	m.rebuildRows()
}

var sectionLabel = map[string]string{
	actCreate: "＋ create", actUpdate: "～ update", actReplace: "± replace",
	actDelete: "－ destroy", actRead: "↻ read", actImport: "↓ import",
	actForget: "⊘ forget — leaves state, resource kept", actMove: "→ move", actInvoke: "▶ invoke",
}

func sectionTitle(action string, n int) string {
	return fmt.Sprintf("%s (%d)", sectionLabel[action], n)
}

// noteOf says why a node is listed: replace/delete reason, import id,
// previous address.
func noteOf(rc *ResourceChange, action string) string {
	var parts []string
	if r := prettifyReason(rc.ActionReason); r != "" && action != actForget {
		parts = append(parts, r)
	}
	if len(rc.Change.Actions) == 2 && slices.Contains(rc.Change.Actions, "forget") {
		parts = append(parts, "old object forgotten, not destroyed")
	}
	if im := rc.Change.Importing; im != nil && im.ID != "" {
		if action == actImport {
			parts = append(parts, "id "+im.ID)
		} else {
			parts = append(parts, "imported from "+im.ID)
		}
	}
	if rc.PreviousAddress != "" && rc.PreviousAddress != rc.Address {
		parts = append(parts, "moved from "+rc.PreviousAddress)
	}
	return strings.Join(parts, ", ")
}

// keyWidth is the widest attribute key in a node, capped so one long path
// doesn't push every value off screen.
func keyWidth(lines []diffLine) int {
	w := 0
	for _, d := range lines {
		if n := len([]rune(d.key)); n > w {
			w = n
		}
	}
	return min(w, 32)
}

func (m *model) rebuildRows() {
	m.rows = m.rows[:0]
	counts := map[string]int{}
	for _, n := range m.nodes {
		counts[n.action]++
	}
	last := ""
	for i, n := range m.nodes {
		if n.action != last {
			if last != "" {
				m.rows = append(m.rows, row{kind: rowBlank})
			}
			m.rows = append(m.rows, row{kind: rowSection, text: sectionTitle(n.action, counts[n.action]), nodeIdx: -1, action: n.action})
			last = n.action
		}
		caret := "▸"
		if n.expanded {
			caret = "▾"
		}
		m.rows = append(m.rows, row{kind: rowNode, text: caret + " " + n.addr, note: n.reason, nodeIdx: i, action: n.action})
		if n.expanded {
			if len(n.detail) == 0 {
				m.rows = append(m.rows, row{kind: rowDetail, text: "(no attribute changes)", sym: ' ', nodeIdx: i})
			}
			kw := keyWidth(n.detail)
			for _, d := range n.detail {
				m.rows = append(m.rows, row{kind: rowDetail, diffLine: d, keyW: kw, nodeIdx: i})
			}
		}
	}

	// actions to run
	if n := len(m.plan.ActionInvocations); n > 0 {
		m.rows = append(m.rows, row{kind: rowBlank}, row{kind: rowSection, text: sectionTitle(actInvoke, n), nodeIdx: -1, action: actInvoke})
		for _, ai := range m.plan.ActionInvocations {
			m.rows = append(m.rows, row{kind: rowDetail, text: clean(ai.Address), sym: ' ', nodeIdx: -1})
		}
	}

	// outputs section
	if len(m.plan.OutputChanges) > 0 {
		names := make([]string, 0, len(m.plan.OutputChanges))
		for name, ch := range m.plan.OutputChanges {
			if actionOf(ch.Actions) != actNoop {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			m.rows = append(m.rows, row{kind: rowBlank}, row{kind: rowSection, text: fmt.Sprintf("⇒ outputs (%d)", len(names)), nodeIdx: -1})
			var lines []diffLine
			for _, name := range names {
				ch := m.plan.OutputChanges[name]
				sym := byte('~')
				switch actionOf(ch.Actions) {
				case actCreate:
					sym = '+'
				case actDelete:
					sym = '-'
				}
				oldStr, newStr := renderVal(ch.Before), renderVal(ch.After)
				if u := truePaths(ch.AfterUnknown); u[""] {
					newStr = "(known after apply)"
				} else if len(u) > 0 {
					newStr += " (partly known after apply)"
				}
				if b, ok := ch.AfterSensitive.(bool); ok && b {
					newStr = "(sensitive)"
				}
				if b, ok := ch.BeforeSensitive.(bool); ok && b {
					oldStr = "(sensitive)"
				}
				l := diffLine{sym: sym, key: clean(name)}
				if sym != '+' {
					l.old = oldStr
				}
				if sym != '-' {
					l.new = newStr
				}
				lines = append(lines, l)
			}
			kw := keyWidth(lines)
			for _, l := range lines {
				m.rows = append(m.rows, row{kind: rowDetail, diffLine: l, keyW: kw, nodeIdx: -1})
			}
		}
	}

	// drift section
	drift := m.plan.drifted()
	if len(drift) > 0 {
		m.rows = append(m.rows, row{kind: rowBlank}, row{kind: rowSection, text: fmt.Sprintf("≠ drift detected outside terraform (%d)", len(drift)), nodeIdx: -1})
		for _, rc := range drift {
			sym, what := byte('~'), "changed"
			if actionOf(rc.Change.Actions) == actDelete {
				sym, what = '-', "deleted"
			}
			m.rows = append(m.rows, row{kind: rowDetail, text: clean(rc.Address) + "  (" + what + ")", sym: sym, nodeIdx: -1})
		}
	}

	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.clampScroll()
}

func prettifyReason(r string) string {
	if r == "" {
		return ""
	}
	r = strings.TrimPrefix(r, "replace_")
	return strings.ReplaceAll(r, "_", " ")
}

func (m *model) bodyHeight() int {
	return max(m.height-5, 3)
}

func (m *model) clampScroll() {
	h := m.bodyHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *model) moveCursor(delta int) {
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	m.clampScroll()
}

func (m *model) toggleAt(cursor int) {
	if cursor < 0 || cursor >= len(m.rows) {
		return
	}
	r := m.rows[cursor]
	if r.kind == rowBlank || r.nodeIdx < 0 {
		return // blank rows carry no node (their zero nodeIdx would open node 0)
	}
	n := m.nodes[r.nodeIdx]
	n.expanded = !n.expanded
	// keep the cursor on the node's header row
	m.rebuildRows()
	for i, rr := range m.rows {
		if rr.kind == rowNode && rr.nodeIdx == r.nodeIdx {
			m.cursor = i
			break
		}
	}
	m.clampScroll()
}

func (m *model) setAllExpanded(v bool) {
	for _, n := range m.nodes {
		n.expanded = v
	}
	m.rebuildRows()
}

// ---- keys ----

func (m model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.stage == stagePrompt {
		return m.handlePromptKey(k) // typed text, so q must not quit
	}
	key := k.String()

	// interrupt handling for running stages
	if key == "ctrl+c" || (key == "q" && m.stage != stageReview) {
		switch m.stage {
		case stagePlanning, stageApplying:
			// a second interrupt is terraform's own "stop now"; SIGKILL would
			// leave the state lock held
			m.interrupted = true
			m.proc.signal(os.Interrupt)
			return m, nil
		default:
			m.declined()
			m.quitting = true
			return m, tea.Quit
		}
	}

	if m.stage != stageReview {
		return m, nil
	}

	if m.confirm {
		switch key {
		case "y", "Y": // not enter: it expands rows, and terraform wants an explicit yes
			m.confirm = false
			return m.beginApply()
		default:
			m.confirm = false
			return m, nil
		}
	}

	switch key {
	case "q", "esc":
		m.planRecap()
		m.declined()
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "pgup", "ctrl+u":
		m.moveCursor(-m.bodyHeight())
	case "pgdown", "ctrl+d":
		m.moveCursor(m.bodyHeight())
	case "g", "home":
		m.cursor = 0
		m.clampScroll()
	case "G", "end":
		m.cursor = len(m.rows) - 1
		m.clampScroll()
	case "enter", " ", "l", "h", "right", "left":
		m.toggleAt(m.cursor)
	case "e":
		m.setAllExpanded(true)
	case "c":
		m.setAllExpanded(false)
	case "a":
		m.confirm = true
	}
	return m, nil
}

// ---- required-variable prompt ----

var unsetVarRe = regexp.MustCompile(`input variable "([^"]+)" is not set`)

// unsetVars returns the required variables a failed plan reported as unset.
// It returns nil when the failure was anything else, or when a variable we
// already supplied is still reported (a bad value must not loop forever).
func (m model) unsetVars() []pendingVar {
	var out []pendingVar
	for _, d := range m.diags {
		if d.Severity != "error" {
			continue
		}
		sm := unsetVarRe.FindStringSubmatch(d.Detail)
		if d.Summary != "No value for required variable" || sm == nil {
			return nil
		}
		for _, v := range m.vars {
			if strings.HasPrefix(v, "TF_VAR_"+sm[1]+"=") {
				return nil
			}
		}
		pv := pendingVar{name: sm[1]}
		if d.Range != nil {
			pv.file, pv.line = d.Range.Filename, d.Range.Start.Line
			pv.sensitive = declSensitive(m.chdir(), pv.file, pv.line)
		}
		out = append(out, pv)
	}
	// terraform reports them in random order; ask in declaration order
	slices.SortFunc(out, func(a, b pendingVar) int {
		return cmp.Or(strings.Compare(a.file, b.file), cmp.Compare(a.line, b.line))
	})
	return out
}

var secretNameRe = regexp.MustCompile(`(?i)pass|pw|secret|token|key|cred|auth`)
var sensitiveDeclRe = regexp.MustCompile(`\bsensitive\s*=\s*true\b`)

// masked reports whether the typed value should be hidden: the variable is
// declared sensitive (as terraform's own prompt checks), or its name looks
// secret when the declaration can't be read.
func (v pendingVar) masked() bool { return v.sensitive || secretNameRe.MatchString(v.name) }

// chdir is the directory given with the global -chdir= option, if any.
func (m model) chdir() string {
	for _, g := range m.cfg.o.globals {
		if dir, ok := strings.CutPrefix(g, "-chdir="); ok {
			return dir
		}
	}
	return ""
}

// declSensitive reports whether the variable block starting at file:line has
// sensitive = true. terraform's diagnostics don't say.
func declSensitive(dir, file string, line int) bool {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil || line < 1 {
		return false
	}
	lines := strings.Split(string(b), "\n")
	var blk strings.Builder
	depth, opened := 0, false
	for i := line - 1; i < len(lines) && i < line+199; i++ {
		blk.WriteString(lines[i] + "\n")
		depth += strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		opened = opened || strings.Contains(lines[i], "{")
		if opened && depth <= 0 {
			break
		}
	}
	return sensitiveDeclRe.MatchString(blk.String())
}

func (m model) handlePromptKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyCtrlC, tea.KeyEsc:
		// give up: report the plan failure exactly as if there were no prompt
		m.exitCode = 1
		m.final = append(m.final, stRedBold.Render("✗ Plan failed."))
		m.appendDiagRecap()
		m.stage, m.quitting = stageDone, true
		return m, tea.Quit
	case tea.KeyEnter:
		return m.submitVar() // empty is a valid value, as in terraform's own prompt
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
	case tea.KeyCtrlU:
		m.input = ""
	case tea.KeySpace:
		m.input += " "
	case tea.KeyRunes:
		m.input += strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1 // pasted newlines
			}
			return r
		}, string(k.Runes))
	}
	return m, nil
}

// submitVar stores the typed value and moves on: next variable, or once the
// queue is empty, run the plan again.
func (m model) submitVar() (tea.Model, tea.Cmd) {
	m.vars = append(m.vars, "TF_VAR_"+m.queue[0].name+"="+m.input)
	m.queue, m.input = m.queue[1:], ""
	if len(m.queue) > 0 {
		return m, nil
	}
	m.stage = stagePlanning
	m.diags, m.tasks, m.order = nil, map[string]*task{}, nil
	m.refreshed, m.plannedSeen = 0, 0
	m.startedAt = time.Now()
	return m, tea.Batch(m.startPlanCmd(), waitFor(m.events))
}

// ---- recaps ----

// declined records leaving the review without applying. Like terraform's own
// "Apply cancelled." that is a failure for apply and destroy, so `tf apply &&
// next` doesn't carry on.
func (m *model) declined() {
	if m.cfg.sub != "plan" {
		m.exitCode = 1
		m.final = append(m.final, stDim.Render("Apply cancelled."))
	}
}

func (m *model) planRecap() {
	c := m.counts
	line := fmt.Sprintf("Plan: %d to add, %d to change, %d to destroy", c.add+c.replace, c.change, c.destroy+c.replace)
	if c.imports > 0 {
		line = fmt.Sprintf("Plan: %d to import, %d to add, %d to change, %d to destroy", c.imports, c.add+c.replace, c.change, c.destroy+c.replace)
	}
	if c.replace > 0 {
		line += fmt.Sprintf(" (%d replaced)", c.replace)
	}
	m.final = append(m.final, stBold.Render(line+"."))
	if m.cfg.keepPlan {
		apply := strings.Join(append(slices.Clone(m.cfg.o.globals), "apply", m.cfg.planPath), " ") // keeps -chdir
		m.final = append(m.final, stDim.Render("Plan saved to "+m.cfg.planPath+" — apply with: tf "+apply))
	}
	m.appendDiagRecap()
}

func (m *model) appendDiagRecap() {
	seen := map[string]bool{} // terraform repeats some warnings at apply
	for _, d := range append(append([]*Diagnostic{}, m.carried...), m.diags...) {
		key := d.Severity + "|" + d.Address + "|" + d.Summary + "|" + d.Detail
		if seen[key] {
			continue
		}
		seen[key] = true
		m.final = append(m.final, renderDiagLines(d, m.width, true)...)
	}
}

func (m *model) appendOutputsRecap() {
	if len(m.outputs) == 0 {
		return
	}
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(m.outputs)) {
		o := m.outputs[name]
		val := "(sensitive)"
		if !o.Sensitive {
			val = truncRunes(clean(string(o.Value)), 120)
		}
		if strings.TrimSpace(val) == "" || val == "null" {
			continue
		}
		lines = append(lines, "  "+stCyan.Render(clean(name))+" = "+val)
	}
	if len(lines) > 0 {
		m.final = append(m.final, stBold.Render("Outputs:"))
		m.final = append(m.final, lines...)
	}
}
