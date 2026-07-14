// metaargs.go — the CLI grammar for building a JSON meta patch from the
// command line, without shell quoting hell.
//
// Grammar (borrowed from httpie; the operator decides the type, never a guess):
//
//	key=value    → value is ALWAYS a string      (name=deploy → "deploy")
//	key:=value   → value is parsed as JSON        (count:=5 → 5, tags:=["a"] → ["a"])
//	a.b.c=value  → dotted keys nest               ({"a":{"b":{"c":"value"}}})
//	foo\.bar=v   → escaped dot is literal         ({"foo.bar":"v"})
//
// A --json source ("-" = stdin, "@file" = file, otherwise inline JSON) supplies
// a base document; k=v / k:=v pairs then merge on top.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

// buildMetaPatch assembles a JSON object from an optional --json source and a
// list of key/value pairs. Later pairs override earlier ones and the --json
// base. Returns canonical JSON bytes ready to send as a merge/replace patch.
func buildMetaPatch(jsonSrc string, pairs []string, stdin io.Reader) (json.RawMessage, error) {
	root := map[string]any{}
	if jsonSrc != "" {
		raw, err := readJSONSource(jsonSrc, stdin)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &root); err != nil {
			return nil, errcodes.Newf(errcodes.InvalidInput, "--json must be a JSON object: %v", err)
		}
	}
	for _, p := range pairs {
		key, val, err := parseMetaPair(p)
		if err != nil {
			return nil, err
		}
		setPath(root, key, val)
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, errcodes.Newf(errcodes.InvalidInput, "encode meta: %v", err)
	}
	return out, nil
}

// buildUnsetPatch makes a merge patch that deletes each key path (RFC 7386
// null-deletes), e.g. `meta unset s task.id` → {"task":{"id":null}}.
func buildUnsetPatch(keys []string) (json.RawMessage, error) {
	root := map[string]any{}
	for _, k := range keys {
		setPath(root, k, nil)
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, errcodes.Newf(errcodes.InvalidInput, "encode meta: %v", err)
	}
	return out, nil
}

// buildIncrPatch makes a patch for `meta incr`: each `key=delta` becomes a
// numeric leaf at the dotted key path. The delta must be a number; a non-number
// is a loud error (incr only adds to numeric fields).
func buildIncrPatch(pairs []string) (json.RawMessage, error) {
	root := map[string]any{}
	for _, p := range pairs {
		eq := strings.IndexByte(p, '=')
		if eq < 1 {
			return nil, errcodes.Newf(errcodes.InvalidInput, "incr %q: want key=delta (a number)", p)
		}
		dec := json.NewDecoder(strings.NewReader(p[eq+1:]))
		dec.UseNumber()
		var num json.Number
		if err := dec.Decode(&num); err != nil {
			return nil, errcodes.Newf(errcodes.InvalidInput, "incr %q: delta must be a number", p)
		}
		setPath(root, p[:eq], num)
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, errcodes.Newf(errcodes.InvalidInput, "encode incr: %v", err)
	}
	return out, nil
}

// parseMetaPair splits one `key=value` (string) or `key:=value` (JSON) token.
// The operator is the FIRST unescaped `=`; a `:` immediately before it makes
// it the `:=` JSON operator.
func parseMetaPair(p string) (key string, val any, err error) {
	eq := strings.IndexByte(p, '=')
	if eq < 0 {
		return "", nil, errcodes.Newf(errcodes.InvalidInput,
			"meta %q: want key=value (string) or key:=value (JSON)", p)
	}
	if eq > 0 && p[eq-1] == ':' { // key:=value → JSON value
		key = p[:eq-1]
		if key == "" {
			return "", nil, errcodes.Newf(errcodes.InvalidInput, "meta %q: empty key", p)
		}
		dec := json.NewDecoder(strings.NewReader(p[eq+1:]))
		dec.UseNumber() // keep integers exact, never scientific notation
		if err := dec.Decode(&val); err != nil {
			return "", nil, errcodes.Newf(errcodes.InvalidInput,
				"meta %q: value after := is not valid JSON (%v); use = for a string", p, err)
		}
		return key, val, nil
	}
	key = p[:eq] // key=value → string value
	if key == "" {
		return "", nil, errcodes.Newf(errcodes.InvalidInput, "meta %q: empty key", p)
	}
	return key, p[eq+1:], nil
}

// setPath assigns val at a dotted key path, creating intermediate objects and
// overwriting any non-object encountered along the way. A nil val encodes as
// JSON null (delete under merge semantics).
func setPath(root map[string]any, key string, val any) {
	parts := splitEscapedDots(key)
	m := root
	for _, p := range parts[:len(parts)-1] {
		child, ok := m[p].(map[string]any)
		if !ok {
			child = map[string]any{}
			m[p] = child
		}
		m = child
	}
	m[parts[len(parts)-1]] = val
}

// splitEscapedDots splits a key on unescaped '.'; a backslash escapes the dot
// so a literal-dotted key (foo\.bar) stays one segment.
func splitEscapedDots(key string) []string {
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(key); i++ {
		switch {
		case key[i] == '\\' && i+1 < len(key) && key[i+1] == '.':
			cur.WriteByte('.')
			i++
		case key[i] == '.':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(key[i])
		}
	}
	parts = append(parts, cur.String())
	return parts
}

// metaMatches reports whether a session's JSON meta document satisfies every
// key=value filter (AND). Filtering is client-side on purpose: the daemon
// stays policy-free and never indexes or queries meta. Keys are dotted paths;
// the value at that path is compared as a scalar string (JSON string → its
// text, number → its digits, bool → true/false). A missing path never matches.
func metaMatches(doc []byte, filters []string) (bool, error) {
	if len(filters) == 0 {
		return true, nil
	}
	var root any
	if len(bytes.TrimSpace(doc)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(doc))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			root = nil
		}
	}
	for _, f := range filters {
		eq := strings.IndexByte(f, '=')
		if eq < 0 {
			return false, errcodes.Newf(errcodes.InvalidInput, "filter %q: want key=value", f)
		}
		got, ok := lookupPath(root, splitEscapedDots(f[:eq]))
		if !ok || scalarString(got) != f[eq+1:] {
			return false, nil
		}
	}
	return true, nil
}

// lookupPath walks a decoded JSON tree by dotted-key segments, returning the
// value and whether the full path resolved.
func lookupPath(root any, parts []string) (any, bool) {
	cur := root
	for _, p := range parts {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// scalarString renders a JSON scalar for filter comparison. Objects and arrays
// have no scalar form and return "" (which never equals a real filter value).
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// readJSONSource resolves the --json value: "-" reads stdin, "@path" reads a
// file, anything else is treated as an inline JSON literal.
func readJSONSource(src string, stdin io.Reader) ([]byte, error) {
	switch {
	case src == "-":
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, errcodes.Newf(errcodes.InvalidInput, "read --json from stdin: %v", err)
		}
		return b, nil
	case strings.HasPrefix(src, "@"):
		b, err := os.ReadFile(src[1:])
		if err != nil {
			return nil, errcodes.Newf(errcodes.InvalidInput, "read --json file: %v", err)
		}
		return b, nil
	default:
		return []byte(src), nil
	}
}
