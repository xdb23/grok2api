package gateway

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

// readyRingShards mirrors segmentedSelectorShards so route keys hash into the same
// independent cursor space without contending on one global counter.
const readyRingShards = 1024

// defaultReadyRingWindow is the max claim attempts per request on the hot path
// before falling back to segmented / full planner.
const defaultReadyRingWindow = 64

// defaultReadyRingMinCandidates gates the ring: tiny pools keep the scored planner
// so priority / free-build ordering still matter when N is small.
const defaultReadyRingMinCandidates = 64

type readyRingConfig struct {
	enabled       bool
	minCandidates int
	windowSize    int
}

type readyRingState struct {
	cursors [readyRingShards]atomic.Uint64
	// proven working set: accounts that completed at least one successful upstream
	// request. RR prefers this set so free-quota burn rotates a hot set instead of
	// striping every virgin import equally (which exhausts the whole pool together).
	provenMu     sync.RWMutex
	provenIDs    []uint64
	provenIndex  map[uint64]int
	provenCursor atomic.Uint64
}

func normalizeReadyRingConfig(value readyRingConfig) readyRingConfig {
	if value.minCandidates < 8 {
		value.minCandidates = defaultReadyRingMinCandidates
	}
	if value.windowSize < 4 || value.windowSize > 512 {
		value.windowSize = defaultReadyRingWindow
	}
	return value
}

// UpdateReadyRing changes the memory ready-ring RR policy used on the non-sticky hot path.
func (s *Selector) UpdateReadyRing(enabled bool, minCandidates, windowSize int) {
	s.configMu.Lock()
	s.readyRingConfig = normalizeReadyRingConfig(readyRingConfig{
		enabled: enabled, minCandidates: minCandidates, windowSize: windowSize,
	})
	s.configMu.Unlock()
}

func (s *Selector) readyRingPolicy() readyRingConfig {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.readyRingConfig
}

// nextReadyRingCursor advances the per-route virgin-pool cursor.
func (s *Selector) nextReadyRingCursor(provider account.Provider, upstreamModel, quotaMode string, windowSize int) uint64 {
	shard := segmentedSelectorShard(provider, upstreamModel, quotaMode) % readyRingShards
	if windowSize < 1 {
		windowSize = 1
	}
	return s.readyRingState.cursors[shard].Add(uint64(windowSize)) - uint64(windowSize)
}

// rememberProvenAccount records a successful account into the working-set ring.
func (s *Selector) rememberProvenAccount(accountID uint64) {
	if accountID == 0 {
		return
	}
	s.readyRingState.provenMu.Lock()
	defer s.readyRingState.provenMu.Unlock()
	if s.readyRingState.provenIndex == nil {
		s.readyRingState.provenIndex = make(map[uint64]int)
	}
	if _, exists := s.readyRingState.provenIndex[accountID]; exists {
		return
	}
	s.readyRingState.provenIndex[accountID] = len(s.readyRingState.provenIDs)
	s.readyRingState.provenIDs = append(s.readyRingState.provenIDs, accountID)
}

// forgetProvenAccount drops an exhausted account from the hot set (optional); it
// can re-enter after MarkSuccess once free quota recovers.
func (s *Selector) forgetProvenAccount(accountID uint64) {
	if accountID == 0 {
		return
	}
	s.readyRingState.provenMu.Lock()
	defer s.readyRingState.provenMu.Unlock()
	index, exists := s.readyRingState.provenIndex[accountID]
	if !exists {
		return
	}
	last := len(s.readyRingState.provenIDs) - 1
	if index != last {
		moved := s.readyRingState.provenIDs[last]
		s.readyRingState.provenIDs[index] = moved
		s.readyRingState.provenIndex[moved] = index
	}
	s.readyRingState.provenIDs = s.readyRingState.provenIDs[:last]
	delete(s.readyRingState.provenIndex, accountID)
}

func (s *Selector) provenSnapshot() []uint64 {
	s.readyRingState.provenMu.RLock()
	defer s.readyRingState.provenMu.RUnlock()
	if len(s.readyRingState.provenIDs) == 0 {
		return nil
	}
	out := make([]uint64, len(s.readyRingState.provenIDs))
	copy(out, s.readyRingState.provenIDs)
	return out
}

func (s *Selector) isProvenAccount(accountID uint64) bool {
	if accountID == 0 {
		return false
	}
	s.readyRingState.provenMu.RLock()
	defer s.readyRingState.provenMu.RUnlock()
	_, ok := s.readyRingState.provenIndex[accountID]
	return ok
}

