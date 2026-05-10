// Package model contains the core data types shared by the cluster
// supervisor and (later) the UI.
package model

import (
	"sync"
	"time"
)

// Reach is the reachability/health classification of a single cluster.
type Reach uint8

const (
	ReachUnknown     Reach = iota // not yet probed
	ReachConnecting               // probe in flight, no result yet
	ReachHealthy                  // last probe succeeded
	ReachDegraded                 // partial: e.g. version OK, RBAC limited
	ReachUnreachable              // network / TLS / DNS failure
	ReachAuthFailed               // 401/403/exec-plugin error
)

// String returns a short human label.
func (r Reach) String() string {
	switch r {
	case ReachUnknown:
		return "unknown"
	case ReachConnecting:
		return "connecting"
	case ReachHealthy:
		return "healthy"
	case ReachDegraded:
		return "degraded"
	case ReachUnreachable:
		return "unreachable"
	case ReachAuthFailed:
		return "auth-failed"
	}
	return "?"
}

// Glyph returns the single-cell status glyph used in the rail.
// Shape carries the signal even when color is unavailable.
func (r Reach) Glyph() string {
	switch r {
	case ReachHealthy:
		return "●"
	case ReachConnecting:
		return "◐"
	case ReachDegraded:
		return "⚠"
	case ReachUnreachable, ReachAuthFailed:
		return "✕"
	}
	return "○"
}

// ClusterState is a snapshot of one cluster's current state.
// The struct is intentionally small; it's safe to copy by value.
type ClusterState struct {
	Context       string // unique key (may include disambiguation suffix)
	RawName       string // user-facing context name, no suffix
	File          string // source kubeconfig file path
	Reach         Reach
	ServerVersion string        // e.g. "v1.30.5"
	NodeCount     int           // -1 = unknown / RBAC denied
	NodeReady     int           // count of nodes with Ready=True
	LastError     string        // empty when healthy
	LastProbe     time.Time     // zero if never probed
	ProbeLatency  time.Duration // RTT of the last successful probe

	// Aggregated resources across all nodes. Filled by the probe
	// (Allocatable) and the metrics ticker (Usage). Zero values mean
	// "not yet measured" or "metrics-server absent".
	AllocCPUMilli int64 // sum of node Allocatable CPU in millicores
	AllocMemBytes int64 // sum of node Allocatable memory in bytes
	UsageCPUMilli int64 // sum of node usage CPU in millicores
	UsageMemBytes int64 // sum of node usage memory in bytes

	MetricsAvailable bool // false = metrics-server absent or last call failed
	MetricsAt        time.Time
}

// Store is a concurrency-safe map of cluster context name -> latest state.
// Reads return value copies; writes replace the slot.
type Store struct {
	mu sync.RWMutex
	m  map[string]ClusterState
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{m: make(map[string]ClusterState)}
}

// Set replaces the state for ctx.
func (s *Store) Set(ctx string, st ClusterState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[ctx] = st
}

// Get returns the state for ctx and whether it exists.
func (s *Store) Get(ctx string) (ClusterState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.m[ctx]
	return st, ok
}

// Snapshot returns an unordered slice of all current states. The
// docstring used to claim sorting; in practice every caller re-sorts
// for its own ordering, so leaving it sorted here was just lying
// about the contract.
func (s *Store) Snapshot() []ClusterState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ClusterState, 0, len(s.m))
	for _, st := range s.m {
		out = append(out, st)
	}
	return out
}

// ProbeFields are the slots the probe loop owns. ApplyProbe overwrites
// them under the store lock without touching metrics-owned slots, so a
// concurrent ApplyMetrics can't be clobbered by a probe that snapshot-
// then-took-10s-of-API-calls-then-committed-stale-metrics.
type ProbeFields struct {
	RawName       string
	File          string
	Reach         Reach
	ServerVersion string
	NodeCount     int
	NodeReady     int
	LastError     string
	LastProbe     time.Time
	ProbeLatency  time.Duration
	AllocCPUMilli int64
	AllocMemBytes int64
}

// MetricsFields are the slots the metrics ticker owns.
type MetricsFields struct {
	UsageCPUMilli    int64
	UsageMemBytes    int64
	MetricsAvailable bool
	MetricsAt        time.Time
}

// ApplyProbe atomically merges probe-owned fields into the slot at
// ctx. Metrics-owned fields are preserved.
func (s *Store) ApplyProbe(ctx string, p ProbeFields) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.m[ctx]
	st.Context = ctx
	st.RawName = p.RawName
	st.File = p.File
	st.Reach = p.Reach
	st.ServerVersion = p.ServerVersion
	st.NodeCount = p.NodeCount
	st.NodeReady = p.NodeReady
	st.LastError = p.LastError
	st.LastProbe = p.LastProbe
	st.ProbeLatency = p.ProbeLatency
	st.AllocCPUMilli = p.AllocCPUMilli
	st.AllocMemBytes = p.AllocMemBytes
	s.m[ctx] = st
}

// ApplyMetrics atomically merges metrics-owned fields. Probe-owned
// fields are preserved. Slot must already exist (probe runs first).
func (s *Store) ApplyMetrics(ctx string, mf MetricsFields) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[ctx]
	if !ok {
		// No probe has populated this slot yet — drop the metrics
		// rather than initialise a partially-known state. Next probe
		// tick will create the slot and a follow-up metrics tick will
		// fill it.
		return
	}
	st.UsageCPUMilli = mf.UsageCPUMilli
	st.UsageMemBytes = mf.UsageMemBytes
	st.MetricsAvailable = mf.MetricsAvailable
	st.MetricsAt = mf.MetricsAt
	s.m[ctx] = st
}
