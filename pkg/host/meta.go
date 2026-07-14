// meta.go — the session's JSON meta document: merge/replace writes, the size
// cap, the reserved rpty.* namespace, and the version counter.
//
// Storage model: meta is one canonical JSON object held as metaDoc []byte,
// never mutated in place (every write assigns a fresh slice), so a reader
// holding the session lock sees a consistent document. Writes run under the
// session mutex, which serializes concurrent patches: two agents merging
// different fields both survive (see docsi/META_SPEC.md).
package host

import (
	"bytes"
	"sort"
	"strconv"
	"strings"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/metajson"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

const (
	// MaxMetaBytes caps the merged JSON meta document. Meta rides in frames
	// (MaxFrameLen 1 MiB) and INFO responses; a bound keeps per-session memory
	// and frame size predictable on a public daemon.
	MaxMetaBytes = 64 << 10 // 64 KiB

	// reservedMetaRoot / reservedMetaPrefix namespace daemon-owned meta keys.
	// Clients may not write a top-level key equal to "rpty" or beginning with
	// "rpty." — reserved so daemon-set annotations can never collide with an
	// agent's keys.
	reservedMetaRoot   = "rpty"
	reservedMetaPrefix = "rpty."
)

// isReservedMetaKey reports whether a top-level key is in the daemon-reserved
// namespace.
func isReservedMetaKey(k string) bool {
	return k == reservedMetaRoot || strings.HasPrefix(k, reservedMetaPrefix)
}

// reservedKeyError returns a typed E_RESERVED_META_KEY error if b writes any
// reserved top-level key, else nil. Shared by the spawn-time and write-time
// paths so the reservation rule has exactly one enforcement point.
func reservedKeyError(b []byte) error {
	for _, k := range metajson.TopLevelKeys(b) {
		if isReservedMetaKey(k) {
			return errcodes.Newf(errcodes.ReservedMetaKey,
				"meta key %q uses the reserved %q namespace", k, reservedMetaPrefix)
		}
	}
	return nil
}

// ValidateSpawnAnnotations checks a spawn-time JSON meta blob: it must be a
// JSON object, may not write the reserved rpty.* namespace, and must be within
// the size cap. Empty is valid (no annotations). The daemon calls this before
// creating the session so a bad blob fails the spawn loudly rather than being
// silently dropped at construction.
func ValidateSpawnAnnotations(b []byte) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if !metajson.IsObject(b) {
		return errcodes.Newf(errcodes.InvalidInput, "annotations must be a JSON object")
	}
	if err := reservedKeyError(b); err != nil {
		return err
	}
	if len(b) > MaxMetaBytes {
		return errcodes.Newf(errcodes.MetaTooLarge,
			"annotations %d bytes exceeds the %d-byte cap", len(b), MaxMetaBytes)
	}
	return nil
}

// initialMetaDoc builds a session's starting meta document from a spawn
// config: the legacy flat Meta map seeds top-level string fields, then the
// JSON Annotations blob merges on top. Returns nil (an empty document) when
// neither is set. Defensive on bad Annotations — the daemon validates before
// spawn, so a parse failure here falls back to the flat base rather than
// failing the spawn.
func initialMetaDoc(cfg SpawnConfig) []byte {
	base, err := metajson.FromFlat(cfg.Meta)
	if err != nil {
		base = nil
	}
	if len(bytes.TrimSpace(cfg.Annotations)) == 0 {
		if len(cfg.Meta) == 0 {
			return nil
		}
		return base
	}
	merged, err := metajson.MergePatch(base, cfg.Annotations)
	if err != nil || !metajson.IsObject(merged) {
		return base
	}
	return merged
}

// adoptMetaDoc reconstructs the meta document during a takeover. A new-enough
// old daemon sends MetaDoc directly; a pre-upgrade one sends only the flat
// Meta map, which we rebuild from. Keeps meta surviving a daemon upgrade.
func adoptMetaDoc(st HandoverState) []byte {
	if len(bytes.TrimSpace(st.MetaDoc)) > 0 {
		return st.MetaDoc
	}
	if len(st.Meta) == 0 {
		return nil
	}
	doc, err := metajson.FromFlat(st.Meta)
	if err != nil {
		return nil
	}
	return doc
}

// annotationsForWire returns the meta document for the INFO/LIST wire field,
// or nil so `omitempty` drops it when meta is empty.
func annotationsForWire(doc []byte) []byte {
	if len(bytes.TrimSpace(doc)) == 0 {
		return nil
	}
	return doc
}

