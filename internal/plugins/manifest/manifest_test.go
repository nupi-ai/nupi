package manifest

import (
	"math"
	"strconv"
	"testing"
)

func TestParseAdapterOptionsSuccess(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
  options:
    use_gpu:
      type: boolean
      default: true
    threads:
      type: integer
      default: 4
    voice:
      type: enum
      default: en-US
      values: [en-US, en-GB]
`

	mf, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if mf.Adapter == nil {
		t.Fatalf("expected adapter spec")
	}

	opts := mf.Adapter.Options
	if len(opts) != 3 {
		t.Fatalf("expected 3 options, got %d", len(opts))
	}

	useGPU, ok := opts["use_gpu"]
	if !ok {
		t.Fatalf("missing use_gpu option")
	}
	if val, ok := useGPU.Default.(bool); !ok || !val {
		t.Fatalf("use_gpu default mismatch: %#v", useGPU.Default)
	}

	threads, ok := opts["threads"]
	if !ok {
		t.Fatalf("missing threads option")
	}
	if val, ok := threads.Default.(int); !ok || val != 4 {
		t.Fatalf("threads default mismatch: %#v", threads.Default)
	}

	voice, ok := opts["voice"]
	if !ok {
		t.Fatalf("missing voice option")
	}
	if val, ok := voice.Default.(string); !ok || val != "en-US" {
		t.Fatalf("voice default mismatch: %#v", voice.Default)
	}
	if len(voice.Values) != 2 {
		t.Fatalf("expected 2 voice values, got %d", len(voice.Values))
	}
	if voice.Values[0] != "en-US" || voice.Values[1] != "en-GB" {
		t.Fatalf("unexpected enum values: %#v", voice.Values)
	}
}

func TestParseAdapterOptionsInvalidType(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
  options:
    bad:
      type: unsupported
`
	if _, err := Parse([]byte(data)); err == nil {
		t.Fatalf("expected error for unsupported option type")
	}
}

func TestParseAdapterOptionsInvalidEnumDefault(t *testing.T) {
	data := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: test
  slug: test
  catalog: ai.nupi
  version: 0.0.1
spec:
  slot: stt
  mode: local
  entrypoint:
    command: ./adapter
  options:
    voice:
      type: enum
      default: en-AU
      values: [en-US, en-GB]
`
	if _, err := Parse([]byte(data)); err == nil {
		t.Fatalf("expected error when enum default not in values")
	}
}

func TestInferOptionType(t *testing.T) {
	cases := map[string]struct {
		input  any
		expect string
	}{
		"nil":         {nil, ""},
		"bool":        {true, "boolean"},
		"int":         {int64(5), "integer"},
		"uint":        {uint32(10), "integer"},
		"float_int":   {float64(2.0), "integer"},
		"float":       {float64(2.5), "number"},
		"string":      {"value", "string"},
		"unsupported": {[]string{"a"}, ""},
	}

	for name, tc := range cases {
		if got := inferOptionType(tc.input); got != tc.expect {
			t.Fatalf("%s: expected %q, got %q", name, tc.expect, got)
		}
	}
}

func TestCoerceNumber(t *testing.T) {
	if val, err := coerceNumber(" 3.14 "); err != nil || val.(float64) != 3.14 {
		t.Fatalf("coerceNumber string failed: val=%v err=%v", val, err)
	}
	if val, err := coerceNumber(uint32(7)); err != nil || val.(float64) != 7 {
		t.Fatalf("coerceNumber uint failed: val=%v err=%v", val, err)
	}
	if val, err := coerceNumber(float32(1.5)); err != nil || val.(float64) != 1.5 {
		t.Fatalf("coerceNumber float32 failed: val=%v err=%v", val, err)
	}
	if _, err := coerceNumber("not-a-number"); err == nil {
		t.Fatalf("expected error for invalid number string")
	}
}

func TestCoerceInt(t *testing.T) {
	if val, err := coerceInt("42"); err != nil || val.(int) != 42 {
		t.Fatalf("expected string 42 -> 42, got %v err=%v", val, err)
	}
	if _, err := coerceInt(""); err == nil {
		t.Fatalf("expected error for empty string")
	}
	large := int64(math.MaxInt64)
	if val, err := coerceInt(large); err != nil || val.(int) != int(large) {
		t.Fatalf("expected MaxInt64 -> %d, got %v err=%v", large, val, err)
	}
	overflow := strconv.FormatUint(uint64(math.MaxInt64)+1, 10)
	if _, err := coerceInt(overflow); err == nil {
		t.Fatalf("expected overflow error")
	}
	if val, err := coerceInt(float32(8.0)); err != nil || val.(int) != 8 {
		t.Fatalf("expected float32 8.0 -> 8, got %v err=%v", val, err)
	}
	if _, err := coerceInt(3.14); err == nil {
		t.Fatalf("expected error for non-integral float")
	}
}

func TestIsFloatIntegral(t *testing.T) {
	if !isFloatIntegral(5.0) {
		t.Fatalf("expected 5.0 to be integral")
	}
	if isFloatIntegral(5.1) {
		t.Fatalf("expected 5.1 to be non-integral")
	}
}

func TestCoerceBool(t *testing.T) {
	cases := []struct {
		name   string
		input  any
		expect bool
	}{
		{"literal_true", true, true},
		{"string_true", "true", true},
		{"string_one", "1", true},
		{"literal_false", false, false},
		{"string_false", "false", false},
		{"string_zero", "0", false},
	}

	for _, tc := range cases {
		val, err := coerceBool(tc.input)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
		if val.(bool) != tc.expect {
			t.Fatalf("%s: expected %v, got %v", tc.name, tc.expect, val)
		}
	}

	if _, err := coerceBool("maybe"); err == nil {
		t.Fatalf("expected error for invalid boolean string")
	}
}

type customStringer struct{}

func (customStringer) String() string { return "stringer" }

func TestCoerceString(t *testing.T) {
	val, err := coerceString(customStringer{})
	if err != nil {
		t.Fatalf("coerceString stringer error: %v", err)
	}
	if val.(string) != "stringer" {
		t.Fatalf("expected stringer output, got %v", val)
	}

	if val, err := coerceString(" direct "); err != nil || val.(string) != " direct " {
		t.Fatalf("coerceString literal failed: %v %v", val, err)
	}
}
