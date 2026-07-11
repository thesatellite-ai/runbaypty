package proto

// protodoc_test.go — the doc-drift gate (task-m7-protodoc): PROTOCOL.md
// must name every registered frame type (with its wire value) and every
// error code. Add a type or code without documenting it → CI fails.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

func readProtocolDoc(t *testing.T) string {
	t.Helper()
	// Test cwd is the package dir. The doc lives in docsi/ — a repolink
	// symlink to the private-repo, gitignored, so it EXISTS on dev machines
	// but not in CI or fresh clones. Skip loudly when absent: the drift
	// gate runs wherever the docs are checked out (which is where protocol
	// edits happen), and never silently passes.
	data, err := os.ReadFile("../../docsi/PROTOCOL.md")
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("docsi/PROTOCOL.md not present (private docs not linked on this machine) — drift gate runs on dev machines only")
		}
		t.Fatalf("PROTOCOL.md: %v", err)
	}
	return string(data)
}

func TestProtocolDoc_EveryFrameTypeDocumented(t *testing.T) {
	t.Parallel()
	doc := readProtocolDoc(t)
	for typ, name := range knownTypes {
		row := fmt.Sprintf("| %d | `%s` |", typ, name)
		if !strings.Contains(doc, row) {
			t.Errorf("PROTOCOL.md missing frame-type row %q — document the type or fix the value", row)
		}
	}
}

func TestProtocolDoc_EveryErrorCodeDocumented(t *testing.T) {
	t.Parallel()
	doc := readProtocolDoc(t)
	for _, code := range errcodes.All() {
		if !strings.Contains(doc, "`"+string(code)+"`") {
			t.Errorf("PROTOCOL.md missing error code `%s`", code)
		}
	}
}

func TestProtocolDoc_EveryEventTypeDocumented(t *testing.T) {
	t.Parallel()
	doc := readProtocolDoc(t)
	for _, ev := range EventTypeValues {
		if !strings.Contains(doc, "`"+string(ev)+"`") {
			t.Errorf("PROTOCOL.md missing event type `%s`", ev)
		}
	}
}

func TestProtocolDoc_EveryDataKeyDocumented(t *testing.T) {
	t.Parallel()
	doc := readProtocolDoc(t)
	// Event data keys are a closed, additive-only contract just like frame
	// types and error codes — so PROTOCOL.md must name each one. Match the
	// `data.<key>` prefix (not a closing backtick) so optional keys documented
	// as `data.signal?` / `data.exit_code?` still count.
	for _, key := range DataKeyValues {
		if !strings.Contains(doc, "`data."+key) {
			t.Errorf("PROTOCOL.md missing event data key `data.%s` — document which event carries it", key)
		}
	}
}

func TestProtocolDoc_StatesTheProtocolVersion(t *testing.T) {
	t.Parallel()
	doc := readProtocolDoc(t)
	if !strings.Contains(doc, fmt.Sprintf("— v%d", ProtocolVersion)) {
		t.Errorf("PROTOCOL.md title does not state protocol v%d", ProtocolVersion)
	}
}
