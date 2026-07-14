package metajson

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// IncrPatch atomically adds the numeric leaves of patch to the matching fields
// of target and returns the resulting canonical JSON. It is the operation
// merge cannot express: "add 200 to tokens" rather than "set tokens".
//
// Rules: every leaf in the patch object must be a number (the delta); nested
// objects recurse, creating intermediate objects as needed; a missing target
// field counts as 0. Adding to an existing non-number field is an error (you
// cannot increment a string). Integer deltas on integer fields stay integers;
// any float operand promotes the result to a float.
func IncrPatch(target, patch []byte) ([]byte, error) {
	patchVal, err := decode(patch)
	if err != nil {
		return nil, fmt.Errorf("incr: invalid JSON patch: %w", err)
	}
	patchObj, ok := patchVal.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("incr: patch must be a JSON object")
	}
	targetObj := map[string]any{}
	if len(target) > 0 {
		tv, err := decode(target)
		if err != nil {
			return nil, fmt.Errorf("incr: invalid JSON target: %w", err)
		}
		if obj, ok := tv.(map[string]any); ok {
			targetObj = obj
		}
	}
	if err := incrInto(targetObj, patchObj); err != nil {
		return nil, err
	}
	return encode(targetObj)
}

// incrInto applies the deltas in patch to target in place.
func incrInto(target, patch map[string]any) error {
	for k, v := range patch {
		switch pv := v.(type) {
		case json.Number:
			cur := json.Number("0")
			if existing, ok := target[k]; ok {
				n, isNum := existing.(json.Number)
				if !isNum {
					return fmt.Errorf("incr: field %q is not a number", k)
				}
				cur = n
			}
			sum, err := addNumbers(cur, pv)
			if err != nil {
				return fmt.Errorf("incr: field %q: %w", k, err)
			}
			target[k] = sum
		case map[string]any:
			child, ok := target[k].(map[string]any)
			if !ok {
				if _, present := target[k]; present {
					return fmt.Errorf("incr: field %q is not an object", k)
				}
				child = map[string]any{}
				target[k] = child
			}
			if err := incrInto(child, pv); err != nil {
				return err
			}
		default:
			return fmt.Errorf("incr: value for %q must be a number", k)
		}
	}
	return nil
}

// addNumbers sums two JSON numbers, keeping integer arithmetic when both are
// integers and falling back to float otherwise. The result is a json.Number so
// it re-encodes without reformatting.
func addNumbers(a, b json.Number) (json.Number, error) {
	ai, aErr := a.Int64()
	bi, bErr := b.Int64()
	if aErr == nil && bErr == nil {
		return json.Number(strconv.FormatInt(ai+bi, 10)), nil
	}
	af, err := a.Float64()
	if err != nil {
		return "", fmt.Errorf("not a number: %q", a)
	}
	bf, err := b.Float64()
	if err != nil {
		return "", fmt.Errorf("not a number: %q", b)
	}
	return json.Number(strconv.FormatFloat(af+bf, 'g', -1, 64)), nil
}
