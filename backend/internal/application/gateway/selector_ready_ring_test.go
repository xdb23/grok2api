package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestReadyRingSelectsWithoutConcurrencySnapshot(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	selector := newSegmentedActiveTestSelector(200, limiter, nil)
	selector.UpdateReadyRing(true, 64, 16)
	selector.UpdateSegmentedSelector(false, 3000, 64)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.selectorObservation == nil || !strings.HasPrefix(lease.selectorObservation.stage, "ready_ring") {
		t.Fatalf("observation = %#v, want ready_ring*", lease.selectorObservation)
	}
	if sizes := limiter.BatchSizes(); len(sizes) != 0 {
		t.Fatalf("ready ring must not call CurrentMany, batch sizes = %v", sizes)
	}
}

func TestReadyRingRoundRobinsAcrossRequests(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	selector := newSegmentedActiveTestSelector(128, limiter, nil)
	selector.UpdateReadyRing(true, 64, 8)
	selector.UpdateSegmentedSelector(false, 3000, 64)

	seen := make(map[uint64]int)
	for range 4 {
		lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		seen[lease.Credential.ID]++
		lease.Release()
	}
	if len(seen) < 4 {
		t.Fatalf("expected distinct RR picks across windows, got %v", seen)
	}
}

func TestReadyRingFallsBackWhenWindowSaturated(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	for id := uint64(1); id <= 8; id++ {
		limiter.SetSaturated(id, true)
	}
	selector := newSegmentedActiveTestSelector(100, limiter, nil)
	selector.UpdateReadyRing(true, 64, 8)
	selector.UpdateSegmentedSelector(false, 3000, 64)
	selector.concurrencySnapshots = resultcache.New[[32]byte, map[string]int](maxConcurrencySnapshots, time.Nanosecond)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID <= 8 {
		t.Fatalf("selected saturated window account %d", lease.Credential.ID)
	}
}

func TestReadyRingDisabledBelowMinCandidates(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	selector := newSegmentedActiveTestSelector(32, limiter, nil)
	selector.UpdateReadyRing(true, 64, 8)
	selector.UpdateSegmentedSelector(false, 3000, 64)
	selector.concurrencySnapshots = resultcache.New[[32]byte, map[string]int](maxConcurrencySnapshots, time.Nanosecond)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.selectorObservation != nil && strings.HasPrefix(lease.selectorObservation.stage, "ready_ring") {
		t.Fatal("ready ring should not activate below minCandidates")
	}
}

func TestReadyRingPrefersProvenOverVirgin(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	selector := newSegmentedActiveTestSelector(200, limiter, nil)
	selector.UpdateReadyRing(true, 64, 32)
	selector.UpdateSegmentedSelector(false, 3000, 64)
	for id := uint64(50); id <= 60; id++ {
		selector.rememberProvenAccount(id)
	}
	seen := make(map[uint64]int)
	for range 20 {
		lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		seen[lease.Credential.ID]++
		if lease.selectorObservation == nil || lease.selectorObservation.stage != "ready_ring_proven" {
			t.Fatalf("stage=%v id=%d want ready_ring_proven", lease.selectorObservation, lease.Credential.ID)
		}
		if lease.Credential.ID < 50 || lease.Credential.ID > 60 {
			t.Fatalf("selected virgin id=%d while proven set available", lease.Credential.ID)
		}
		lease.Release()
	}
	if len(seen) < 5 {
		t.Fatalf("expected rotation within proven set, got %v", seen)
	}
}

func TestReadyRingFallsBackToVirginWhenProvenSaturated(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	selector := newSegmentedActiveTestSelector(100, limiter, nil)
	selector.UpdateReadyRing(true, 64, 16)
	selector.UpdateSegmentedSelector(false, 3000, 64)
	for id := uint64(1); id <= 10; id++ {
		selector.rememberProvenAccount(id)
		limiter.SetSaturated(id, true)
	}
	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID <= 10 {
		t.Fatalf("expected virgin fallback, got proven id=%d", lease.Credential.ID)
	}
	if lease.selectorObservation == nil || lease.selectorObservation.stage != "ready_ring_virgin" {
		t.Fatalf("stage=%v want ready_ring_virgin", lease.selectorObservation)
	}
}

func TestReadyRingForgetProvenOnFreeExhaust(t *testing.T) {
	selector := newSegmentedActiveTestSelector(50, newSegmentedSelectiveLimiter(), nil)
	selector.rememberProvenAccount(7)
	if !selector.isProvenAccount(7) {
		t.Fatal("expected proven")
	}
	selector.forgetProvenAccount(7)
	if selector.isProvenAccount(7) {
		t.Fatal("expected forgotten after free exhaust")
	}
}

func TestReadyRingConcurrentAcquireDistinctSlots(t *testing.T) {
	const accounts = 2000
	const workers = 200
	limiter := memory.NewConcurrencyLimiter()
	selector := newSegmentedActiveTestSelector(accounts, limiter, nil)
	bases := make([]account.RoutingAccountBase, accounts)
	for index := range bases {
		bases[index] = account.RoutingAccountBase{Credential: account.Credential{
			ID: uint64(index + 1), Provider: account.ProviderBuild, AuthStatus: account.AuthStatusActive,
			Enabled: true, Priority: 10, MaxConcurrent: 1,
		}}
	}
	selector.accounts = &layeredAccountRepository{bases: bases, overlays: map[string]account.RoutingOverlaySnapshot{"model": {}}}
	selector.UpdateReadyRing(true, 64, 64)
	selector.UpdateSegmentedSelector(false, 3000, 64)

	var ok atomic.Int64
	var wg sync.WaitGroup
	leases := make([]*accountLease, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(index int) {
			defer wg.Done()
			lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
			if err != nil {
				t.Errorf("worker %d: %v", index, err)
				return
			}
			leases[index] = lease
			ok.Add(1)
		}(i)
	}
	wg.Wait()
	if got := ok.Load(); got != workers {
		t.Fatalf("acquired %d/%d leases", got, workers)
	}
	ids := make(map[uint64]int, workers)
	for _, lease := range leases {
		if lease == nil {
			continue
		}
		ids[lease.Credential.ID]++
		lease.Release()
	}
	for id, count := range ids {
		if count != 1 {
			t.Fatalf("account %d claimed %d times under maxConcurrent=1", id, count)
		}
	}
}

func BenchmarkReadyRingAcquire(b *testing.B) {
	for _, candidateCount := range []int{3000, 10000, 30000} {
		b.Run(fmt.Sprintf("%d", candidateCount), func(b *testing.B) {
			limiter := memory.NewConcurrencyLimiter()
			selector := newSegmentedActiveTestSelector(candidateCount, limiter, nil)
			selector.UpdateReadyRing(true, 64, 64)
			selector.UpdateSegmentedSelector(false, 3000, 64)
			// Seed a modest working set like production after warm traffic.
			for id := uint64(1); id <= 500; id++ {
				selector.rememberProvenAccount(id)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "benchmark-model", "", "", nil, false)
				if err != nil {
					b.Fatal(err)
				}
				lease.Release()
			}
		})
	}
}

var _ repository.ConcurrencyLimiter = (*segmentedSelectiveLimiter)(nil)
