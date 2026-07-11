package style

import (
	"os"
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := map[string]Mode{
		"":        ModeAuto,
		"auto":    ModeAuto,
		"always":  ModeAlways,
		"yes":     ModeAlways,
		"on":      ModeAlways,
		"true":    ModeAlways,
		"1":       ModeAlways,
		"never":   ModeNever,
		"no":      ModeNever,
		"off":     ModeNever,
		"false":   ModeNever,
		"0":       ModeNever,
		"garbage": ModeAuto,
	}
	for in, want := range cases {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMode_String(t *testing.T) {
	if ModeAuto.String() != "auto" {
		t.Errorf("auto: %q", ModeAuto.String())
	}
	if ModeAlways.String() != "always" {
		t.Errorf("always: %q", ModeAlways.String())
	}
	if ModeNever.String() != "never" {
		t.Errorf("never: %q", ModeNever.String())
	}
}

func TestInit_AlwaysMode(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")
	Init(ModeAlways)
	if !Enabled() {
		t.Error("expected enabled with ModeAlways")
	}
	if !strings.Contains(Success("ok"), "\x1b[") {
		t.Error("expected ANSI codes when always")
	}
}

func TestInit_NeverMode(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")
	Init(ModeNever)
	if Enabled() {
		t.Error("expected disabled with ModeNever")
	}
	if got := Success("ok"); strings.Contains(got, "\x1b[") {
		t.Errorf("expected no ANSI when never, got %q", got)
	}
	if got := Success("ok"); got != "ok" {
		t.Errorf("expected raw passthrough when never, got %q", got)
	}
}

func TestInit_NoColorEnv(t *testing.T) {
	// Restore later
	t.Setenv("NO_COLOR", "1")
	t.Setenv("FORCE_COLOR", "")
	Init(ModeAuto) // auto + NO_COLOR -> never
	if Enabled() {
		t.Error("expected NO_COLOR to disable")
	}
}

func TestInit_ForceColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "1")
	Init(ModeAuto) // auto + FORCE_COLOR -> always
	if !Enabled() {
		t.Error("expected FORCE_COLOR to enable")
	}
}

func TestInit_FlagOverridesEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("FORCE_COLOR", "1")
	Init(ModeAlways) // explicit flag wins over env
	if !Enabled() {
		t.Error("expected explicit ModeAlways to win over NO_COLOR")
	}

	Init(ModeNever)
	if Enabled() {
		t.Error("expected explicit ModeNever to win over FORCE_COLOR")
	}
}

func TestSemanticTokens_AllAcceptStrings(t *testing.T) {
	Init(ModeAlways)
	in := "hello"
	cases := map[string]func(string) string{
		"Success": Success,
		"Error":   Error,
		"Warn":    Warn,
		"Info":    Info,
		"Hint":    Hint,
		"Muted":   Muted,
		"Code":    Code,
		"Bold":    Bold,
	}
	for name, fn := range cases {
		out := fn(in)
		if !strings.Contains(out, in) {
			t.Errorf("%s: lost input string", name)
		}
		if !strings.Contains(out, "\x1b[") {
			t.Errorf("%s: missing ANSI codes when enabled", name)
		}
		if !strings.Contains(out, "\x1b[0m") {
			t.Errorf("%s: missing reset code", name)
		}
	}
}

func TestScopeBadge_Format(t *testing.T) {
	Init(ModeNever) // strip ANSI for assertion
	if got := ScopeBadge("repo:web1"); got != "[repo:web1]" {
		t.Errorf("ScopeBadge format: %q", got)
	}
	if got := ScopeBadge("master"); got != "[master]" {
		t.Errorf("ScopeBadge format: %q", got)
	}
}

// ── coverage gap closers ────────────────────────────────────────────

func TestCurrentMode_ReflectsInit(t *testing.T) {
	for _, mode := range []Mode{ModeAlways, ModeNever, ModeAuto} {
		Init(mode)
		if got := CurrentMode(); got != mode {
			t.Errorf("CurrentMode() = %v after Init(%v)", got, mode)
		}
	}
}

func TestIsTerminal_NonTTYFile(t *testing.T) {
	// A regular file is never a terminal — the branch Init() relies on for
	// the auto mode (piped output must not emit ANSI).
	f, err := os.CreateTemp(t.TempDir(), "notty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("isTerminal(regular file) = true")
	}
}

func TestParseMode_AllSynonyms(t *testing.T) {
	for _, s := range modeAlwaysSynonyms {
		if got := ParseMode(s); got != ModeAlways {
			t.Errorf("ParseMode(%q) = %v, want ModeAlways", s, got)
		}
	}
	for _, s := range modeNeverSynonyms {
		if got := ParseMode(s); got != ModeNever {
			t.Errorf("ParseMode(%q) = %v, want ModeNever", s, got)
		}
	}
	for _, s := range []string{"", "auto", "maybe", "ALWAYS"} {
		if got := ParseMode(s); got != ModeAuto {
			t.Errorf("ParseMode(%q) = %v, want ModeAuto (unknown → auto)", s, got)
		}
	}
	// Names round-trip through String().
	if ModeAlways.String() != ModeNameAlways || ModeNever.String() != ModeNameNever || ModeAuto.String() != ModeNameAuto {
		t.Error("Mode.String() diverged from the ModeName* constants")
	}
}
