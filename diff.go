package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
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

// flatten walks an arbitrary decoded-JSON value, producing dotted leaf paths.
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
			flatten(fmt.Sprintf("%s[%d]", prefix, i), vv, out)
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
			subtrees(fmt.Sprintf("%s[%d]", prefix, i), vv, out)
		}
	}
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// parentPath drops the last path step.
func parentPath(p string) (string, bool) {
	i := strings.LastIndexAny(p, ".[")
	if i < 0 {
		return "", false
	}
	return p[:i], true
}

// marked reports whether path, or any container above it, is flagged.
// terraform flags a whole sensitive or unknown collection once, at the
// collection itself, not on each element inside it.
func marked(set map[string]bool, path string) bool {
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
		if p == path || strings.HasPrefix(p, path+".") || strings.HasPrefix(p, path+"[") {
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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := strings.ReplaceAll(strings.TrimSuffix(buf.String(), "\n"), "\\n", "↵")
	return truncRunes(s, maxDiffValueLen)
}

func replacePathPrefixes(rps [][]any) []string {
	var out []string
	for _, rp := range rps {
		var parts []string
		for _, step := range rp {
			switch s := step.(type) {
			case string:
				parts = append(parts, s)
			case float64:
				if len(parts) > 0 {
					parts[len(parts)-1] = fmt.Sprintf("%s[%d]", parts[len(parts)-1], int(s))
				}
			}
		}
		out = append(out, strings.Join(parts, "."))
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
	sort.Strings(keys)

	forcesRepl := func(path string) bool {
		for _, p := range forces {
			if p != "" && (path == p || strings.HasPrefix(path, p+".") || strings.HasPrefix(path, p+"[")) {
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
			lines = append(lines, diffLine{sym: '~', key: k, old: oldStr, new: newStr, forces: forcesRepl(k)})
		case !hasB && (hasA || unk):
			lines = append(lines, diffLine{sym: '+', key: k, new: newStr})
		case hasB:
			lines = append(lines, diffLine{sym: '-', key: k, old: oldStr})
		}
	}
	return lines
}
