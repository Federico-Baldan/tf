package main

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Equivalence checks for the Go 1.27 fast paths: parsePlan must decode exactly
// like json.Unmarshal, renderVal must print exactly like the per-value
// json.Encoder it replaced.

// terraform writes numbers in full, without exponent: 1e400 is a 1 and 400 zeros
var (
	bigNumber   = "1" + strings.Repeat("0", 400)
	tinyNumber  = "0." + strings.Repeat("0", 399) + "1"
	maxFloatLit = "17976931348623157" + strings.Repeat("0", 292)
)

func decodeV1(b []byte) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func sameDecode(t *testing.T, name string, in []byte) {
	t.Helper()
	got, gotErr := parsePlan(in)
	want, wantErr := decodeV1(in)
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
		t.Errorf("%s: error %v, json.Unmarshal %v", name, gotErr, wantErr)
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: parsePlan and json.Unmarshal disagree", name)
	}
}

// miniPlan is n resources shaped like real `terraform show -json` output.
func miniPlan(n int) []byte {
	var rcs []any
	for i := 0; i < n; i++ {
		rcs = append(rcs, map[string]any{"address": fmt.Sprintf("t.r[%d]", i), "change": map[string]any{
			"actions": []string{"update"}, "before": map[string]any{"id": fmt.Sprint(i)},
			"after": map[string]any{"id": fmt.Sprint(i), "n": float64(i) + 0.5, "cidrs": []any{"10.0.0.0/16", nil},
				"tags": map[string]any{"env": "prod", "<k>": "&"}},
			"after_unknown": map[string]any{"arn": true}, "replace_paths": []any{[]any{"id"}},
		}})
	}
	b, _ := json.Marshal(map[string]any{"resource_changes": rcs, "applyable": true,
		// members the Plan type does not declare, as real output has
		"format_version": "1.2", "terraform_version": "1.17.2", "planned_values": map[string]any{"root_module": map[string]any{}}})
	return b
}

