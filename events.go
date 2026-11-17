package main

import "encoding/json"

// Event is one line of terraform's machine-readable UI output (-json).
type Event struct {
	Type       string                 `json:"type"`
	Hook       *Hook                  `json:"hook"`
	Diagnostic *Diagnostic            `json:"diagnostic"`
	Changes    *ChangeSummary         `json:"changes"`
	Outputs    map[string]*OutputMeta `json:"outputs"`
}

type Hook struct {
	Resource       *HookResource `json:"resource"`
	Action         string        `json:"action"`
	ElapsedSeconds float64       `json:"elapsed_seconds"`
}

type HookResource struct {
	Addr string `json:"addr"`
}

type Diagnostic struct {
	Severity string     `json:"severity"`
	Summary  string     `json:"summary"`
	Detail   string     `json:"detail"`
	Address  string     `json:"address"`
	Range    *DiagRange `json:"range"`
}

type DiagRange struct {
	Filename string `json:"filename"`
	Start    struct {
		Line int `json:"line"`
	} `json:"start"`
}

type ChangeSummary struct {
	Add    int `json:"add"`
	Change int `json:"change"`
	Import int `json:"import"`
	// actions run during apply
	ActionInvocation int `json:"action_invocation"`
	Remove           int `json:"remove"`
}

type OutputMeta struct {
	Sensitive bool            `json:"sensitive"`
	Value     json.RawMessage `json:"value"`
}

func parseEvent(line []byte) *Event {
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}
	return &ev
}