// SetMetaJSON merges or replaces the session's JSON meta document and returns
// the new version. It runs entirely under the session lock, so concurrent
// writers are serialized: disjoint-field merges never clobber each other.
//
// Enforced, in order: the optional compare-and-swap (ifVersion), the reserved
// rpty.* namespace (client patches may not write it), that the result stays a
// JSON object, and the size cap. On any failure the document is unchanged and
// the current version is returned alongside a typed error.
func (s *Session) SetMetaJSON(mode proto.MetaMode, patch []byte, ifVersion *uint64) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ifVersion != nil && *ifVersion != s.metaVersion {
		return s.metaVersion, errcodes.Newf(errcodes.MetaConflict,
			"meta version is %d, not %d (another writer updated it first)", s.metaVersion, *ifVersion)
	}

	// Reserved-namespace guard: only the client's own patch keys are checked;
	// daemon-set rpty.* keys already in the document are preserved by merge.
	if err := reservedKeyError(patch); err != nil {
		return s.metaVersion, err
	}

	// Apply per mode. Replace merges into an empty doc (canonicalizes + rejects
	// a non-object below); incr adds numeric deltas; merge/"" is RFC 7386.
	var next []byte
	var err error
	switch mode {
	case proto.MetaModeReplace:
		next, err = metajson.MergePatch(nil, patch)
	case proto.MetaModeIncr:
		next, err = metajson.IncrPatch(s.metaDoc, patch)
	case "", proto.MetaModeMerge:
		next, err = metajson.MergePatch(s.metaDoc, patch)
	default:
		return s.metaVersion, errcodes.Newf(errcodes.InvalidInput, "unknown meta mode %q", mode)
	}
	if err != nil {
		// Chain the underlying metajson error (matches the spawn-handler
		// pattern) so errors.Is/As still reaches it past the CLI envelope.
		return s.metaVersion, errcodes.Newf(errcodes.InvalidInput,
			"meta %s: %v", modeName(mode), err).WithCause(err)
	}
	if !metajson.IsObject(next) {
		return s.metaVersion, errcodes.Newf(errcodes.InvalidInput,
			"meta %s result is not a JSON object", modeName(mode))
	}
	if len(next) > MaxMetaBytes {
		return s.metaVersion, errcodes.Newf(errcodes.MetaTooLarge,
			"meta document %d bytes exceeds the %d-byte cap", len(next), MaxMetaBytes)
	}

	s.metaDoc = next
	s.metaVersion++
	version := s.metaVersion
	// EmitSession is safe to call under the lock: the bus fans out on its own
	// goroutines and never calls back into the session. The data lets a
	// subscriber react without a follow-up INFO round-trip.
	s.bus.EmitSession(proto.EventMetaChanged, s.id, metaChangeData(version, metajson.TopLevelKeys(patch)))
	return version, nil
}

// metaChangeData builds the meta-changed event payload: the touched top-level
// keys (sorted, comma-joined) and the new version.
func metaChangeData(version uint64, keys []string) map[string]string {
	sort.Strings(keys)
	return map[string]string{
		proto.DataKeyMetaKeys:    strings.Join(keys, ","),
		proto.DataKeyMetaVersion: strconv.FormatUint(version, 10),
	}
}

// modeName renders a mode for error messages, defaulting empty to "merge".
func modeName(m proto.MetaMode) string {
	switch m {
	case proto.MetaModeReplace:
		return string(proto.MetaModeReplace)
	case proto.MetaModeIncr:
		return string(proto.MetaModeIncr)
	default:
		return string(proto.MetaModeMerge)
	}
}

// SetMeta is the legacy flat-KV entry point (SET_META frame, --meta k=v). It
// wholesale-replaces the meta document with the map's string fields. Trusted
// path: no reserved-namespace or size enforcement, matching the pre-upgrade
// behavior. Prefer SetMetaJSON for anything structured.
func (s *Session) SetMeta(meta map[string]string) {
	doc, err := metajson.FromFlat(meta)
	if err != nil {
		return
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	s.mu.Lock()
	s.metaDoc = doc
	s.metaVersion++
	version := s.metaVersion
	s.mu.Unlock()
	s.bus.EmitSession(proto.EventMetaChanged, s.id, metaChangeData(version, keys))
}
