// Package ids generates and validates runbaypty opaque IDs.
//
// Format: <3-char-prefix>_<32-char-lowercase-hex-UUIDv7>
//
// The prefix tags the entity kind for human readability in logs and CLI output.
// The UUIDv7 portion provides:
//   - Time-sortable ordering (high bits = ms-since-epoch) — sorting sessions by ID is chronological
//   - Collision resistance (74 random bits per ID)
//   - No coordination needed across daemon restarts
package ids

// Prefix constants for every ID-bearing entity in runbaypty. Add new prefixes
// here when introducing a new kind; the coverage test enforces uniqueness and
// registration.
const (
	// PrefixSession — a PTY session owned by the daemon (ses_…).
	PrefixSession = "ses"
	// PrefixClient — one connected client socket; used in logs and INFO output
	// to attribute subscriptions and write locks (cli_…).
	PrefixClient = "cli"
	// PrefixToken — an auth token identity for the WS listener (tok_…). This is
	// the token's NAME for logs/discovery, never the secret value itself.
	PrefixToken = "tok"
	// PrefixWatch — a server-side WATCH registration (v2, reserved) (wch_…).
	PrefixWatch = "wch"
)

// AllPrefixes lists every registered prefix. Used by ValidateAny and tests.
var AllPrefixes = []string{
	PrefixSession,
	PrefixClient,
	PrefixToken,
	PrefixWatch,
}
