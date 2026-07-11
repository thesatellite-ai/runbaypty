// registry.go — the in-memory session registry: id minting, unique-name
// index, lookup by id or name, caps. This IS the daemon's state; there is
// deliberately no database (MISSION: policy-free, dumb durable owner).
package host

import (
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
	"github.com/thesatellite-ai/runbaypty/pkg/ids"
	"github.com/thesatellite-ai/runbaypty/pkg/proto"
)

// nameRe bounds session names: tmux-ish, safe in CLIs and URLs. Names never
// collide with ids because ids always match ids.PrefixSession + "_" + 32 hex
// and names are capped at 64 chars with no way to be exactly that shape…
// except by writing one deliberately — so Lookup checks ids FIRST.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Registry owns every live and retained session, plus the event bus they
// emit on.
type Registry struct {
	mu sync.Mutex
	// byID holds every session, live or exited-retained.
	byID map[string]*Session
	// byName indexes the unique human names ("" never indexed).
	byName map[string]*Session
	// maxSessions caps concurrent sessions (guardrail, not quota).
	maxSessions int
	// ringTotalCap bounds the sum of all sessions' ring reservations;
	// ringReserved tracks the current sum (placeholders included).
	ringTotalCap int64
	ringReserved int64
	// ringByID remembers each session's reservation for release on Remove
	// (the Session itself may be nil while a spawn is in flight).
	ringByID map[string]int64
	// bus receives every session lifecycle event.
	bus *EventBus
}

// NewRegistry returns an empty registry with its own event bus.
// maxSessions ≤ 0 uses the default; the ring-total cap uses the default
// (override via SetRingTotalCap before serving).
func NewRegistry(maxSessions int) *Registry {
	if maxSessions <= 0 {
		maxSessions = constants.DefaultMaxSessions
	}
	return &Registry{
		byID:         make(map[string]*Session),
		byName:       make(map[string]*Session),
		maxSessions:  maxSessions,
		ringTotalCap: constants.DefaultRingTotalBytes,
		ringByID:     make(map[string]int64),
		bus:          NewEventBus(),
	}
}

// SetRingTotalCap overrides the global ring-memory cap (≤ 0 keeps default).
// Call before serving; not synchronized against in-flight spawns.
func (r *Registry) SetRingTotalCap(capBytes int64) {
	if capBytes > 0 {
		r.ringTotalCap = capBytes
	}
}

// Events returns the registry's event bus (daemon subscribes clients here).
func (r *Registry) Events() *EventBus { return r.bus }

// Spawn validates, mints the id, spawns the PTY, and indexes the session.
func (r *Registry) Spawn(cfg SpawnConfig) (*Session, error) {
	if cfg.Name != "" && !nameRe.MatchString(cfg.Name) {
		return nil, errcodes.Newf(errcodes.InvalidName, "name %q: 1–64 chars of [A-Za-z0-9._-], starting alphanumeric", cfg.Name)
	}

	// Reserve the capacity slot AND the name BEFORE the (slow) spawn so two
	// concurrent spawns can't both pass the checks. nil placeholders are
	// never visible: Lookup/List skip them under the same mutex.
	ringBytes := int64(cfg.RingBytes)
	if ringBytes <= 0 {
		ringBytes = constants.DefaultRingBytes
	}

	r.mu.Lock()
	if len(r.byID) >= r.maxSessions {
		r.mu.Unlock()
		return nil, errcodes.Newf(errcodes.LimitExceeded, "max sessions (%d) reached", r.maxSessions).
			WithHint("kill or reap sessions, or raise --max-sessions")
	}
	if r.ringReserved+ringBytes > r.ringTotalCap {
		r.mu.Unlock()
		return nil, errcodes.Newf(errcodes.LimitExceeded, "ring memory cap reached (%d of %d bytes reserved)", r.ringReserved, r.ringTotalCap).
			WithHint("reap exited sessions or spawn with a smaller --ring")
	}
	if cfg.Name != "" {
		if _, taken := r.byName[cfg.Name]; taken {
			r.mu.Unlock()
			return nil, errcodes.Newf(errcodes.NameTaken, "session name %q already in use", cfg.Name)
		}
	}
	id, err := mintSessionID()
	if err != nil {
		r.mu.Unlock()
		return nil, errcodes.New(errcodes.Internal, "mint session id").WithCause(err)
	}
	r.byID[id] = nil
	if cfg.Name != "" {
		r.byName[cfg.Name] = nil // name reserved; a racing Spawn now sees taken
	}
	r.ringReserved += ringBytes
	r.ringByID[id] = ringBytes
	r.mu.Unlock()

	s, err := spawn(id, cfg, r.bus)

	r.mu.Lock()
	if err != nil {
		delete(r.byID, id)
		if cfg.Name != "" {
			delete(r.byName, cfg.Name)
		}
		r.ringReserved -= r.ringByID[id]
		delete(r.ringByID, id)
		r.mu.Unlock()
		return nil, err
	}
	r.byID[id] = s
	if cfg.Name != "" {
		r.byName[cfg.Name] = s
	}
	r.mu.Unlock()

	r.bus.EmitSession(proto.EventCreated, id, map[string]string{proto.DataKeyCmd: cfg.Cmd, proto.DataKeyName: cfg.Name})
	return s, nil
}