// acquireReadyRingDirect prefers the proven working-set ring, then discovers
// virgin accounts only when the hot set cannot supply a lease.
func (s *Selector) acquireReadyRingDirect(ctx context.Context, values []account.RoutingCandidate, quotaMode string, provider account.Provider, upstreamModel string, excluded map[uint64]bool, now time.Time) (*accountLease, error) {
	config := s.readyRingPolicy()
	if !config.enabled || len(values) < config.minCandidates {
		return nil, nil
	}
	startedAt := time.Now()
	byID := make(map[uint64]account.RoutingCandidate, len(values))
	for _, candidate := range values {
		byID[candidate.Credential.ID] = candidate
	}

	// Pass 1: rotate proven successful accounts (working set).
	// Advance the proven cursor by 1 each request so the hot set round-robins even
	// when it is smaller than windowSize (Add(window)%len == 0 would stick on index 0).
	if proven := s.provenSnapshot(); len(proven) > 0 {
		window := config.windowSize
		if window > len(proven) {
			window = len(proven)
		}
		start := int(s.readyRingState.provenCursor.Add(1) - 1)
		if lease, tried, err := s.claimProvenPass(ctx, proven, byID, start, window, quotaMode, provider, excluded, now); err != nil {
			observeReadyRing(provider, "error", tried, startedAt)
			return nil, err
		} else if lease != nil {
			observeReadyRing(provider, "selected_proven", tried, startedAt)
			return lease, nil
		}
	}

	// Pass 2: admit a virgin account into the working set when hot stock is thin.
	window := config.windowSize
	if window > len(values) {
		window = len(values)
	}
	scanLimit := window * 4
	if scanLimit > len(values) {
		scanLimit = len(values)
	}
	cursor := s.nextReadyRingCursor(provider, upstreamModel, quotaMode, window)
	start := int(cursor % uint64(len(values)))
	if lease, tried, err := s.claimVirginPass(ctx, values, start, scanLimit, window, quotaMode, provider, excluded, now); err != nil {
		observeReadyRing(provider, "error", tried, startedAt)
		return nil, err
	} else if lease != nil {
		observeReadyRing(provider, "selected_virgin", tried, startedAt)
		return lease, nil
	}
	observeReadyRing(provider, "miss", 0, startedAt)
	return nil, nil
}

func (s *Selector) claimProvenPass(ctx context.Context, proven []uint64, byID map[uint64]account.RoutingCandidate, start, window int, quotaMode string, provider account.Provider, excluded map[uint64]bool, now time.Time) (*accountLease, int, error) {
	if len(proven) == 0 {
		return nil, 0, nil
	}
	tried := 0
	// Walk up to len(proven) so a sparse cool-down set still finds live members.
	limit := max(window, min(len(proven), window*4))
	for offset := 0; offset < limit && tried < window; offset++ {
		accountID := proven[(start+offset)%len(proven)]
		if excluded != nil && excluded[accountID] {
			continue
		}
		candidate, ok := byID[accountID]
		if !ok || !readyRingEligible(candidate, now) {
			continue
		}
		tried++
		lease, err := s.claimAccountSlot(ctx, candidate.Credential)
		if err != nil {
			return nil, tried, err
		}
		if lease == nil {
			continue
		}
		lease.Billing = candidate.Billing
		lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
		lease.selectorObservation = &selectorLeaseObservation{provider: provider, stage: "ready_ring_proven"}
		return lease, tried, nil
	}
	return nil, tried, nil
}

func (s *Selector) claimVirginPass(ctx context.Context, values []account.RoutingCandidate, start, scanLimit, window int, quotaMode string, provider account.Provider, excluded map[uint64]bool, now time.Time) (*accountLease, int, error) {
	tried := 0
	for offset := 0; offset < scanLimit && tried < window; offset++ {
		candidate := values[(start+offset)%len(values)]
		if excluded != nil && excluded[candidate.Credential.ID] {
			continue
		}
		if s.isProvenAccount(candidate.Credential.ID) {
			continue
		}
		if !readyRingEligible(candidate, now) {
			continue
		}
		tried++
		lease, err := s.claimAccountSlot(ctx, candidate.Credential)
		if err != nil {
			return nil, tried, err
		}
		if lease == nil {
			continue
		}
		lease.Billing = candidate.Billing
		lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
		lease.selectorObservation = &selectorLeaseObservation{provider: provider, stage: "ready_ring_virgin"}
		return lease, tried, nil
	}
	return nil, tried, nil
}

// readyRingEligible mirrors the normal-candidate filter without allocating indexes.
func readyRingEligible(candidate account.RoutingCandidate, now time.Time) bool {
	value := candidate.Credential
	if value.AuthStatus != account.AuthStatusActive || !value.Enabled {
		return false
	}
	if candidate.ModelCapabilityKnown && !candidate.SupportsModel {
		return false
	}
	if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
		return false
	}
	if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
		return false
	}
	if candidate.QuotaRecovery != nil && candidate.QuotaRecovery.Status != account.QuotaRecoveryStatusActive {
		return false
	}
	if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
		return false
	}
	if candidate.QuotaWindow != nil && candidate.QuotaWindow.Remaining <= 0 {
		return false
	}
	return true
}

func observeReadyRing(provider account.Provider, outcome string, tried int, startedAt time.Time) {
	labels := perfmetrics.Labels{
		Subsystem: "selector", Operation: "ready_ring", Provider: string(provider),
		Stage: "rr", Outcome: outcome,
	}
	perfmetrics.Default.Inc("selector_ready_ring_total", labels)
	if tried > 0 {
		perfmetrics.Default.Add("selector_ready_ring_attempts", labels, int64(tried))
	}
	perfmetrics.Default.ObserveDuration("selector_ready_ring_duration_us", labels, time.Since(startedAt))
}
