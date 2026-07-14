package metajson

import (
	"encoding/json"
	"reflect"
	"testing"
)

// eqJSON compares two JSON byte slices by decoded value, so key order and
// whitespace never make an equal document look unequal.
func eqJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("merge result mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestMergePatch_RFC7386Table(t *testing.T) {
	t.Parallel()
	// Cases straight from RFC 7386 Appendix A, plus the meta-specific ones.
	cases := []struct {
		name          string
		target, patch string
		want          string
	}{
		{"add field", `{"a":"b"}`, `{"c":"d"}`, `{"a":"b","c":"d"}`},
		{"replace field", `{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{"delete via null", `{"a":"b","c":"d"}`, `{"a":null}`, `{"c":"d"}`},
		{"delete absent is noop", `{"a":"b"}`, `{"x":null}`, `{"a":"b"}`},
		{"recursive object merge", `{"a":{"b":"c","d":"e"}}`, `{"a":{"b":"x"}}`, `{"a":{"b":"x","d":"e"}}`},
		{"array replaces, not merges", `{"a":["1","2"]}`, `{"a":["3"]}`, `{"a":["3"]}`},
		{"scalar over object", `{"a":{"b":"c"}}`, `{"a":1}`, `{"a":1}`},
		{"object over scalar", `{"a":"b"}`, `{"a":{"c":"d"}}`, `{"a":{"c":"d"}}`},
		{"empty target gets object", ``, `{"a":"b"}`, `{"a":"b"}`},
		{"nested delete", `{"a":{"b":"c","d":"e"}}`, `{"a":{"d":null}}`, `{"a":{"b":"c"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := MergePatch([]byte(c.target), []byte(c.patch))
			if err != nil {
				t.Fatalf("MergePatch: %v", err)
			}
			eqJSON(t, got, c.want)
		})
	}
}

func TestMergePatch_IntegerPreserved(t *testing.T) {
	t.Parallel()
	// Regression: json.Number keeps 900 as 900, never 9e2, and large ids exact.
	got, err := MergePatch([]byte(`{"budget":900}`), []byte(`{"tokens":123456789012345}`))
	if err != nil {
		t.Fatalf("MergePatch: %v", err)
	}
	eqJSON(t, got, `{"budget":900,"tokens":123456789012345}`)
	if string(got) == "" || containsAny(string(got), "e+", "E+") {
		t.Fatalf("number reformatted to scientific notation: %s", got)
	}
}

func TestMergePatch_InvalidJSON(t *testing.T) {
	t.Parallel()
	if _, err := MergePatch([]byte(`{}`), []byte(`{bad`)); err == nil {
		t.Fatal("want error on invalid patch, got nil")
	}
	if _, err := MergePatch([]byte(`{oops`), []byte(`{}`)); err == nil {
		t.Fatal("want error on invalid target, got nil")
	}
	if _, err := MergePatch([]byte(`{}`), []byte(`{}{}`)); err == nil {
		t.Fatal("want error on trailing data, got nil")
	}
}

func TestIsObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		doc  string
		want bool
	}{
		{``, true}, // empty meta is {}
		{`{}`, true},
		{`{"a":1}`, true},
		{`[]`, false},
		{`5`, false},
		{`"s"`, false},
		{`{bad`, false},
	}
	for _, c := range cases {
		if got := IsObject([]byte(c.doc)); got != c.want {
			t.Errorf("IsObject(%q) = %v, want %v", c.doc, got, c.want)
		}
	}
}

func TestProject_TopLevelStringsOnly(t *testing.T) {
	t.Parallel()
	// Only top-level string fields project into the legacy flat map.
	got := Project([]byte(`{"agent":"a1","step":3,"tags":["ci"],"task":{"id":5}}`))
	want := map[string]string{"agent": "a1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Project = %v, want %v", got, want)
	}
	if Project([]byte(``)) != nil {
		t.Fatal("Project(empty) should be nil")
	}
	if Project([]byte(`{"step":3}`)) != nil {
		t.Fatal("Project with no string fields should be nil")
	}
}

func TestFromFlat_RoundTrips(t *testing.T) {
	t.Parallel()
	doc, err := FromFlat(map[string]string{"agent": "a1", "task": "deploy"})
	if err != nil {
		t.Fatalf("FromFlat: %v", err)
	}
	eqJSON(t, doc, `{"agent":"a1","task":"deploy"}`)
	back := Project(doc)
	if !reflect.DeepEqual(back, map[string]string{"agent": "a1", "task": "deploy"}) {
		t.Fatalf("FromFlat->Project mismatch: %v", back)
	}
	empty, err := FromFlat(nil)
	if err != nil {
		t.Fatalf("FromFlat(nil): %v", err)
	}
	eqJSON(t, empty, `{}`)
}

func TestTopLevelKeys(t *testing.T) {
	t.Parallel()
	keys := TopLevelKeys([]byte(`{"a":1,"rpty.pid":2}`))
	if len(keys) != 2 {
		t.Fatalf("want 2 keys, got %v", keys)
	}
	if TopLevelKeys([]byte(`[]`)) != nil {
		t.Fatal("non-object should yield nil keys")
	}
	if TopLevelKeys([]byte(`{bad`)) != nil {
		t.Fatal("invalid JSON should yield nil keys")
	}
}

func TestProject_InvalidJSONIsNil(t *testing.T) {
	t.Parallel()
	if Project([]byte(`{bad`)) != nil {
		t.Fatal("invalid JSON should project to nil")
	}
	if Project([]byte(`[1,2]`)) != nil {
		t.Fatal("non-object should project to nil")
	}
}

// containsAny reports whether s contains any of subs.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
