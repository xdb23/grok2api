package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

type accountLease struct {
	Credential          account.Credential
	Billing             *account.Billing
	QuotaProbe          bool
	QuotaProbeKind      account.QuotaRecoveryKind
	QuotaMode           string
	// StickyMode explains how session affinity influenced this lease (for cache diagnostics).
	// hit=bound account; borrow=temporary other account; rebind=forced new bind; bind=first bind; none=no affinity.
	StickyMode          string
	StickyBoundID        uint64 // original sticky account when borrow/rebind
	selectorObservation *selectorLeaseObservation
	release             func()
}

const (
	stickyModeNone   = "none"
	stickyModeHit    = "hit"
	stickyModeBorrow = "borrow"
	stickyModeRebind = "rebind"
	stickyModeBind   = "bind"
)

const quotaProbeLease = 5 * time.Minute
const successPersistInterval = 30 * time.Second
// candidateCacheTTL keeps the routing candidate snapshot warm across requests.
// Token refresh must NOT wipe this cache (see UpdateTokens); secrets are rebound
// per-request via accounts.Get after Acquire so a multi-minute TTL is safe and
// critical for large build pools (tens of thousands of accounts).
// candidateCacheTTL is the fresh window. After invalidation we keep serving the
// last snapshot (SWR) until a background refresh completes, so large pools do
// not re-block every request on multi-second SQLite reloads.
const candidateCacheTTL = 10 * time.Minute
const concurrencySnapshotTTL = 25 * time.Millisecond
const maxConcurrencySnapshots = 256

const modelAccessDeniedCooldown = 5 * time.Minute

const defaultFreeQuotaRecoveryPause = 24 * time.Hour

// paymentRequiredRecoveryPause is only the final fallback for a 402 account
// without an upstream reset, Retry-After, or parseable billing period.
const paymentRequiredRecoveryPause = 20 * time.Hour

type quotaRecoveryHints struct {
	Billing    *account.Billing
	QuotaMode  string
	RetryAfter time.Duration
	Fallback   time.Duration
}

type candidateSnapshot struct {
	values    []account.RoutingCandidate
	byAccount map[uint64]int
	expiresAt time.Time
}

func newCandidateSnapshot(values []account.RoutingCandidate, expiresAt time.Time) candidateSnapshot {
	byAccount := make(map[uint64]int, len(values))
	for index, value := range values {
		if _, exists := byAccount[value.Credential.ID]; !exists {
			byAccount[value.Credential.ID] = index
		}
	}
	return candidateSnapshot{values: values, byAccount: byAccount, expiresAt: expiresAt}
}

type candidateCacheKey struct {
	provider      account.Provider
	upstreamModel string
	quotaMode     string
}

type routingBaseCacheKey struct {
	provider  account.Provider
	quotaMode string
}

type routingOverlayCacheKey struct {
	provider      account.Provider
	upstreamModel string
}

type routingLayerVersion struct {
	global   uint64
	provider uint64
}

type routingBaseSnapshot struct {
	values    []account.RoutingAccountBase
	version   routingLayerVersion
	expiresAt time.Time
}

type routingOverlaySnapshot struct {
	value     account.RoutingOverlaySnapshot
	version   routingLayerVersion
	expiresAt time.Time
}

type SelectionUnavailableReason string

const (
	SelectionNoAccounts       SelectionUnavailableReason = "no_accounts"
	SelectionUnsupportedModel SelectionUnavailableReason = "unsupported_model"
	SelectionCooling          SelectionUnavailableReason = "cooling"
	SelectionModelCooling     SelectionUnavailableReason = "model_cooling"
	SelectionQuotaExhausted   SelectionUnavailableReason = "quota_exhausted"
	SelectionSaturated        SelectionUnavailableReason = "saturated"
)

// SelectionUnavailableError 保留选号失败的真实原因，避免所有情况都退化成模糊的 503。
type SelectionUnavailableError struct {
	Reason     SelectionUnavailableReason
	RetryAfter time.Duration
}

func (e *SelectionUnavailableError) Error() string {
	if e == nil {
		return "没有可用上游账号"
	}
	switch e.Reason {
	case SelectionUnsupportedModel:
		return "当前账号池不支持该模型"
	case SelectionCooling:
		return "可用上游账号正在冷却"
	case SelectionModelCooling:
		return "可用上游账号的目标模型正在冷却"
	case SelectionQuotaExhausted:
		return "可用上游账号额度等待恢复"
	case SelectionSaturated:
		return "可用上游账号均达到并发上限"
	default:
		return "没有可用上游账号"
	}
}

func (l *accountLease) Release() {
	if l == nil {
		return
	}
	if l.selectorObservation != nil {
		l.selectorObservation.completeRelease()
	}
	if l.release != nil {
		l.release()
		l.release = nil
	}
}

func (l *accountLease) markSelectorUpstreamStarted() {
	if l != nil && l.selectorObservation != nil {
		l.selectorObservation.upstreamStarted.Store(true)
	}
}

func (l *accountLease) completeSelectorObservation(success bool) {
	if l != nil && l.selectorObservation != nil {
		l.selectorObservation.complete(success)
	}
}

// Selector 实现可替换的 balanced 账号选择策略。
type Selector struct {
	accounts               repository.AccountRepository
	concurrency            repository.ConcurrencyLimiter
	sticky                 repository.StickySessionRepository
	stickyTTL              time.Duration
	cooldownBase           time.Duration
	cooldownMax            time.Duration
	capacityWait           time.Duration
	// stickyCapacityWait is how long Acquire waits on a bound sticky account
	// before temporary borrow. Zero means fall back to capacityWait.
	stickyCapacityWait     time.Duration
	// minAccountConcurrent floors MaxConcurrent at claim time (0 = disabled).
	minAccountConcurrent   int
	// loadFactor reports process in-flight load in [0,1+] for sticky-wait shedding.
	loadFactor             func() float64
	preferFreeBuild        bool
	segmentedConfig        segmentedSelectorConfig
	segmentedState         segmentedSelectorState
	readyRingConfig        readyRingConfig
	readyRingState         readyRingState
	configMu               sync.RWMutex
	candidateMu            sync.Mutex
	selectionMu            sync.RWMutex
	leaseWakeMu            sync.Mutex
	leaseWake              chan struct{}
	lastSelectedAt         map[uint64]time.Time
	lastSuccessAt          map[uint64]time.Time
	candidates             map[candidateCacheKey]candidateSnapshot
	routingBases           map[routingBaseCacheKey]routingBaseSnapshot
	routingOverlays        map[routingOverlayCacheKey]routingOverlaySnapshot
	baseGlobalVersion      uint64
	overlayGlobalVersion   uint64
	baseProviderVersion    map[account.Provider]uint64
	overlayProviderVersion map[account.Provider]uint64
	candidateLoads         singleflight.Group
	concurrencySnapshots   *resultcache.Cache[[32]byte, map[string]int]
	tierOrders             interface {
		TierOrder(account.Provider, string) []account.WebTier
	}
}

func NewSelector(accounts repository.AccountRepository, concurrency repository.ConcurrencyLimiter, sticky repository.StickySessionRepository, tierOrders interface {
	TierOrder(account.Provider, string) []account.WebTier
}, stickyTTL, cooldownBase, cooldownMax time.Duration, capacityWait ...time.Duration) *Selector {
	wait := time.Duration(0)
	if len(capacityWait) > 0 && capacityWait[0] > 0 {
		wait = capacityWait[0]
	}
	stickyWait := time.Duration(0)
	if len(capacityWait) > 1 && capacityWait[1] > 0 {
		stickyWait = capacityWait[1]
	}
	return &Selector{accounts: accounts, concurrency: concurrency, sticky: sticky, tierOrders: tierOrders, stickyTTL: stickyTTL, cooldownBase: cooldownBase, cooldownMax: cooldownMax, capacityWait: wait, stickyCapacityWait: stickyWait, leaseWake: make(chan struct{}), lastSelectedAt: make(map[uint64]time.Time), lastSuccessAt: make(map[uint64]time.Time), candidates: make(map[candidateCacheKey]candidateSnapshot), routingBases: make(map[routingBaseCacheKey]routingBaseSnapshot), routingOverlays: make(map[routingOverlayCacheKey]routingOverlaySnapshot), baseProviderVersion: make(map[account.Provider]uint64), overlayProviderVersion: make(map[account.Provider]uint64), concurrencySnapshots: resultcache.New[[32]byte, map[string]int](maxConcurrencySnapshots, concurrencySnapshotTTL)}
}

