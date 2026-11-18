package main

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A single attribute-level diff line inside an expanded resource node.
// Attribute lines carry key/old/new so the view can colour and align them;
// free-form lines (notices) use text alone.
type diffLine struct {
	sym      byte // '+', '-', '~'
	key      string
	old, new string
	text     string
	forces   bool // attribute forces replacement
}

const maxDiffValueLen = 100
const maxDiffLines = 400

// Value paths. Each step is introduced by a control character that cannot
// occur in a real key, so keys containing "." or "[" (kubernetes labels like
// "app.kubernetes.io/name") can't be mistaken for nesting. display() turns a
// path into the usual a.b[0].c form.
const (
	keyStep = "\x1f" // a map key / object attribute follows
	idxStep = "\x1e" // a list index follows
)

func joinPath(prefix, key string) string  { return prefix + keyStep + key }
func idxPath(prefix string, i int) string { return prefix + idxStep + strconv.Itoa(i) }

var idxRe = regexp.MustCompile(idxStep + "([0-9]+)")

func display(p string) string {
	p = idxRe.ReplaceAllString(p, "[$1]")
	return clean(strings.TrimPrefix(strings.ReplaceAll(p, keyStep, "."), "."))
}

// flatten walks an arbitrary decoded-JSON value, producing leaf paths.
func flatten(prefix string, v any, out map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			out[prefix] = t
			return
		}
		for k, vv := range t {
			flatten(joinPath(prefix, k), vv, out)
		}
	case []any:
		if len(t) == 0 {
			out[prefix] = t
			return
		}
		for i, vv := range t {
			flatten(idxPath(prefix, i), vv, out)
		}
	default:
		out[prefix] = v
	}
}

// subtrees records every value in v, containers included, under the same
// paths flatten uses, so a collapsed collection can be rendered by lookup.
func subtrees(prefix string, v any, out map[string]any) {
	out[prefix] = v
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			subtrees(joinPath(prefix, k), vv, out)
		}
	case []any:
		for i, vv := range t {
			subtrees(idxPath(prefix, i), vv, out)
		}
	}
}

// parentPath drops the last path step (a top-level key's parent is the root "").
func parentPath(p string) (string, bool) {
	i := strings.LastIndexAny(p, keyStep+idxStep)
	if i < 0 {
		return "", false
	}
	return p[:i], true
}

// marked reports whether path, or any container above it, is flagged.
// terraform flags a whole sensitive or unknown collection once, at the
// collection itself, not on each element inside it.
func marked(set map[string]bool, path string) bool {
	if len(set) == 0 {
		return false
	}
	for p, ok := path, true; ok; p, ok = parentPath(p) {
		if set[p] {
			return true
		}
	}
	return false
}

// anyUnder reports whether path itself or anything inside it is flagged.
func anyUnder(set map[string]bool, path string) bool {
	for p := range set {
		if p == path || strings.HasPrefix(p, path+keyStep) || strings.HasPrefix(p, path+idxStep) {
			return true
		}
	}
	return false
}

// truePaths collects paths whose leaf value is `true` (used for
// after_unknown and *_sensitive structures).
func truePaths(v any) map[string]bool {
	flat := map[string]any{}
	flatten("", v, flat)
	out := map[string]bool{}
	for k, vv := range flat {
		if b, ok := vv.(bool); ok && b {
			out[k] = true
		}
	}
	return out
}

// renderVal prints a value as compact JSON, without HTML escaping (so & < >
// stay readable) and cut on rune boundaries.
func renderVal(v any) string {
	if v == nil {
		return "null"
	}
	b, err := jsonv2.Marshal(v, valJSON)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return truncRunes(clean(string(b)), maxDiffValueLen)
}

// valJSON is encoding/json without HTML escaping, the same as an Encoder
// after SetEscapeHTML(false), but built once instead of per value.
var valJSON = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsontext.EscapeForHTML(false))

// replacePathPrefixes converts terraform's replace_paths (a list of steps:
// strings are keys, numbers are indexes) to our path form.
func replacePathPrefixes(rps [][]any) []string {
	var out []string
	for _, rp := range rps {
		p := ""
		for _, step := range rp {
			switch s := step.(type) {
			case string:
				p = joinPath(p, s)
			case float64:
				p = idxPath(p, int(s))
			}
		}
		out = append(out, p)
	}
	return out
}

// diffResource computes attribute-level diff lines for a resource change.
func diffResource(ch *ChangeRepr) []diffLine {
	before := map[string]any{}
	after := map[string]any{}
	flatten("", ch.Before, before)
	flatten("", ch.After, after)
	beforeAll := map[string]any{}
	subtrees("", ch.Before, beforeAll)
	unknown := truePaths(ch.AfterUnknown)
	sensB := truePaths(ch.BeforeSensitive)
	sensA := truePaths(ch.AfterSensitive)
	forces := replacePathPrefixes(ch.ReplacePaths)

	keySet := map[string]bool{}
	for k := range before {
		keySet[k] = true
	}
	for k := range after {
		keySet[k] = true
	}
	for k := range unknown {
		keySet[k] = true
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		if k != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)

	forcesRepl := func(path string) bool {
		for _, p := range forces {
			if p != "" && (path == p || strings.HasPrefix(path, p+keyStep) || strings.HasPrefix(path, p+idxStep)) {
				return true
			}
		}
		return false
	}

	var lines []diffLine
	for _, k := range keys {
		if len(lines) >= maxDiffLines {
			lines = append(lines, diffLine{sym: '~', text: "… (diff truncated)"})
			break
		}
		// inside a collection that became unknown: reported once, at the collection
		if par, ok := parentPath(k); ok && marked(unknown, par) {
			continue
		}
		b, hasB := before[k]
		a, hasA := after[k]
		unk := unknown[k]
		hidden := false // old value contains something sensitive
		if !hasB && unk {
			// a whole object/list that existed before is now unknown
			if sub, ok := beforeAll[k]; ok {
				b, hasB = sub, true
				hidden = anyUnder(sensB, k)
			}
		}

		oldStr := renderVal(b)
		newStr := renderVal(a)
		if hidden || marked(sensB, k) {
			oldStr = "(sensitive)"
		}
		if marked(sensA, k) {
			newStr = "(sensitive)"
		}
		if unk {
			newStr = "(known after apply)"
		}

		switch {
		case hasB && (hasA || unk):
			if !unk && reflect.DeepEqual(a, b) {
				continue // unchanged
			}
			lines = append(lines, diffLine{sym: '~', key: display(k), old: oldStr, new: newStr, forces: forcesRepl(k)})
		case !hasB && (hasA || unk):
			lines = append(lines, diffLine{sym: '+', key: display(k), new: newStr})
		case hasB:
			lines = append(lines, diffLine{sym: '-', key: display(k), old: oldStr})
		}
	}
	return lines
}
