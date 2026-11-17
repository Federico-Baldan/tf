package main

// Structures for `terraform show -json <planfile>` output. Only the fields
// the review tree needs.

type Plan struct {
	ResourceChanges []*ResourceChange      `json:"resource_changes"`
	OutputChanges   map[string]*ChangeRepr `json:"output_changes"`
	ResourceDrift   []*ResourceChange      `json:"resource_drift"`
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
)

func actionOf(actions []string) string {
	if len(actions) == 2 {
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
		}
	}
	return c
}

// total is everything worth reviewing and applying.
func (c planCounts) total() int {
	return c.add + c.change + c.destroy + c.replace + c.stateOnly
}

// hooks is how many apply start/complete pairs terraform will emit. A
// replace runs twice (delete, then create); state-only changes run none.
func (c planCounts) hooks() int {
	return c.add + c.change + c.destroy + 2*c.replace
}