// planEdgeCases are inputs where json/v2 and v1 could plausibly part ways.
var planEdgeCases = []string{
	`{}`, `null`, `[]`, `""`, `1`, ``, ` `, `{"resource_changes":null,"output_changes":null}`,
	`{"resource_changes":[],"output_changes":{},"resource_drift":[]}`,
	`{"resource_changes":[null,{"address":"a","change":null}]}`,
	`{"resource_changes":[{"address":"a","change":{"actions":null,"before":null,"after":{},"after_unknown":[],"replace_paths":[]}}]}`,
	// numbers into any: big, small, negative zero, exponents, out of range
	`{"resource_changes":[{"address":"n","change":{"actions":["update"],"before":{"a":12345678901234567890,"b":1e-320,"c":-0,"d":1.5E+300,"e":0.1},"after":[1e308,-1e308,123456789012345678]}}]}`,
	`{"resource_changes":[{"address":"n","change":{"before":1e400}}]}`,
	`{"resource_changes":[{"address":"n","change":{"replace_paths":[["a",0,1e400]]}}]}`,
	// strings: escapes, unicode, surrogates, invalid UTF-8
	`{"resource_changes":[{"address":"A\"\\\/\b\f\n\r\t  <>&","change":{"before":"🎉 è 漢","after":"\ud800 lone \udfff"}}]}`,
	"{\"resource_changes\":[{\"address\":\"bad\xff\xfeutf8\",\"change\":{\"before\":{\"k\xff\":\"v\xc3\x28\"}}}]}",
	"{\"resource_changes\":[{\"address\":\"x\",\"change\":{\"before\":{\"a\xff\":1,\"a\xfe\":2}}}]}",
	// duplicate names: v1 keeps the last, json/v2 rejects them
	`{"applyable":true,"applyable":false}`,
	`{"resource_changes":[{"address":"a","address":"b"}]}`,
	`{"resource_changes":[{"address":"a","change":{"actions":["create"]},"change":{"before":1}}]}`,
	`{"resource_changes":[{"address":"a","change":{"before":{"k":1,"k":{"x":2}}}}]}`,
	`{"resource_changes":[{"address":"a","address":null}]}`,
	// names matched without regard to case, as v1 does
	`{"Resource_Changes":[{"ADDRESS":"a","Change":{"Actions":["create"]}}],"APPLYABLE":true}`,
	`{"resource_changes":[{"address":"a","Address":"b"}]}`,
	// wrong types, trailing data, truncation, syntax errors
	`{"applyable":"yes"}`, `{"resource_changes":{}}`, `{"resource_changes":[{"change":{"actions":"create"}}]}`,
	`{"output_changes":{"a":[]}}`, `{"applyable":true} {}`, `{"applyable":true}x`, `{"applyable":tru}`,
	`{"resource_changes":[{"address":"a"`, `{"a":1,}`, `[1,2`, `{"a" 1}`,
	`{"resource_changes":[{"change":{"importing":{"id":7}}}]}`,
	// unknown fields of every kind are skipped
	`{"format_version":"1.2","planned_values":{"root_module":{"resources":[{"x":[1,{"y":null}]}]}},"configuration":{},"applyable":true}`,
	// numbers as terraform writes them: in full, possibly beyond float64
	`{"resource_changes":[{"address":"n","change":{"after":{"big":` + bigNumber + `,"tiny":` + tinyNumber + `,"max":` + maxFloatLit + `}}}]}`,
	`{"resource_changes":[{"address":"n","change":{"after":{"tiny":` + tinyNumber + `,"max":` + maxFloatLit + `}}}]}`,
	`{"planned_values":{"outputs":{"big":{"value":` + bigNumber + `}}},"applyable":true}`,
	// syntax errors inside members that are only skipped
	`{"planned_values":{"a":tru},"applyable":true}`, `{"configuration":{"a":[1,]},"applyable":true}`,
	"{\"prior_state\":\"raw\x01control\",\"applyable\":true}", `{"prior_state":01,"applyable":true}`,
	`{"prior_state":"\q","applyable":true}`, "\ufeff{\"applyable\":true}", `{"prior_state":NaN}`,
	// map keys that differ only in case are distinct keys, not one field
	`{"output_changes":{"Value":{"actions":["create"],"after":"A"},"value":{"actions":["create"],"after":"a"}}}`,
	`{"resource_changes":[{"address":"a","change":{"after":{"tags":{"Name":"A","name":"a","NAME":"N"}}}}]}`,
}

func TestParsePlanMatchesV1(t *testing.T) {
	sameDecode(t, "planFixture", []byte(planFixture))
	for _, n := range []int{0, 1, 50} {
		sameDecode(t, fmt.Sprintf("miniPlan(%d)", n), miniPlan(n))
	}
	for i, c := range planEdgeCases {
		sameDecode(t, fmt.Sprintf("edge %d %.40q", i, c), []byte(c))
	}
}

// Real terraform output must stay on the fast path: if it needed the
// json.Unmarshal fallback the speedup would be gone.
func TestParsePlanStaysOnFastPath(t *testing.T) {
	for name, in := range map[string][]byte{
		"planFixture": []byte(planFixture), "miniPlan": miniPlan(50),
		// members the program never reads are skipped, not converted
		"bigNumberSkipped": []byte(`{"planned_values":{"outputs":{"big":{"value":` + bigNumber + `}}},"applyable":true}`),
		"caseOnlyMapKeys":  []byte(`{"output_changes":{"Value":{"actions":["create"]},"value":{"actions":["create"]}}}`),
	} {
		var p Plan
		if err := jsonv2.Unmarshal(in, &p, fastPlanJSON); err != nil {
			t.Errorf("%s: fast decode failed: %v", name, err)
		}
	}
}