func (s *Selector) UpdateConfig(stickyTTL, cooldownBase, cooldownMax time.Duration, capacityWait ...time.Duration) {
	s.configMu.Lock()
	s.stickyTTL = stickyTTL
	s.cooldownBase = cooldownBase
	s.cooldownMax = cooldownMax
	if len(capacityWait) > 0 {
		s.capacityWait = max(time.Duration(0), capacityWait[0])
	}
	if len(capacityWait) > 1 {
		s.stickyCapacityWait = max(time.Duration(0), capacityWait[1])
	}
	s.configMu.Unlock()
}

// UpdateMinAccountConcurrent floors per-account concurrency at claim time.
func (s *Selector) UpdateMinAccountConcurrent(min int) {
	s.configMu.Lock()
	s.minAccountConcurrent = max(0, min)
	s.configMu.Unlock()
}

// SetLoadFactor wires an optional process load gauge (e.g. inference concurrency gate).
func (s *Selector) SetLoadFactor(fn func() float64) {
	s.configMu.Lock()
	s.loadFactor = fn
	s.configMu.Unlock()
}

// UpdatePreferFreeBuild 热更新 Build Free 账号优先策略。
func (s *Selector) UpdatePreferFreeBuild(value bool) {
	s.configMu.Lock()
	s.preferFreeBuild = value
	s.configMu.Unlock()
}

// UpdateSegmentedSelector changes the large-pool bounded planner policy.
func (s *Selector) UpdateSegmentedSelector(enabled bool, minCandidates, windowSize int) {
	s.configMu.Lock()
	s.segmentedConfig = normalizeSegmentedSelectorConfig(segmentedSelectorConfig{
		enabled: enabled, minCandidates: minCandidates, windowSize: windowSize,
	})
	s.configMu.Unlock()
}

func (s *Selector) routingConfig() (time.Duration, time.Duration, time.Duration, time.Duration) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.stickyTTL, s.cooldownBase, s.cooldownMax, s.capacityWait
}

// stickyWaitDuration returns how long to wait on a bound sticky account before temporary borrow.
// Under high process load the wait is shortened so sticky sessions do not convoy every request.
// Thresholds are intentionally conservative: medium load still prefers a short sticky wait
// (hot cache saves seconds of upstream TTFT) and only sheds hard near gate saturation.
func (s *Selector) stickyWaitDuration() time.Duration {
	s.configMu.RLock()
	base := s.stickyCapacityWait
	if base <= 0 {
		base = s.capacityWait
	}
	loadFn := s.loadFactor
	s.configMu.RUnlock()
	if base <= 0 {
		return 0
	}
	load := 0.0
	if loadFn != nil {
		load = loadFn()
	}
	// Prefer waiting on the sticky account: cold cache after borrow costs seconds of
	// upstream TTFT. Only shed hard when the process gate is essentially full.
	switch {
	case load >= 0.98:
		return min(base, 200*time.Millisecond)
	case load >= 0.90:
		return min(base, 750*time.Millisecond)
	case load >= 0.80:
		return min(base, 1500*time.Millisecond)
	default:
		// Idle / moderate / busy-but-not-saturated: full sticky wait (default 3s).
		return base
	}
}

func (s *Selector) accountConcurrencyLimit(value account.Credential) int {
	limit := value.MaxConcurrent
	if limit <= 0 {
		limit = account.DefaultMaxConcurrent
	}
	s.configMu.RLock()
	minLimit := s.minAccountConcurrent
	s.configMu.RUnlock()
	if minLimit > limit {
		return minLimit
	}
	return limit
}

// stickyAccountConcurrencyLimit raises the floor for sticky-bound accounts so multi-agent
// parallel turns on the same session stay on the warm cache account instead of borrowing.
func (s *Selector) stickyAccountConcurrencyLimit(value account.Credential) int {
	limit := s.accountConcurrencyLimit(value)
	// Prefer keeping sticky sessions on-account: free-tier imports often have max_concurrent=1.
	const stickyFloor = 16
	if limit < stickyFloor {
		return stickyFloor
	}
	return limit
}

func (s *Selector) preferFreeBuildEnabled() bool {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.preferFreeBuild
}