// Adopt inserts a handover-received session (id already minted by the old
// daemon). Ring accounting and the name index update as in Spawn.
func (r *Registry) Adopt(s *Session) error {
	ringBytes := int64(s.RingLen())
	if c := int64(s.ring.capBytes()); c > ringBytes {
		ringBytes = c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[s.ID()]; dup {
		return errcodes.Newf(errcodes.Internal, "adopt: session %s already registered", s.ID())
	}
	if n := s.Name(); n != "" {
		if _, taken := r.byName[n]; taken {
			return errcodes.Newf(errcodes.NameTaken, "adopt: name %q already in use", n)
		}
		r.byName[n] = s
	}
	r.byID[s.ID()] = s
	r.ringReserved += ringBytes
	r.ringByID[s.ID()] = ringBytes
	return nil
}

// Lookup resolves an id (ses_…) or a human name. Ids are checked first —
// they are format-validated, so a name can never shadow one.
func (r *Registry) Lookup(idOrName string) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ids.Validate(idOrName, ids.PrefixSession) == nil {
		if s, ok := r.byID[idOrName]; ok && s != nil {
			return s, nil
		}
		return nil, errcodes.Newf(errcodes.SessionNotFound, "no session with id %s", idOrName)
	}
	if s, ok := r.byName[idOrName]; ok && s != nil {
		return s, nil
	}
	return nil, errcodes.Newf(errcodes.SessionNotFound, "no session named %q", idOrName).
		WithHint("run `" + constants.BinaryName + " ls` to see sessions")
}

// Rename changes a session's unique name ("" clears it).
func (r *Registry) Rename(idOrName, newName string) error {
	if newName != "" && !nameRe.MatchString(newName) {
		return errcodes.Newf(errcodes.InvalidName, "name %q: 1–64 chars of [A-Za-z0-9._-], starting alphanumeric", newName)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var s *Session
	if ids.Validate(idOrName, ids.PrefixSession) == nil {
		s = r.byID[idOrName]
	} else {
		s = r.byName[idOrName]
	}
	if s == nil {
		return errcodes.Newf(errcodes.SessionNotFound, "no session %q", idOrName)
	}
	if newName != "" {
		if holder, taken := r.byName[newName]; taken && holder != s {
			return errcodes.Newf(errcodes.NameTaken, "session name %q already in use", newName)
		}
	}
	if old := s.Name(); old != "" {
		delete(r.byName, old)
	}
	s.setName(newName)
	if newName != "" {
		r.byName[newName] = s
	}
	r.bus.EmitSession(proto.EventRenamed, s.ID(), map[string]string{proto.DataKeyName: newName})
	return nil
}

// ReapExpired removes exited sessions whose retention TTL has passed,
// returning the reaped ids. The daemon's ticker calls this; tests call it
// with a manual `now` — no timers inside.
func (r *Registry) ReapExpired(now time.Time, ttl time.Duration) []string {
	var reaped []string
	for _, s := range r.List() {
		exitedAt, exited := s.ExitedAt()
		if exited && now.Sub(exitedAt) >= ttl {
			r.Remove(s.ID())
			reaped = append(reaped, s.ID())
		}
	}
	return reaped
}

// Remove drops a session from the registry (reap). The caller ensures the
// process is dead (or explicitly wants to orphan it — the daemon never does).
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	if !ok {
		return
	}
	delete(r.byID, id)
	r.ringReserved -= r.ringByID[id]
	delete(r.ringByID, id)
	if s != nil {
		if n := s.Name(); n != "" {
			delete(r.byName, n)
		}
	}
}

// List snapshots every session, sorted by id (UUIDv7 → chronological).
func (r *Registry) List() []*Session {
	r.mu.Lock()
	out := make([]*Session, 0, len(r.byID))
	for _, s := range r.byID {
		if s != nil { // skip in-flight spawn placeholders
			out = append(out, s)
		}
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Count returns the number of registered sessions (including in-flight
// spawn reservations — they hold capacity).
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}
