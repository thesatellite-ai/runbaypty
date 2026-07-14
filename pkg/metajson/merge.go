// Package metajson implements the JSON document operations behind a session's
// `meta` field: RFC 7386 JSON Merge Patch, and the legacy flat-string
// projection used for wire back-compat.
//
// Why it exists: session meta upgraded from a flat map[string]string to an
// arbitrary JSON document (see docsi/META_SPEC.md). Merge-by-default lets two
// writers update disjoint fields without clobbering each other. The merge is
// the one place the daemon looks inside client data, so it is isolated here,
// pure and dependency-free, and exhaustively tested.
//
// Boundary note: JSON of unknown shape is decoded into `any` (the documented
// system-boundary escape). Numbers are decoded as json.Number so a round-trip
// never reformats an integer (900 stays 900, never 9e2) or loses precision.
package metajson

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MergePatch applies an RFC 7386 JSON Merge Patch to target and returns the
// resulting document as canonical JSON (object keys sorted, numbers verbatim).
//
// Semantics (RFC 7386): objects merge recursively; a null value deletes its
// key; any non-object patch (scalar or array) replaces the target wholesale.
// An empty target is treated as JSON null, so merging an object into "" yields
// that object.
//
// Callers that require the result to stay an object (session meta does) must
// check with IsObject after merging — this function follows the RFC and does
// not impose that constraint itself.
func MergePatch(target, patch []byte) ([]byte, error) {
	patchVal, err := decode(patch)
	if err != nil {
		return nil, fmt.Errorf("merge patch: invalid JSON patch: %w", err)
	}
	// An absent/empty target is JSON null: MergePatch(null, obj) => obj.
	var targetVal any
	if len(bytes.TrimSpace(target)) > 0 {
		targetVal, err = decode(target)
		if err != nil {
			return nil, fmt.Errorf("merge patch: invalid JSON target: %w", err)
		}
	}
	merged := mergeValue(targetVal, patchVal)
	return encode(merged)
}

// mergeValue is the recursive core of RFC 7386. It returns the new value for
// the position: when patch is an object it merges into target (coercing a
// non-object target to an empty object first, per the RFC); otherwise patch
// replaces target outright.
func mergeValue(target, patch any) any {
	patchObj, patchIsObj := patch.(map[string]any)
	if !patchIsObj {
		// Scalar, array, string, number, bool, or null-at-top: replace.
		return patch
	}
	targetObj, targetIsObj := target.(map[string]any)
	if !targetIsObj {
		targetObj = map[string]any{}
	}
	for name, val := range patchObj {
		if val == nil {
			// null means delete the key (RFC 7386). A no-op if absent.
			delete(targetObj, name)
			continue
		}
		targetObj[name] = mergeValue(targetObj[name], val)
	}
	return targetObj
}

// IsObject reports whether doc is a JSON object (the shape session meta must
// keep). Empty input counts as an object: an empty meta document is {}.
func IsObject(doc []byte) bool {
	trimmed := bytes.TrimSpace(doc)
	if len(trimmed) == 0 {
		return true
	}
	val, err := decode(trimmed)
	if err != nil {
		return false
	}
	_, ok := val.(map[string]any)
	return ok
}

// TopLevelKeys returns the object's top-level key names, or nil for a
// non-object / empty document. Used to enforce the reserved-namespace rule
// without the caller re-parsing.
func TopLevelKeys(doc []byte) []string {
	val, err := decode(doc)
	if err != nil {
		return nil
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	return keys
}

// Project returns the top-level string-valued fields of doc as a flat map.
// This is the back-compat projection filling the legacy wire `meta`
// (map[string]string) field so pre-upgrade clients still see their tags.
// Non-string top-level values (objects, arrays, numbers, bools) are omitted:
// they have no faithful flat representation. Returns nil for an empty or
// non-object document so `omitempty` keeps it off the wire.
func Project(doc []byte) map[string]string {
	val, err := decode(doc)
	if err != nil {
		return nil
	}
	obj, ok := val.(map[string]any)
	if !ok || len(obj) == 0 {
		return nil
	}
	out := make(map[string]string, len(obj))
	for k, v := range obj {
		if s, isStr := v.(string); isStr {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FromFlat encodes a flat string map as a canonical JSON object. It bridges
// the legacy SetMeta{meta: map[string]string} frame and the flat --meta k=v
// spawn shorthand into the JSON document store. A nil/empty map yields "{}".
func FromFlat(m map[string]string) ([]byte, error) {
	obj := make(map[string]any, len(m))
	for k, v := range m {
		obj[k] = v
	}
	return encode(obj)
}

// decode parses JSON into an `any` tree using json.Number for numerics, so
// re-encoding never rewrites an integer or drops precision.
func decode(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// Reject trailing garbage after the first JSON value (e.g. "{}{}").
	if dec.More() {
		return nil, fmt.Errorf("unexpected trailing data after JSON value")
	}
	return v, nil
}

// encode marshals a decoded tree back to canonical JSON. Go sorts object keys
// on marshal, giving deterministic, diff-stable, test-friendly output.
func encode(v any) ([]byte, error) {
	return json.Marshal(v)
}