func (s *Selector) Acquire(ctx context.Context, provider account.Provider, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool) (*accountLease, error) {
	now := time.Now().UTC()
	stickyKeys := stickyKeysFromAffinity(affinityKey)
	stickyKey := ""
	if len(stickyKeys) > 0 {
		stickyKey = stickyKeys[0] // primary; multi-key helpers use stickyKeys
	}
	values, err := s.loadCandidates(ctx, provider, upstreamModel, quotaMode, now)
	if err != nil {
		return nil, err
	}
	// Non-sticky large pools: try O(window) ready-ring BEFORE O(N) full eligibility scan.
	// Success path never materializes normalCandidates for tens of thousands of accounts.
	if stickyKey == "" {
		if lease, ringErr := s.acquireReadyRingDirect(ctx, values, quotaMode, provider, upstreamModel, excluded, now); ringErr != nil {
			return nil, ringErr
		} else if lease != nil {
			return lease, nil
		}
	}
	// 仅保留候选下标，避免每个请求复制包含凭据、计费和额度结构的完整账号切片。
	normalCandidates := make([]int, 0, len(values))
	probeCandidates := make([]int, 0, len(values))
	supportedCandidates := 0
	consideredCandidates := 0
	coolingCandidates := 0
	modelCoolingCandidates := 0
	quotaCandidates := 0
	var earliestRetry time.Time
	for index, candidate := range values {
		value := candidate.Credential
		if excluded[value.ID] || value.AuthStatus != account.AuthStatusActive {
			continue
		}
		consideredCandidates++
		if candidate.ModelCapabilityKnown && !candidate.SupportsModel {
			continue
		}
		supportedCandidates++
		if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
			modelCoolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, candidate.ModelQuotaBlock.CooldownUntil, now)
			continue
		}
		if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
			coolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, *value.CooldownUntil, now)
			continue
		}
		quotaRecovery := candidate.QuotaRecovery
		if quotaRecovery != nil && quotaRecovery.Status != account.QuotaRecoveryStatusActive {
			if allowQuotaProbe && quotaRecovery.NextProbeAt != nil && !now.Before(*quotaRecovery.NextProbeAt) {
				probeCandidates = append(probeCandidates, index)
			} else {
				quotaCandidates++
				if quotaRecovery.NextProbeAt != nil {
					earliestRetry = earlierFuture(earliestRetry, *quotaRecovery.NextProbeAt, now)
				}
			}
			continue
		}
		if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
			quotaCandidates++
			continue
		}
		if candidate.QuotaWindow != nil && candidate.QuotaWindow.Remaining <= 0 {
			quotaCandidates++
			if candidate.QuotaWindow.ResetAt != nil {
				earliestRetry = earlierFuture(earliestRetry, *candidate.QuotaWindow.ResetAt, now)
			}
			continue
		}
		normalCandidates = append(normalCandidates, index)
	}
	if len(normalCandidates) == 0 && len(probeCandidates) == 0 {
		reason := SelectionNoAccounts
		switch {
		case consideredCandidates > 0 && supportedCandidates == 0:
			reason = SelectionUnsupportedModel
		case modelCoolingCandidates > 0:
			reason = SelectionModelCooling
		case coolingCandidates > 0:
			reason = SelectionCooling
		case quotaCandidates > 0:
			reason = SelectionQuotaExhausted
		}
		return nil, &SelectionUnavailableError{Reason: reason, RetryAfter: retryDelay(now, earliestRetry)}
	}
	if len(probeCandidates) > 0 {
		plan, err := s.planCandidateIndexes(ctx, values, probeCandidates, now, s.resolveTierOrder(provider, upstreamModel))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			lease, err := s.claimAccountSlot(ctx, candidate.Credential)
			if err != nil {
				return nil, err
			}
			if lease == nil {
				continue
			}
			claimed, err := s.accounts.ClaimQuotaProbe(ctx, candidate.Credential.ID, now, now.Add(quotaProbeLease))
			if err != nil || !claimed {
				lease.Release()
				if err != nil {
					return nil, err
				}
				continue
			}
			lease.QuotaProbe = true
			lease.QuotaProbeKind = candidate.QuotaRecovery.Kind
			lease.Billing = candidate.Billing
			return lease, nil
		}
	}
	// stickyPreserveID: bound account that is only temporarily unavailable (saturated /
	// request-excluded). Temporary borrow must NOT rebind sticky so multi-turn cache stays on-account.
	// stickyRebindFrom: bound account permanently ineligible (quota/cool/auth) — force Set to the new account.
	var saturatedStickyID uint64
	var stickyPreserveID uint64
	var stickyRebindFrom uint64
	if len(stickyKeys) > 0 {
		stickyID, _, ok, err := s.stickyGetAny(ctx, stickyKeys, now)
		if err != nil {
			return nil, fmt.Errorf("读取会话粘滞状态: %w", err)
		}
		if ok {
			candidate, eligible := routingCandidateByID(values, normalCandidates, stickyID)
			if eligible {
				stickyTTL, _, _, _ := s.routingConfig()
				boundID, bindErr := s.stickyBindAll(ctx, stickyKeys, stickyID, now, now.Add(stickyTTL))
				if bindErr != nil {
					return nil, fmt.Errorf("刷新会话粘滞状态: %w", bindErr)
				}
				if boundID != stickyID {
					candidate, eligible = routingCandidateByID(values, normalCandidates, boundID)
					stickyID = boundID
				}
				if eligible {
					// Strong sticky: wait on the bound account before any temporary borrow.
					lease, acquireErr := s.acquirePinnedCapacityWithWait(ctx, candidate.Credential, s.stickyWaitDuration())
					if acquireErr == nil {
						lease.Billing = candidate.Billing
						lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
						lease.StickyMode = stickyModeHit
						lease.StickyBoundID = stickyID
						return lease, nil
					}
					if !isSelectionUnavailable(acquireErr, SelectionSaturated) {
						return nil, acquireErr
					}
					saturatedStickyID = stickyID
					stickyPreserveID = stickyID
				}
			} else if stickyAccountPoolEligible(values, stickyID, now) {
				// Bound account is pool-healthy but excluded for this request (e.g. capacity rotate).
				// Borrow elsewhere without rebinding so the next turn can return to the cache-warm account.
				stickyPreserveID = stickyID
			} else {
				// Quota exhausted / cooling / disabled: clear affinity so the new account can rebuild cache.
				stickyRebindFrom = stickyID
			}
		}
	}
	// 粘性账号仅因并发满载而暂时不可用时，已在上面等待过；超时后允许本次请求临时借用
	// 其他账号，但不覆盖原绑定，避免并行请求让活跃会话在账号池中来回抖动。
	if saturatedStickyID != 0 {
		plan, err := s.planCandidateIndexes(ctx, values, normalCandidates, time.Now().UTC(), s.resolveTierOrder(provider, upstreamModel))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			if candidate.Credential.ID == saturatedStickyID {
				continue
			}
			lease, claimErr := s.claimAccountSlot(ctx, candidate.Credential)
			if claimErr != nil {
				return nil, claimErr
			}
			if lease == nil {
				continue
			}
			lease.Billing = candidate.Billing
			lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
			lease.StickyMode = stickyModeBorrow
			lease.StickyBoundID = saturatedStickyID
			return lease, nil
		}
		return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
	}
	if stickyKey == "" {
		// Ready-ring already attempted above; segmented then full planner.
		activeRequest := s.nextSegmentedActiveRequest(provider, upstreamModel, quotaMode, len(normalCandidates))
		if activeRequest != nil {
			return s.acquireSegmentedCandidates(ctx, values, normalCandidates, quotaMode, s.resolveTierOrder(provider, upstreamModel), *activeRequest)
		}
	}
	_, _, _, capacityWait := s.routingConfig()
	waitDeadline := time.Now().Add(capacityWait)
	for {
		currentTime := time.Now().UTC()
		plan, err := s.planCandidateIndexes(ctx, values, normalCandidates, currentTime, s.resolveTierOrder(provider, upstreamModel))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			if stickyPreserveID != 0 && candidate.Credential.ID == stickyPreserveID {
				continue
			}
			lease, err := s.claimAccountSlot(ctx, candidate.Credential)
			if err != nil {
				return nil, err
			}
			if lease == nil {
				continue
			}
			stickyMode := stickyModeNone
			stickyBound := uint64(0)
			if stickyKey != "" {
				stickyTTL, _, _, _ := s.routingConfig()
				if stickyPreserveID != 0 {
					// Temporary borrow: refresh TTL on the original binding only (all dual keys).
					if _, bindErr := s.stickyBindAll(ctx, stickyKeys, stickyPreserveID, currentTime, currentTime.Add(stickyTTL)); bindErr != nil {
						lease.Release()
						return nil, fmt.Errorf("刷新会话粘滞状态: %w", bindErr)
					}
					stickyMode = stickyModeBorrow
					stickyBound = stickyPreserveID
				} else if stickyRebindFrom != 0 {
					// Permanent ineligibility (quota/cool): rebind so subsequent turns warm cache on the new account.
					if err := s.stickySetAll(ctx, stickyKeys, candidate.Credential.ID, currentTime.Add(stickyTTL)); err != nil {
						lease.Release()
						return nil, fmt.Errorf("重建会话粘滞状态: %w", err)
					}
					stickyMode = stickyModeRebind
					stickyBound = stickyRebindFrom
				} else {
					boundID, bindErr := s.stickyBindAll(ctx, stickyKeys, candidate.Credential.ID, currentTime, currentTime.Add(stickyTTL))
					if bindErr != nil {
						lease.Release()
						return nil, fmt.Errorf("写入会话粘滞状态: %w", bindErr)
					}
					if boundID != candidate.Credential.ID {
						if boundCandidate, eligible := routingCandidateByID(values, normalCandidates, boundID); eligible {
							boundLease, boundErr := s.acquirePinnedCapacityWithWait(ctx, boundCandidate.Credential, s.stickyWaitDuration())
							if boundErr == nil {
								lease.Release()
								boundLease.Billing = boundCandidate.Billing
								boundLease.QuotaMode = effectiveQuotaMode(boundCandidate, quotaMode)
								boundLease.StickyMode = stickyModeHit
								boundLease.StickyBoundID = boundID
								return boundLease, nil
							}
							if !isSelectionUnavailable(boundErr, SelectionSaturated) {
								lease.Release()
								return nil, boundErr
							}
							// Bound account saturated: keep original sticky, use temporary lease.
							stickyMode = stickyModeBorrow
							stickyBound = boundID
						} else if stickyAccountPoolEligible(values, boundID, currentTime) {
							// Bound still pool-healthy but not selectable this request — keep sticky.
							stickyMode = stickyModeBorrow
							stickyBound = boundID
						} else if err := s.stickySetAll(ctx, stickyKeys, candidate.Credential.ID, currentTime.Add(stickyTTL)); err != nil {
							lease.Release()
							return nil, fmt.Errorf("重建会话粘滞状态: %w", err)
						} else {
							stickyMode = stickyModeRebind
							stickyBound = boundID
						}
					} else {
						stickyMode = stickyModeBind
						stickyBound = candidate.Credential.ID
					}
				}
			}
			lease.Billing = candidate.Billing
			lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
			lease.StickyMode = stickyMode
			lease.StickyBoundID = stickyBound
			return lease, nil
		}
		if capacityWait <= 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, waitDeadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

