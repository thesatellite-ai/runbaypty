package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestSkills_ListAndGet drives the command through the testable run() entry:
// list must name every skill, `get <name>` must print each guide, and an
// unknown name must exit non-zero. No daemon is involved (skills are local).
func TestSkills_ListAndGet(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"skills"}, &out, &errb); code != 0 {
		t.Fatalf("skills list exit %d: %s", code, errb.String())
	}
	for _, s := range skillRegistry {
		if !strings.Contains(out.String(), s.Name) {
			t.Errorf("skills list is missing %q", s.Name)
		}
	}

	for _, s := range skillRegistry {
		var o, e bytes.Buffer
		if code := run([]string{"skills", "get", s.Name}, &o, &e); code != 0 {
			t.Errorf("skills get %s exit %d: %s", s.Name, code, e.String())
		}
		// The guide must lead with its own name so list and body cannot drift.
		if !strings.HasPrefix(strings.TrimSpace(o.String()), "# "+s.Name) {
			t.Errorf("skills get %s: body should start with %q", s.Name, "# "+s.Name)
		}
	}

	var o, e bytes.Buffer
	if code := run([]string{"skills", "get", "no-such-skill"}, &o, &e); code == 0 {
		t.Error("skills get on an unknown name should exit non-zero")
	}
}

// TestSkills_RegistryConsistency guards the invariants the embedded guides must
// hold: every field populated, and no em-dashes (a house style rule).
func TestSkills_RegistryConsistency(t *testing.T) {
	for _, s := range skillRegistry {
		if s.Name == "" || s.Desc == "" || s.Body == "" {
			t.Errorf("skill %q has an empty Name/Desc/Body", s.Name)
		}
		if strings.ContainsRune(s.Desc, '—') || strings.ContainsRune(s.Body, '—') {
			t.Errorf("skill %q contains an em-dash", s.Name)
		}
	}
}
