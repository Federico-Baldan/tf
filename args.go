package main

import "strings"

type opts struct {
	globals     []string // args before the subcommand (e.g. -chdir=...)
	varEnv      []string // -var values as TF_VAR_* entries, replayed at apply (ephemeral variables need them there too)
	planArgs    []string // flags passed to the plan step
	shared      []string // flags passed to both plan and apply steps
	autoApprove bool
	planOut     string // user-supplied -out path (we keep the file)
	planFileArg string // positional plan file given to `apply`
	refreshOnly bool
	noInput     bool // -input=false: never prompt for variables
}

// flags that take a value (possibly space-separated)
var valueFlags = map[string]bool{
	"-var": true, "-var-file": true, "-target": true, "-exclude": true,
	"-replace": true, "-out": true, "-backup": true, "-state": true,
	"-state-out": true, "-lock-timeout": true, "-parallelism": true,
	"-generate-config-out": true, "-invoke": true,
}

// flags that should also be passed to the apply-plan-file step
var sharedFlags = map[string]bool{
	"-parallelism": true, "-lock": true, "-lock-timeout": true,
	// a saved plan must be applied against the same state files it was made from
	"-state": true, "-state-out": true, "-backup": true,
}

// flags we manage ourselves and must strip from user args
var strippedFlags = map[string]bool{
	"-json": true, "-no-color": true, "-input": true,
	"-detailed-exitcode": true, "-compact-warnings": true,
}

func parseArgs(sub string, rest []string) opts {
	var o opts
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if !strings.HasPrefix(a, "-") {
			if sub == "apply" {
				o.planFileArg = a
			}
			continue
		}
		name, val, hasEq := strings.Cut(a, "=")
		name = "-" + strings.TrimLeft(name, "-")

		tokens := []string{a}
		if valueFlags[name] && !hasEq && i+1 < len(rest) {
			i++
			val = rest[i]
			tokens = append(tokens, val)
		}

		switch {
		case name == "-var":
			if k, v, ok := strings.Cut(val, "="); ok {
				o.varEnv = append(o.varEnv, "TF_VAR_"+k+"="+v)
			}
		case name == "-refresh-only":
			o.refreshOnly = true
		case name == "-input" && val == "false":
			o.noInput = true
		}

		switch {
		case name == "-auto-approve":
			o.autoApprove = true
		case name == "-out":
			o.planOut = val
		case strippedFlags[name]:
			// managed by the wrapper
		case sharedFlags[name]:
			o.shared = append(o.shared, tokens...)
		default:
			o.planArgs = append(o.planArgs, tokens...)
		}
	}
	return o
}