// stickySessionKey 将调用方粘滞 identity 压缩为固定长度，仅用于账号粘滞索引。
func stickySessionKey(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// stickyKeysFromAffinity expands a (possibly dual) affinity key into sticky store keys.
// Soft sessions may pass primary\x1efallback so turn-2+ inherits the turn-1 binding (CPA-style).
func stickyKeysFromAffinity(affinityKey string) []string {
	if affinityKey == "" {
		return nil
	}
	parts := strings.Split(affinityKey, affinityKeySeparator)
	keys := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		key := stickySessionKey(part)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

func (s *Selector) stickyGetAny(ctx context.Context, keys []string, now time.Time) (accountID uint64, hitKey string, ok bool, err error) {
	for _, key := range keys {
		id, found, getErr := s.sticky.Get(ctx, key, now)
		if getErr != nil {
			return 0, "", false, getErr
		}
		if found {
			return id, key, true, nil
		}
	}
	return 0, "", false, nil
}

func (s *Selector) stickySetAll(ctx context.Context, keys []string, accountID uint64, expiresAt time.Time) error {
	for _, key := range keys {
		if err := s.sticky.Set(ctx, key, accountID, expiresAt); err != nil {
			return err
		}
	}
	return nil
}

// stickyBindAll binds/refreshes every affinity key onto the same account. If any key is
// already bound to a different account, that existing binding wins (lookup order).
// Primary key is refreshed via Bind (TTL update); sibling keys are Set to the same account.
func (s *Selector) stickyBindAll(ctx context.Context, keys []string, proposedAccountID uint64, now, expiresAt time.Time) (uint64, error) {
	if len(keys) == 0 || proposedAccountID == 0 {
		return 0, nil
	}
	targetID := proposedAccountID
	if id, _, ok, err := s.stickyGetAny(ctx, keys, now); err != nil {
		return 0, err
	} else if ok {
		targetID = id
	}
	boundID, err := s.sticky.Bind(ctx, keys[0], targetID, now, expiresAt)
	if err != nil {
		return 0, err
	}
	for _, key := range keys[1:] {
		if err := s.sticky.Set(ctx, key, boundID, expiresAt); err != nil {
			return 0, err
		}
	}
	return boundID, nil
}

func routingCandidateByID(values []account.RoutingCandidate, indexes []int, accountID uint64) (account.RoutingCandidate, bool) {
	for _, index := range indexes {
		candidate := values[index]
		if candidate.Credential.ID == accountID {
			return candidate, true
		}
	}
	return account.RoutingCandidate{}, false
}

// stickyAccountPoolEligible reports whether the bound account is still usable for
// future sticky hits (active, model-capable, not cooling, not quota-exhausted).
// Request-level exclusion and concurrency saturation are intentionally ignored.
func stickyAccountPoolEligible(values []account.RoutingCandidate, accountID uint64, now time.Time) bool {
	for _, candidate := range values {
		if candidate.Credential.ID != accountID {
			continue
		}
		value := candidate.Credential
		if value.AuthStatus != account.AuthStatusActive {
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
		if recovery := candidate.QuotaRecovery; recovery != nil && recovery.Status != account.QuotaRecoveryStatusActive {
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
	return false
}

func isSelectionUnavailable(err error, reason SelectionUnavailableReason) bool {
	var unavailable *SelectionUnavailableError
	return errors.As(err, &unavailable) && unavailable.Reason == reason
}

// AcquirePinned 为 previous_response_id 等账号归属请求获取指定账号租约。
func (s *Selector) AcquirePinned(ctx context.Context, provider account.Provider, accountID uint64, upstreamModel, quotaMode string, inference bool) (*accountLease, error) {
	now := time.Now().UTC()
	values, err := s.loadCandidates(ctx, provider, upstreamModel, quotaMode, now)
	if err != nil {
		return nil, err
	}
	for _, candidate := range values {
		value := candidate.Credential
		if value.ID != accountID {
			continue
		}
		if !value.Enabled || value.AuthStatus != account.AuthStatusActive {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
		if inference {
			if candidate.ModelCapabilityKnown && !candidate.SupportsModel {
				return nil, &SelectionUnavailableError{Reason: SelectionUnsupportedModel}
			}
			if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
				return nil, &SelectionUnavailableError{Reason: SelectionModelCooling, RetryAfter: retryDelay(now, candidate.ModelQuotaBlock.CooldownUntil)}
			}
			if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
				return nil, &SelectionUnavailableError{Reason: SelectionCooling, RetryAfter: retryDelay(now, *value.CooldownUntil)}
			}
			if recovery := candidate.QuotaRecovery; recovery != nil && recovery.Status != account.QuotaRecoveryStatusActive {
				if recovery.NextProbeAt == nil || now.Before(*recovery.NextProbeAt) {
					var retryAfter time.Duration
					if recovery.NextProbeAt != nil {
						retryAfter = retryDelay(now, *recovery.NextProbeAt)
					}
					return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
				}
				lease, err := s.acquirePinnedCapacity(ctx, value)
				if err != nil {
					return nil, err
				}
				claimed, err := s.accounts.ClaimQuotaProbe(ctx, value.ID, now, now.Add(quotaProbeLease))
				if err != nil || !claimed {
					lease.Release()
					if err != nil {
						return nil, err
					}
					return nil, fmt.Errorf("绑定的上游账号恢复探测已被占用")
				}
				lease.QuotaProbe = true
				lease.QuotaProbeKind = recovery.Kind
				lease.Billing = candidate.Billing
				return lease, nil
			}
			if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
				return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted}
			}
			if candidate.QuotaWindow != nil && candidate.QuotaWindow.Remaining <= 0 {
				var retryAfter time.Duration
				if candidate.QuotaWindow.ResetAt != nil {
					retryAfter = retryDelay(now, *candidate.QuotaWindow.ResetAt)
				}
				return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
			}
		}
		lease, err := s.acquirePinnedCapacity(ctx, value)
		if err != nil {
			return nil, err
		}
		lease.Billing = candidate.Billing
		lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
		return lease, nil
	}
	return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
}

func effectiveQuotaMode(candidate account.RoutingCandidate, fallback string) string {
	if candidate.QuotaWindow != nil && candidate.QuotaWindow.Mode == "weekly" {
		return "weekly"
	}
	return fallback
}

func (s *Selector) MarkSuccess(ctx context.Context, credential account.Credential) {
	s.markSuccess(ctx, credential, true)
}

func (s *Selector) markSuccess(ctx context.Context, credential account.Credential, quotaProbe bool) {
	now := time.Now().UTC()
	persist := credential.FailureCount > 0 || credential.CooldownUntil != nil || credential.LastError != ""
	s.selectionMu.Lock()
	if last := s.lastSuccessAt[credential.ID]; last.IsZero() || now.Sub(last) >= successPersistInterval {
		persist = true
	}
	if persist {
		s.lastSuccessAt[credential.ID] = now
	}
	s.selectionMu.Unlock()
	// Grow the ready-ring working set so future traffic prefers proven accounts.
	s.rememberProvenAccount(credential.ID)
	if persist {
		_ = s.accounts.UpdateHealth(ctx, credential.ID, 0, nil, "", true)
	}
	if quotaProbe {
		_ = s.accounts.ClearQuotaRecovery(ctx, credential.ID)
	}
	if quotaProbe || credential.FailureCount > 0 || credential.CooldownUntil != nil || credential.LastError != "" {
		s.invalidateCandidates(credential.Provider)
	}
}

func (s *Selector) MarkFreeQuotaExhausted(ctx context.Context, credential account.Credential, used, limit int64, hints quotaRecoveryHints) {
	now := time.Now().UTC()
	if hints.Fallback <= 0 {
		hints.Fallback = defaultFreeQuotaRecoveryPause
	}
	nextProbeAt := s.resolveQuotaRecoveryAt(ctx, credential.ID, now, hints)
	s.markFreeQuotaExhaustedAt(ctx, credential, used, limit, now, nextProbeAt)
}

func (s *Selector) markFreeQuotaExhaustedAt(ctx context.Context, credential account.Credential, used, limit int64, now, nextProbeAt time.Time) {
	// Align with cpa-xai-quota-guard soft cooldown: anchor on first exhaust time
	// (ExhaustedAt + 24h), never push recover further out on every re-hit/patrol.
	exhaustedAt := now
	if existing, err := s.accounts.GetQuotaRecovery(ctx, credential.ID); err == nil &&
		existing.Kind == account.QuotaRecoveryKindFree && existing.ExhaustedAt != nil && !existing.ExhaustedAt.IsZero() {
		exhaustedAt = existing.ExhaustedAt.UTC()
		capAt := exhaustedAt.Add(defaultFreeQuotaRecoveryPause)
		if nextProbeAt.After(capAt) {
			nextProbeAt = capAt
		}
		if existing.NextProbeAt != nil && !existing.NextProbeAt.Before(now) && existing.NextProbeAt.Before(nextProbeAt) {
			nextProbeAt = existing.NextProbeAt.UTC()
		}
		if nextProbeAt.After(capAt) {
			nextProbeAt = capAt
		}
	} else {
		// First soft exhaust: default probe = exhaust + 24h when no precise reset.
		if nextProbeAt.IsZero() || nextProbeAt.Before(now) || nextProbeAt.After(now.Add(defaultFreeQuotaRecoveryPause)) {
			nextProbeAt = now.Add(defaultFreeQuotaRecoveryPause)
		}
	}
	_ = s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: credential.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted,
		ConfirmedUsed: used, ConfirmedLimit: limit, ExhaustedAt: &exhaustedAt,
		NextProbeAt: &nextProbeAt, LastConfirmedAt: &now, UpdatedAt: now,
	})
	// Drop from hot set while cooling; MarkSuccess after recovery re-admits it.
	s.forgetProvenAccount(credential.ID)
	_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	s.invalidateCandidates(credential.Provider)
}