// The fast pass is only fast while both v1 options it drops stay dropped:
// with duplicate names allowed json/v2 skips its decoder for any values
// (go.dev/issue/80270), and with legacy errors it validates the whole input
// first. Both changes would keep results identical, so pin them here.
func TestFastPlanJSONOptions(t *testing.T) {
	var p Plan
	err := jsonv2.Unmarshal([]byte(`{"resource_changes":[{"change":{"after":{"k":1,"k":2}}}]}`), &p, fastPlanJSON)
	if err == nil {
		t.Error("fastPlanJSON accepts duplicate names; json/v2 takes its slow path for any")
	}
	err = jsonv2.Unmarshal([]byte(`{"applyable":"yes"}`), &p, fastPlanJSON)
	var se *jsonv2.SemanticError
	if !errors.As(err, &se) {
		t.Errorf("fastPlanJSON reports %T, want *json.SemanticError: legacy error semantics are back on", err)
	}
	// the value an out-of-range number leaves behind never escapes parsePlan
	if got, err := parsePlan([]byte(`{"resource_changes":[{"address":"a","change":{"after":` + bigNumber + `}}]}`)); got != nil || err == nil {
		t.Errorf("parsePlan(out of range) = %v, %v; want nil plan and json.Unmarshal's error", got, err)
	}
}

// renderValV1 is renderVal as it was: a fresh Encoder per value.
func renderValV1(v any) string {
	if v == nil {
		return "null"
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%v", v)
	}
	return truncRunes(clean(strings.TrimSuffix(buf.String(), "\n")), maxDiffValueLen)
}

func renderValCases(t *testing.T) []any {
	vals := []any{
		nil, true, false, 0.0, math.Copysign(0, -1), 1.0, -1.5, 0.1, 1e20, 1e21, 1e-6, 1e-7,
		123456789012345678.0, 1.7976931348623157e308, 5e-324, float64(1 << 53),
		"", "plain", `quote " backslash \ slash /`, "<script>a && b</script>", "\x00\x01\x1f\x7f",
		"esc \x1b[31m red", "c1 \u0085 \u009b", "  ", "tab\tnl\ncr\r", "bad \xff\xfe utf8 \xc3\x28",
		"퟿\U0010ffff", "🎉 è 漢字", strings.Repeat("x", 99), strings.Repeat("x", 100),
		strings.Repeat("x", 101), strings.Repeat("è", 150), "a" + strings.Repeat("🎉", 120),
		map[string]any{}, []any{}, []any{nil, 1.0, "a", []any{}, map[string]any{}},
		map[string]any{"z": 1.0, "a": map[string]any{"y": []any{true, nil}, "b": "<&>"}, "m": " "},
		map[string]any{"k\xff": "v", "\x1b": "esc", "app.kubernetes.io/name": "x", "": "empty"},
		// U+2028/2029 stay escaped; keys sort by UTF-8 bytes, not UTF-16 units
		"line\u2028sep\u2029para", map[string]any{"ｆｕｌｌ": 1.0, "🎉": 2.0, "Key": "K", "key": "k", "\u2028": "ls", "\ufeff": "bom"},
		// Go values that never come out of json.Unmarshal still behave the same
		1, int64(-7), []string{"a"}, map[string]string{"b": "<"}, json.Number("12.50"),
		json.RawMessage(`{"b":1,"a":"<"}`), math.NaN(), math.Inf(1), make(chan int),
	}
	// every value, containers included, in the fixtures and a generated plan
	for _, js := range [][]byte{miniPlan(20), []byte(planFixture)} {
		p, err := decodeV1(js)
		if err != nil {
			t.Fatal(err)
		}
		var changes []*ChangeRepr
		for _, rc := range append(p.ResourceChanges, p.ResourceDrift...) {
			changes = append(changes, rc.Change)
		}
		for _, ch := range p.OutputChanges {
			changes = append(changes, ch)
		}
		for _, ch := range changes {
			for _, root := range []any{ch.Before, ch.After, ch.AfterUnknown, ch.BeforeSensitive, ch.AfterSensitive} {
				all := map[string]any{}
				subtrees("", root, all)
				for _, v := range all {
					vals = append(vals, v)
				}
			}
		}
	}
	return vals
}

func TestRenderValMatchesEncoder(t *testing.T) {
	vals := renderValCases(t)
	for _, v := range vals {
		if got, want := renderVal(v), renderValV1(v); got != want {
			t.Errorf("renderVal(%#v) = %q, want %q", v, got, want)
		}
	}
	// and from many goroutines (run with -race)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for _, v := range vals {
				if got, want := renderVal(v), renderValV1(v); got != want {
					t.Errorf("concurrent renderVal(%#v) = %q, want %q", v, got, want)
					return
				}
			}
		})
	}
	wg.Wait()
}
