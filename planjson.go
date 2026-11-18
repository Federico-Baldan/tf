package main

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"slices"
)

// Structures for `terraform show -json <planfile>` output. Only the fields
// the review tree needs.

type Plan struct {
	ResourceChanges []*ResourceChange      `json:"resource_changes"`
	OutputChanges   map[string]*ChangeRepr `json:"output_changes"`
	ResourceDrift   []*ResourceChange      `json:"resource_drift"`
	Applyable       bool                   `json:"applyable"` // false when there is nothing to apply
	// actions (terraform 1.14+) to run on apply, e.g. via -invoke
	ActionInvocations []*ActionInvocation `json:"action_invocations"`
}

// parsePlan decodes `terraform show -json` output exactly as json.Unmarshal
// does, about 1.4x faster on real plans on Go 1.27. Plan values are mostly untyped (any),
// and encoding/json keeps json/v2 off its fast path for those while it
// accepts duplicate names (go.dev/issue/80270); it also validates the whole
// input before decoding. fastPlanJSON drops just those two v1 options.
// terraform never writes duplicate names, and anything the fast pass
// rejects is decoded again by json.Unmarshal, so results and errors stay v1's.
func parsePlan(b []byte) (*Plan, error) {
	var p Plan
	if jsonv2.Unmarshal(b, &p, fastPlanJSON) == nil {
		return &p, nil
	}
	p = Plan{}
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

var fastPlanJSON = jsonv2.JoinOptions(json.DefaultOptionsV1(),
	jsontext.AllowDuplicateNames(false), json.ReportErrorsWithLegacySemantics(false))

type ActionInvocation struct {
	Address string `json:"address"`
}

// drifted lists objects that changed outside terraform.
func (p *Plan) drifted() []*ResourceChange {
	var out []*ResourceChange
	for _, rc := range p.ResourceDrift {
		if actionOf(rc.Change.Actions) != actNoop {
			out = append(out, rc)
		}
	}
	return out
}

type ResourceChange struct {
	Address         string      `json:"address"`
	PreviousAddress string      `json:"previous_address"` // set when a moved block renamed it
	Change          *ChangeRepr `json:"change"`
	ActionReason    string      `json:"action_reason"`
}

type ChangeRepr struct {
	Actions         []string   `json:"actions"`
	Before          any        `json:"before"`
	After           any        `json:"after"`
	AfterUnknown    any        `json:"after_unknown"`
	BeforeSensitive any        `json:"before_sensitive"`
	AfterSensitive  any        `json:"after_sensitive"`
	ReplacePaths    [][]any    `json:"replace_paths"`
	Importing       *Importing `json:"importing"` // set when an import block adopts the object
}

type Importing struct {
	ID string `json:"id"`
}

const (
	actCreate  = "create"
	actUpdate  = "update"
	actDelete  = "delete"
	actReplace = "replace"
	actRead    = "read"
	actNoop    = "no-op"
	actForget  = "forget" // removed block: dropped from state, not destroyed
	// Pseudo-actions: terraform reports these as "no-op" plus a side marker.
	actImport = "import"
	actMove   = "move"
	actInvoke = "invoke" // action block run on apply
)

func actionOf(actions []string) string {
	if len(actions) == 2 {
		// lifecycle { destroy = false } makes a "replace" that forgets the old
		// object instead of destroying it: that's a create, not a delete+create
		if slices.Contains(actions, "forget") {
			return actCreate
		}
		return actReplace
	}
	if len(actions) == 1 {
		switch actions[0] {
		case "create", "update", "delete", "read", "no-op", "forget":
			return actions[0]
		}
	}
	return actNoop
}

// kindOf is actionOf plus the state-only changes terraform hides behind a
// "no-op": a bare import block or a moved block. Anything still returning
// actNoop has nothing to show.
func kindOf(rc *ResourceChange) string {
	if a := actionOf(rc.Change.Actions); a != actNoop {
		return a
	}
	switch {
	case rc.Change.Importing != nil:
		return actImport
	case rc.PreviousAddress != "" && rc.PreviousAddress != rc.Address:
		return actMove
	}
	return actNoop
}

// counts of real changes in a plan
type planCounts struct {
	add, change, destroy, replace int
	imports                       int // objects adopted by import blocks, whatever else changes
	stateOnly                     int // import-only, move and forget entries
	reads                         int // data sources read during apply
	invokes                       int // actions to run
}

func countPlan(p *Plan) planCounts {
	var c planCounts
	for _, rc := range p.ResourceChanges {
		if rc.Change.Importing != nil {
			c.imports++
		}
		switch kindOf(rc) {
		case actCreate:
			c.add++
		case actUpdate:
			c.change++
		case actDelete:
			c.destroy++
		case actReplace:
			c.replace++
		case actImport, actMove, actForget:
			c.stateOnly++
		case actRead:
			c.reads++
		}
	}
	c.invokes = len(p.ActionInvocations)
	return c
}

// total is everything worth reviewing and applying.
func (c planCounts) total() int {
	return c.add + c.change + c.destroy + c.replace + c.stateOnly + c.invokes
}

// hooks is how many apply start/complete pairs terraform will emit. A
// replace runs twice (delete, then create), a data read once; state-only
// changes run none.
func (c planCounts) hooks() int {
	return c.add + c.change + c.destroy + 2*c.replace + c.reads
}