func (s *Selector) MarkModelQuotaExhausted(ctx context.Context, credential account.Credential, upstreamModel string, retryAfter time.Duration) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		s.MarkFreeQuotaExhausted(ctx, credential, 0, 0, quotaRecoveryHints{})
		return
	}
	if retryAfter <= 0 {
		retryAfter = defaultFreeQuotaRecoveryPause
	}
	until := time.Now().UTC().Add(retryAfter)
	_ = s.accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: upstreamModel, Reason: "model_quota_depleted", CooldownUntil: until, UpdatedAt: time.Now().UTC(),
	})
	s.invalidateCandidates(credential.Provider)
}

// MarkModelAccessDenied isolates a permission failure to the rejected model.
// Build OAuth accounts may still have valid video access when a chat endpoint
// returns 403, so a model denial must not invalidate the whole credential.
func (s *Selector) MarkModelAccessDenied(ctx context.Context, credential account.Credential, upstreamModel string, retryAfter time.Duration) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return
	}
	if retryAfter <= 0 {
		retryAfter = modelAccessDeniedCooldown
	}
	now := time.Now().UTC()
	_ = s.accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: upstreamModel, Reason: "model_access_denied",
		CooldownUntil: now.Add(retryAfter), UpdatedAt: now,
	})
	s.invalidateCandidates(credential.Provider)
}

// MarkPaymentQuotaExhausted 将 402/spending-limit 账号移出号池。付费账号按真实账期
// 进行 Billing 探测；Free/Unknown 依次采用上游 ResetAt、Retry-After、账期时间和 20h fallback。
// Prefer MarkSpendingLimitSoftCooldown for Build spending-limit after egress retries.
func (s *Selector) MarkPaymentQuotaExhausted(ctx context.Context, credential account.Credential, hints quotaRecoveryHints) {
	now := time.Now().UTC()
	if hints.Billing != nil && hints.Billing.IsPaid() {
		if periodEnd, ok := hints.Billing.PeriodEnd(); ok && periodEnd.After(now) {
			_ = s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
				AccountID: credential.ID, Kind: account.QuotaRecoveryKindPaid, Status: account.QuotaRecoveryStatusExhausted,
				ExhaustedAt: &now, NextProbeAt: &periodEnd, LastConfirmedAt: &now, UpdatedAt: now,
			})
			_ = s.sticky.DeleteByAccount(ctx, credential.ID)
			s.invalidateCandidates(credential.Provider)
			return
		}
	}
	hints.Fallback = paymentRequiredRecoveryPause
	s.MarkFreeQuotaExhausted(ctx, credential, 0, 0, hints)
}

// MarkSpendingLimitSoftCooldown parks an account after spending-limit 402 survived
// configurable Resin egress rotations. Uses kind=spending_limit so ops can filter
// separately from free/paid quota exhaustion.
func (s *Selector) MarkSpendingLimitSoftCooldown(ctx context.Context, credential account.Credential, pause time.Duration) {
	if pause <= 0 {
		pause = time.Hour
	}
	if pause > 72*time.Hour {
		pause = 72 * time.Hour
	}
	now := time.Now().UTC()
	nextProbeAt := now.Add(pause)
	// Soft: do not extend past first-exhaust + pause when re-hit during cooldown.
	if existing, err := s.accounts.GetQuotaRecovery(ctx, credential.ID); err == nil &&
		existing.Kind == account.QuotaRecoveryKindSpendingLimit && existing.ExhaustedAt != nil && !existing.ExhaustedAt.IsZero() {
		exhaustedAt := existing.ExhaustedAt.UTC()
		capAt := exhaustedAt.Add(pause)
		if existing.NextProbeAt != nil && !existing.NextProbeAt.Before(now) && existing.NextProbeAt.Before(nextProbeAt) {
			nextProbeAt = existing.NextProbeAt.UTC()
		}
		if nextProbeAt.After(capAt) {
			nextProbeAt = capAt
		}
		_ = s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
			AccountID: credential.ID, Kind: account.QuotaRecoveryKindSpendingLimit, Status: account.QuotaRecoveryStatusExhausted,
			ExhaustedAt: &exhaustedAt, NextProbeAt: &nextProbeAt, LastConfirmedAt: &now, UpdatedAt: now,
		})
	} else {
		_ = s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
			AccountID: credential.ID, Kind: account.QuotaRecoveryKindSpendingLimit, Status: account.QuotaRecoveryStatusExhausted,
			ExhaustedAt: &now, NextProbeAt: &nextProbeAt, LastConfirmedAt: &now, UpdatedAt: now,
		})
	}
	s.forgetProvenAccount(credential.ID)
	_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	s.invalidateCandidates(credential.Provider)
}

