package metajson

import (
	"encoding/json"
	"testing"
)

func TestIncrPatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		target, patch string
		want          string
	}{
		{"add to existing int", `{"tokens":900}`, `{"tokens":200}`, `{"tokens":1100}`},
		{"missing field is zero", `{}`, `{"tokens":5}`, `{"tokens":5}`},
		{"other fields untouched", `{"a":"x","n":1}`, `{"n":2}`, `{"a":"x","n":3}`},
		{"nested increment", `{"task":{"retries":1}}`, `{"task":{"retries":1}}`, `{"task":{"retries":2}}`},
		{"nested created", `{}`, `{"task":{"retries":1}}`, `{"task":{"retries":1}}`},
		{"negative delta", `{"n":5}`, `{"n":-2}`, `{"n":3}`},
		{"float promotes", `{"n":1}`, `{"n":0.5}`, `{"n":1.5}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := IncrPatch([]byte(c.target), []byte(c.patch))
			if err != nil {
				t.Fatalf("IncrPatch: %v", err)
			}
			eqJSON(t, got, c.want)
		})
	}
}

func TestIncrPatch_Errors(t *testing.T) {
	t.Parallel()
	// Incrementing a non-number field.
	if _, err := IncrPatch([]byte(`{"a":"str"}`), []byte(`{"a":1}`)); err == nil {
		t.Error("incrementing a string field should error")
	}
	// Non-number delta.
	if _, err := IncrPatch([]byte(`{}`), []byte(`{"a":"x"}`)); err == nil {
		t.Error("non-number delta should error")
	}
	// Non-object patch.
	if _, err := IncrPatch([]byte(`{}`), []byte(`5`)); err == nil {
		t.Error("non-object patch should error")
	}
	// Type clash: target field is an object, patch treats it as a number.
	if _, err := IncrPatch([]byte(`{"a":{"b":1}}`), []byte(`{"a":1}`)); err == nil {
		t.Error("number delta on object field should error")
	}
	// Type clash the other way: target field is a number, patch is an object.
	if _, err := IncrPatch([]byte(`{"a":5}`), []byte(`{"a":{"b":1}}`)); err == nil {
		t.Error("object patch on number field should error")
	}
	// Invalid target JSON.
	if _, err := IncrPatch([]byte(`{bad`), []byte(`{"a":1}`)); err == nil {
		t.Error("invalid target should error")
	}
	// Invalid patch JSON.
	if _, err := IncrPatch([]byte(`{}`), []byte(`{bad`)); err == nil {
		t.Error("invalid patch should error")
	}
	// Error inside a nested object propagates up.
	if _, err := IncrPatch([]byte(`{"a":{"b":"str"}}`), []byte(`{"a":{"b":1}}`)); err == nil {
		t.Error("nested type clash should propagate")
	}
}

func TestIncrPatch_NonObjectTargetTreatedAsEmpty(t *testing.T) {
	t.Parallel()
	// A non-object target (scalar/array) is treated as an empty object, so the
	// increment simply seeds the fields.
	got, err := IncrPatch([]byte(`5`), []byte(`{"n":3}`))
	if err != nil {
		t.Fatalf("IncrPatch: %v", err)
	}
	eqJSON(t, got, `{"n":3}`)
}

func TestAddNumbers_ErrorBranches(t *testing.T) {
	t.Parallel()
	// A malformed json.Number that is neither int nor float exercises the
	// defensive error paths (unreachable via normal decoding, guarded anyway).
	if _, err := addNumbers(json.Number("notanum"), json.Number("1")); err == nil {
		t.Error("bad left operand should error")
	}
	if _, err := addNumbers(json.Number("1.5"), json.Number("notanum")); err == nil {
		t.Error("bad right operand should error")
	}
}