// AcquirePinnedEgressRetry re-leases one account for same-request Resin exit rotation.
// It skips quota-recovery gates so a prior free/spending_limit soft-cool cannot block
// the egress retry that is supposed to clear a hot IP.
func (s *Selector) AcquirePinnedEgressRetry(ctx context.Context, provider account.Provider, accountID uint64, upstreamModel, quotaMode string) (*accountLease, error) {
	now := time.Now().UTC()
	values, err := s.loadCandidates(ctx, provider, upstreamModel, quotaMode, now)
	if err != nil {
		return nil, err
	}
	for _, candidate := range values {
		value := candidate.Credential
		if value.ID != accountID {
			continue
		}
		if !value.Enabled || value.AuthStatus != account.AuthStatusActive {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
		lease, err := s.acquirePinnedCapacity(ctx, value)
		if err != nil {
			return nil, err
		}
		lease.Billing = candidate.Billing
		lease.QuotaMode = quotaMode
		return lease, nil
	}
	return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
}

func (s *Selector) resolveQuotaRecoveryAt(ctx context.Context, accountID uint64, now time.Time, hints quotaRecoveryHints) time.Time {
	// Precise sources first (same priority as qg: ResetAt / Retry-After / billing period).
	if mode := strings.TrimSpace(hints.QuotaMode); mode != "" {
		if windows, err := s.accounts.GetQuotaWindows(ctx, []uint64{accountID}); err == nil {
			var resetAt time.Time
			for _, window := range windows[accountID] {
				if window.Mode != mode || window.ResetAt == nil || !window.ResetAt.After(now) {
					continue
				}
				if resetAt.IsZero() || window.ResetAt.Before(resetAt) {
					resetAt = window.ResetAt.UTC()
				}
			}
			if !resetAt.IsZero() {
				return resetAt
			}
		}
	}
	if hints.RetryAfter > 0 {
		return now.Add(hints.RetryAfter)
	}
	if hints.Billing != nil {
		if periodEnd, ok := hints.Billing.PeriodEnd(); ok && periodEnd.After(now) {
			return periodEnd
		}
	}
	fallback := hints.Fallback
	if fallback <= 0 {
		fallback = defaultFreeQuotaRecoveryPause
	}
	// Soft estimate: if already exhausted, do not restart the 24h clock from "now"
	// (qg EffectiveRecoverAt / CoalesceRecoverAt soft path).
	if existing, err := s.accounts.GetQuotaRecovery(ctx, accountID); err == nil &&
		existing.Kind == account.QuotaRecoveryKindFree && existing.ExhaustedAt != nil && !existing.ExhaustedAt.IsZero() {
		capAt := existing.ExhaustedAt.UTC().Add(fallback)
		if existing.NextProbeAt != nil && !existing.NextProbeAt.Before(now) && !existing.NextProbeAt.After(capAt) {
			return existing.NextProbeAt.UTC()
		}
		return capAt
	}
	return now.Add(fallback)
}

// MarkQuotaStateChanged 在 Billing 探测改变持久化额度状态后立即失效候选快照。
func (s *Selector) MarkQuotaStateChanged(provider account.Provider) { s.invalidateCandidates(provider) }

// ConsumeQuota 将成功请求的本地额度变化应用到候选快照，避免为单账号变化清空整个 Provider 缓存。
func (s *Selector) ConsumeQuota(provider account.Provider, accountID uint64, mode string, amount int) {
	if accountID == 0 || mode == "" || mode == "weekly" || amount <= 0 {
		return
	}
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	for key, snapshot := range s.candidates {
		if key.provider != provider {
			continue
		}
		index, found := snapshot.byAccount[accountID]
		if !found || index >= len(snapshot.values) {
			continue
		}
		candidate := snapshot.values[index]
		if candidate.QuotaWindow == nil || candidate.QuotaWindow.Mode != mode {
			continue
		}
		next := append([]account.RoutingCandidate(nil), snapshot.values...)
		window := *next[index].QuotaWindow
		window.Remaining = max(0, window.Remaining-amount)
		window.UpdatedAt = time.Now().UTC()
		next[index].QuotaWindow = &window
		snapshot.values = next
		s.candidates[key] = snapshot
	}
	for key, snapshot := range s.routingBases {
		if key.provider != provider {
			continue
		}
		index := -1
		for candidateIndex, base := range snapshot.values {
			if base.Credential.ID == accountID {
				index = candidateIndex
				break
			}
		}
		if index < 0 || snapshot.values[index].QuotaWindow == nil || snapshot.values[index].QuotaWindow.Mode != mode {
			continue
		}
		next := append([]account.RoutingAccountBase(nil), snapshot.values...)
		window := *next[index].QuotaWindow
		window.Remaining = max(0, window.Remaining-amount)
		window.UpdatedAt = time.Now().UTC()
		next[index].QuotaWindow = &window
		snapshot.values = next
		s.routingBases[key] = snapshot
	}
}

func (s *Selector) MarkFailure(ctx context.Context, credential account.Credential, status int, retryAfter time.Duration) {
	failureCount := credential.FailureCount + 1
	_, cooldownBase, cooldownMax, _ := s.routingConfig()
	cooldown := cooldownBase
	for i := 1; i < failureCount && cooldown < cooldownMax; i++ {
		cooldown *= 2
	}
	if cooldown > cooldownMax {
		cooldown = cooldownMax
	}
	if retryAfter > cooldown {
		cooldown = retryAfter
	}
	until := time.Now().UTC().Add(cooldown)
	_ = s.accounts.UpdateHealth(ctx, credential.ID, failureCount, &until, fmt.Sprintf("upstream status %d", status), false)
	s.invalidateCandidates(credential.Provider)
	if status == 401 || status == 402 || status == 403 || status == 429 {
		_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	}
}

func (s *Selector) loadCandidates(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string, now time.Time) ([]account.RoutingCandidate, error) {
	if _, ok := s.accounts.(repository.RoutingLayerRepository); ok {
		return s.loadLayeredCandidates(ctx, provider, upstreamModel, quotaMode, now)
	}
	return s.loadCombinedCandidates(ctx, provider, upstreamModel, quotaMode, now)
}

// Prewarm loads routing candidates for a route into the process-local cache so the
// first user request after process start does not block on a multi-second SQLite scan.
func (s *Selector) Prewarm(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string) error {
	_, err := s.loadCandidates(ctx, provider, upstreamModel, quotaMode, time.Now().UTC())
	return err
}

func (s *Selector) loadCombinedCandidates(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string, now time.Time) ([]account.RoutingCandidate, error) {
	key := candidateCacheKey{provider: provider, upstreamModel: upstreamModel, quotaMode: quotaMode}
	s.candidateMu.Lock()
	if snapshot, ok := s.candidates[key]; ok && len(snapshot.values) > 0 {
		fresh := now.Before(snapshot.expiresAt)
		stale := snapshot.values
		s.candidateMu.Unlock()
		if fresh {
			return stale, nil
		}
		// Stale-while-revalidate: serve last snapshot immediately and refresh in background.
		s.scheduleCandidateRefresh(provider, upstreamModel, quotaMode, false)
		return stale, nil
	}
	s.candidateMu.Unlock()
	return s.refreshCombinedCandidates(ctx, provider, upstreamModel, quotaMode)
}

func (s *Selector) refreshCombinedCandidates(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string) ([]account.RoutingCandidate, error) {
	loadKey := string(provider) + "\x00" + upstreamModel + "\x00" + quotaMode
	loaded, err, _ := s.candidateLoads.Do(loadKey, func() (any, error) {
		checkTime := time.Now().UTC()
		s.candidateMu.Lock()
		if snapshot, ok := s.candidates[keyCombined(provider, upstreamModel, quotaMode)]; ok && checkTime.Before(snapshot.expiresAt) && len(snapshot.values) > 0 {
			s.candidateMu.Unlock()
			return snapshot.values, nil
		}
		s.candidateMu.Unlock()
		values, err := s.accounts.ListRoutingCandidates(ctx, provider, upstreamModel, quotaMode)
		if err != nil {
			return nil, err
		}
		s.candidateMu.Lock()
		s.candidates[keyCombined(provider, upstreamModel, quotaMode)] = newCandidateSnapshot(values, checkTime.Add(candidateCacheTTL))
		s.candidateMu.Unlock()
		return values, nil
	})
	if err != nil {
		return nil, err
	}
	return loaded.([]account.RoutingCandidate), nil
}

func keyCombined(provider account.Provider, upstreamModel, quotaMode string) candidateCacheKey {
	return candidateCacheKey{provider: provider, upstreamModel: upstreamModel, quotaMode: quotaMode}
}

func (s *Selector) scheduleCandidateRefresh(provider account.Provider, upstreamModel, quotaMode string, layered bool) {
	// Reuse the same singleflight keys as the blocking refresh paths so concurrent
	// SWR kicks and explicit reloads collapse into one SQLite scan.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if layered {
			_, _ = s.refreshLayeredCandidates(ctx, provider, upstreamModel, quotaMode)
			return
		}
		_, _ = s.refreshCombinedCandidates(ctx, provider, upstreamModel, quotaMode)
	}()
}

func (s *Selector) loadLayeredCandidates(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string, now time.Time) ([]account.RoutingCandidate, error) {
	key := candidateCacheKey{provider: provider, upstreamModel: upstreamModel, quotaMode: quotaMode}
	s.candidateMu.Lock()
	if snapshot, ok := s.candidates[key]; ok && len(snapshot.values) > 0 {
		fresh := now.Before(snapshot.expiresAt)
		stale := snapshot.values
		s.candidateMu.Unlock()
		if fresh {
			return stale, nil
		}
		s.scheduleCandidateRefresh(provider, upstreamModel, quotaMode, true)
		return stale, nil
	}
	s.candidateMu.Unlock()
	return s.refreshLayeredCandidates(ctx, provider, upstreamModel, quotaMode)
}

func (s *Selector) refreshLayeredCandidates(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string) ([]account.RoutingCandidate, error) {
	loadKey := "assembled\x00" + string(provider) + "\x00" + upstreamModel + "\x00" + quotaMode
	loaded, err, _ := s.candidateLoads.Do(loadKey, func() (any, error) {
		checkTime := time.Now().UTC()
		key := candidateCacheKey{provider: provider, upstreamModel: upstreamModel, quotaMode: quotaMode}
		s.candidateMu.Lock()
		if snapshot, ok := s.candidates[key]; ok && checkTime.Before(snapshot.expiresAt) && len(snapshot.values) > 0 {
			s.candidateMu.Unlock()
			return snapshot.values, nil
		}
		s.candidateMu.Unlock()
		layered := s.accounts.(repository.RoutingLayerRepository)
		for attempt := 0; attempt < 4; attempt++ {
			bases, baseVersion, loadErr := s.loadRoutingBases(ctx, layered, provider, quotaMode, checkTime)
			if loadErr != nil {
				return nil, loadErr
			}
			overlay, overlayVersion, loadErr := s.loadRoutingOverlay(ctx, layered, provider, upstreamModel, checkTime)
			if loadErr != nil {
				return nil, loadErr
			}
			if !s.routingVersionsStable(provider, baseVersion, overlayVersion) {
				checkTime = time.Now().UTC()
				continue
			}
			values := assembleRoutingCandidates(provider, bases, overlay)
			s.candidateMu.Lock()
			// Always publish the assembled snapshot for SWR even if versions moved;
			// the next refresh will catch up. Blocking the hot path on churn is worse.
			s.candidates[key] = newCandidateSnapshot(values, checkTime.Add(candidateCacheTTL))
			stable := baseVersion == s.routingBaseVersionLocked(provider) && overlayVersion == s.routingOverlayVersionLocked(provider)
			s.candidateMu.Unlock()
			if stable {
				return values, nil
			}
			checkTime = time.Now().UTC()
		}
		// Sustained account synchronization must not turn cache churn into user-facing
		// failures. Fall back to the established authoritative combined query.
		values, err := s.accounts.ListRoutingCandidates(ctx, provider, upstreamModel, quotaMode)
		if err != nil {
			return nil, err
		}
		s.candidateMu.Lock()
		s.candidates[candidateCacheKey{provider: provider, upstreamModel: upstreamModel, quotaMode: quotaMode}] = newCandidateSnapshot(values, time.Now().UTC().Add(candidateCacheTTL))
		s.candidateMu.Unlock()
		return values, nil
	})
	if err != nil {
		return nil, err
	}
	return loaded.([]account.RoutingCandidate), nil
}

func (s *Selector) loadRoutingBases(ctx context.Context, layered repository.RoutingLayerRepository, provider account.Provider, quotaMode string, now time.Time) ([]account.RoutingAccountBase, routingLayerVersion, error) {
	key := routingBaseCacheKey{provider: provider, quotaMode: quotaMode}
	version := s.routingBaseVersion(provider)
	s.candidateMu.Lock()
	if snapshot, ok := s.routingBases[key]; ok && now.Before(snapshot.expiresAt) && snapshot.version == version {
		values := snapshot.values
		s.candidateMu.Unlock()
		return values, version, nil
	}
	s.candidateMu.Unlock()
	loadKey := "base\x00" + string(provider) + "\x00" + quotaMode
	loaded, err, _ := s.candidateLoads.Do(loadKey, func() (any, error) {
		checkTime := time.Now().UTC()
		checkVersion := s.routingBaseVersion(provider)
		s.candidateMu.Lock()
		if snapshot, ok := s.routingBases[key]; ok && checkTime.Before(snapshot.expiresAt) && snapshot.version == checkVersion {
			values := snapshot.values
			s.candidateMu.Unlock()
			return routingBaseLoadResult{values: values, version: checkVersion}, nil
		}
		s.candidateMu.Unlock()
		values, loadErr := layered.ListRoutingAccountBases(ctx, provider, quotaMode)
		if loadErr != nil {
			return nil, loadErr
		}
		s.candidateMu.Lock()
		currentVersion := s.routingBaseVersionLocked(provider)
		if currentVersion == checkVersion {
			s.routingBases[key] = routingBaseSnapshot{values: values, version: checkVersion, expiresAt: checkTime.Add(candidateCacheTTL)}
		}
		s.candidateMu.Unlock()
		return routingBaseLoadResult{values: values, version: checkVersion}, nil
	})
	if err != nil {
		return nil, routingLayerVersion{}, err
	}
	result := loaded.(routingBaseLoadResult)
	return result.values, result.version, nil
}

func (s *Selector) loadRoutingOverlay(ctx context.Context, layered repository.RoutingLayerRepository, provider account.Provider, upstreamModel string, now time.Time) (account.RoutingOverlaySnapshot, routingLayerVersion, error) {
	key := routingOverlayCacheKey{provider: provider, upstreamModel: upstreamModel}
	version := s.routingOverlayVersion(provider)
	s.candidateMu.Lock()
	if snapshot, ok := s.routingOverlays[key]; ok && now.Before(snapshot.expiresAt) && snapshot.version == version {
		value := snapshot.value
		s.candidateMu.Unlock()
		return value, version, nil
	}
	s.candidateMu.Unlock()
	loadKey := "overlay\x00" + string(provider) + "\x00" + upstreamModel
	loaded, err, _ := s.candidateLoads.Do(loadKey, func() (any, error) {
		checkTime := time.Now().UTC()
		checkVersion := s.routingOverlayVersion(provider)
		s.candidateMu.Lock()
		if snapshot, ok := s.routingOverlays[key]; ok && checkTime.Before(snapshot.expiresAt) && snapshot.version == checkVersion {
			value := snapshot.value
			s.candidateMu.Unlock()
			return routingOverlayLoadResult{value: value, version: checkVersion}, nil
		}
		s.candidateMu.Unlock()
		value, loadErr := layered.ListRoutingAccountOverlays(ctx, provider, upstreamModel)
		if loadErr != nil {
			return nil, loadErr
		}
		s.candidateMu.Lock()
		currentVersion := s.routingOverlayVersionLocked(provider)
		if currentVersion == checkVersion {
			s.routingOverlays[key] = routingOverlaySnapshot{value: value, version: checkVersion, expiresAt: checkTime.Add(candidateCacheTTL)}
		}
		s.candidateMu.Unlock()
		return routingOverlayLoadResult{value: value, version: checkVersion}, nil
	})
	if err != nil {
		return account.RoutingOverlaySnapshot{}, routingLayerVersion{}, err
	}
	result := loaded.(routingOverlayLoadResult)
	return result.value, result.version, nil
}

func (s *Selector) routingBaseVersion(provider account.Provider) routingLayerVersion {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	return s.routingBaseVersionLocked(provider)
}

func (s *Selector) routingBaseVersionLocked(provider account.Provider) routingLayerVersion {
	return routingLayerVersion{global: s.baseGlobalVersion, provider: s.baseProviderVersion[provider]}
}

func (s *Selector) routingOverlayVersion(provider account.Provider) routingLayerVersion {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	return s.routingOverlayVersionLocked(provider)
}

func (s *Selector) routingOverlayVersionLocked(provider account.Provider) routingLayerVersion {
	return routingLayerVersion{global: s.overlayGlobalVersion, provider: s.overlayProviderVersion[provider]}
}

func (s *Selector) routingVersionsStable(provider account.Provider, base, overlay routingLayerVersion) bool {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	return base == s.routingBaseVersionLocked(provider) && overlay == s.routingOverlayVersionLocked(provider)
}

// ApplyInvalidation advances local layer generations before any remote publish.
// Large pools use stale-while-revalidate: mark snapshots expired but keep the
// last good values so the hot path never blocks on a multi-second SQLite reload
// just because one account refreshed or cooled down.
func (s *Selector) ApplyInvalidation(event repository.InvalidationEvent) {
	if !event.Valid() {
		return
	}
	s.candidateMu.Lock()
	base := event.Layer() == repository.InvalidationLayerBase
	overlay := event.Layer() == repository.InvalidationLayerOverlay || event.Layer() == repository.InvalidationLayerRoute
	if base {
		if event.Provider == "" {
			s.baseGlobalVersion++
			expireRoutingBases(s.routingBases, "")
		} else {
			s.baseProviderVersion[event.Provider]++
			expireRoutingBases(s.routingBases, event.Provider)
		}
	}
	if overlay {
		if event.Provider == "" {
			s.overlayGlobalVersion++
			expireRoutingOverlays(s.routingOverlays, "")
		} else {
			s.overlayProviderVersion[event.Provider]++
			expireRoutingOverlays(s.routingOverlays, event.Provider)
		}
	}
	for key, snapshot := range s.candidates {
		if event.Provider == "" || key.provider == event.Provider {
			snapshot.expiresAt = time.Time{}
			s.candidates[key] = snapshot
		}
	}
	s.candidateMu.Unlock()
}

func expireRoutingBases(values map[routingBaseCacheKey]routingBaseSnapshot, provider account.Provider) {
	for key, snapshot := range values {
		if provider == "" || key.provider == provider {
			snapshot.expiresAt = time.Time{}
			values[key] = snapshot
		}
	}
}

func expireRoutingOverlays(values map[routingOverlayCacheKey]routingOverlaySnapshot, provider account.Provider) {
	for key, snapshot := range values {
		if provider == "" || key.provider == provider {
			snapshot.expiresAt = time.Time{}
			values[key] = snapshot
		}
	}
}

func clearRoutingBases(values map[routingBaseCacheKey]routingBaseSnapshot, provider account.Provider) {
	for key := range values {
		if provider == "" || key.provider == provider {
			delete(values, key)
		}
	}
}

func clearRoutingOverlays(values map[routingOverlayCacheKey]routingOverlaySnapshot, provider account.Provider) {
	for key := range values {
		if provider == "" || key.provider == provider {
			delete(values, key)
		}
	}
}

type routingBaseLoadResult struct {
	values  []account.RoutingAccountBase
	version routingLayerVersion
}

type routingOverlayLoadResult struct {
	value   account.RoutingOverlaySnapshot
	version routingLayerVersion
}

func assembleRoutingCandidates(provider account.Provider, bases []account.RoutingAccountBase, overlay account.RoutingOverlaySnapshot) []account.RoutingCandidate {
	byAccount := make(map[uint64]account.RoutingAccountOverlay, len(overlay.Values))
	for _, value := range overlay.Values {
		byAccount[value.AccountID] = value
	}
	sharedSuperBuildModel := false
	if provider == account.ProviderBuild && !overlay.HasBindings {
		for _, base := range bases {
			value, exists := byAccount[base.Credential.ID]
			if exists && value.SupportsModel && account.IsBuildSuper(base.Credential, base.Billing) {
				sharedSuperBuildModel = true
				break
			}
		}
	}
	result := make([]account.RoutingCandidate, 0, len(bases))
	for _, base := range bases {
		overlayValue := byAccount[base.Credential.ID]
		if overlay.HasBindings && !overlayValue.Bound {
			continue
		}
		known, supports := overlayValue.ModelCapabilityKnown, overlayValue.SupportsModel
		if overlay.HasBindings {
			known, supports = true, true
		} else if sharedSuperBuildModel && account.IsBuildSuper(base.Credential, base.Billing) {
			known, supports = true, true
		}
		result = append(result, account.RoutingCandidate{
			Credential: base.Credential, Billing: base.Billing, QuotaWindow: base.QuotaWindow, QuotaRecovery: base.QuotaRecovery,
			ModelQuotaBlock: overlayValue.ModelQuotaBlock, ModelCapabilityKnown: known, SupportsModel: supports,
		})
	}
	return result
}

func (s *Selector) invalidateCandidates(provider account.Provider) {
	s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: provider})
	s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountCapabilityChanged, Provider: provider})
}

func (s *Selector) claimAccountSlot(ctx context.Context, value account.Credential) (*accountLease, error) {
	limit := s.accountConcurrencyLimit(value)
	release, acquired, err := s.concurrency.Acquire(ctx, accountConcurrencyKey(value.ID), limit)
	if err != nil {
		return nil, fmt.Errorf("获取账号并发租约: %w", err)
	}
	if !acquired {
		return nil, nil
	}
	s.selectionMu.Lock()
	s.lastSelectedAt[value.ID] = time.Now().UTC()
	s.selectionMu.Unlock()
	return &accountLease{Credential: value, release: func() {
		release()
		s.announceLeaseReturn()
	}}, nil
}

func (s *Selector) acquirePinnedCapacity(ctx context.Context, value account.Credential) (*accountLease, error) {
	_, _, _, capacityWait := s.routingConfig()
	return s.acquirePinnedCapacityWithWait(ctx, value, capacityWait)
}

func (s *Selector) acquirePinnedCapacityWithWait(ctx context.Context, value account.Credential, wait time.Duration) (*accountLease, error) {
	deadline := time.Now().Add(wait)
	for {
		lease, err := s.claimAccountSlot(ctx, value)
		if err != nil || lease != nil {
			return lease, err
		}
		if wait <= 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, deadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

// RebindSticky forces the session affinity onto accountID. Used after a successful
// request that permanently left the previous account (quota exhaust failover) so the
// same prompt_cache_key warms cache on the new account for subsequent turns.
func (s *Selector) RebindSticky(ctx context.Context, affinityKey string, accountID uint64) error {
	keys := stickyKeysFromAffinity(affinityKey)
	if len(keys) == 0 || accountID == 0 {
		return nil
	}
	stickyTTL, _, _, _ := s.routingConfig()
	return s.stickySetAll(ctx, keys, accountID, time.Now().UTC().Add(stickyTTL))
}

// RefreshSticky preserves an existing binding when present; otherwise binds accountID.
// Dual soft affinity keys are all refreshed onto the same account.
func (s *Selector) RefreshSticky(ctx context.Context, affinityKey string, accountID uint64) error {
	keys := stickyKeysFromAffinity(affinityKey)
	if len(keys) == 0 || accountID == 0 {
		return nil
	}
	stickyTTL, _, _, _ := s.routingConfig()
	now := time.Now().UTC()
	_, err := s.stickyBindAll(ctx, keys, accountID, now, now.Add(stickyTTL))
	return err
}

func (s *Selector) leaseReturnNotice() <-chan struct{} {
	s.leaseWakeMu.Lock()
	defer s.leaseWakeMu.Unlock()
	if s.leaseWake == nil {
		s.leaseWake = make(chan struct{})
	}
	return s.leaseWake
}

func (s *Selector) announceLeaseReturn() {
	s.leaseWakeMu.Lock()
	if s.leaseWake != nil {
		close(s.leaseWake)
	}
	s.leaseWake = make(chan struct{})
	s.leaseWakeMu.Unlock()
}

// awaitLeaseRetry 在本实例归还租约时立即重试；短轮询用于感知其他实例释放的共享并发名额。
func (s *Selector) awaitLeaseRetry(ctx context.Context, deadline time.Time) (bool, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false, nil
	}
	notice := s.leaseReturnNotice()
	timer := time.NewTimer(min(remaining, 100*time.Millisecond))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-notice:
		return true, nil
	case <-timer.C:
		return time.Now().Before(deadline), nil
	}
}

func earlierFuture(current, candidate, now time.Time) time.Time {
	if candidate.IsZero() || !now.Before(candidate) {
		return current
	}
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func retryDelay(now, retryAt time.Time) time.Duration {
	if retryAt.IsZero() || !now.Before(retryAt) {
		return 0
	}
	return retryAt.Sub(now)
}

func (s *Selector) resolveTierOrder(provider account.Provider, upstreamModel string) []account.WebTier {
	if s.tierOrders == nil {
		return nil
	}
	return s.tierOrders.TierOrder(provider, upstreamModel)
}

func tierOrderRank(order []account.WebTier, tier account.WebTier) int {
	for index, value := range order {
		if value == tier {
			return index
		}
	}
	return len(order)
}
