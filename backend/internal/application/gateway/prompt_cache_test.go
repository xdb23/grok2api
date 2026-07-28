package gateway

import (
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestResolveBuildSessionIdentityIsStableAndTenantIsolated(t *testing.T) {
	base := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1", nil)
	if len(base.upstreamID) != 36 || len(base.affinityKey) != 64 || len(base.replayKey) != 64 || base != resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1", nil) {
		t.Fatalf("unstable identity = %#v", base)
	}
	for name, value := range map[string]buildSessionIdentity{
		"client":   resolveBuildSessionIdentity(8, accountdomain.ProviderBuild, "grok-4.5", "", "session-1", nil),
		"provider": resolveBuildSessionIdentity(7, accountdomain.ProviderConsole, "grok-4.5", "", "session-1", nil),
		"session":  resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-2", nil),
	} {
		if value.upstreamID == base.upstreamID || value.affinityKey == base.affinityKey {
			t.Fatalf("%s was not isolated: %#v vs %#v", name, value, base)
		}
	}
}

func TestResolveBuildSessionIdentitySeparatesAffinityFromUpstreamSession(t *testing.T) {
	first := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1", nil)
	otherModel := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.3", "", "session-1", nil)
	if first.upstreamID == "" || first.upstreamID == otherModel.upstreamID || first.replayKey == otherModel.replayKey {
		t.Fatalf("model-specific upstream session was not isolated: first=%#v other=%#v", first, otherModel)
	}
	if first.affinityKey == otherModel.affinityKey {
		t.Fatalf("model-specific account affinity was not isolated: %#v", first)
	}
}

func TestResolveBuildSoftSessionIdentityIsModelScoped(t *testing.T) {
	body := []byte(`{"instructions":"stable system","input":[{"role":"user","content":"hello"}]}`)
	first := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", body)
	otherModel := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.3", "", "", body)
	if first.upstreamID == "" || first.affinityKey == "" || !first.soft || !otherModel.soft {
		t.Fatalf("soft identity missing: first=%#v other=%#v", first, otherModel)
	}
	if first.upstreamID == otherModel.upstreamID || first.affinityKey == otherModel.affinityKey {
		t.Fatalf("soft identity must be model scoped: first=%#v other=%#v", first, otherModel)
	}
}

func TestResolveBuildSessionIdentityPrefersSessionSignal(t *testing.T) {
	first := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "client-key", "session-1", nil)
	second := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "client-key", "session-2", nil)
	if first.upstreamID == "" || first == second {
		t.Fatalf("session signal did not take precedence: first=%#v second=%#v", first, second)
	}
	clientKeyOnly := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "client-key", "", nil)
	if clientKeyOnly.upstreamID == "" || clientKeyOnly == first {
		t.Fatalf("explicit key fallback was not isolated: session=%#v explicit=%#v", first, clientKeyOnly)
	}
	if value := resolveBuildSessionIdentity(0, accountdomain.ProviderBuild, "grok-4.5", "client-key", "", nil); value != (buildSessionIdentity{}) {
		t.Fatal("identity without client ownership should be empty")
	}
	if value := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", nil); value != (buildSessionIdentity{}) {
		t.Fatal("identity without an explicit key or session or body should be empty")
	}
}

func TestResolveBuildSessionIdentitySoftFromMessagesIsStableAcrossTurns(t *testing.T) {
	turn1 := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello world"}]}`)
	turn2 := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello world"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`)
	first := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", turn1)
	second := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", turn2)
	if !first.soft || first.upstreamID == "" {
		t.Fatalf("expected soft identity, got %#v", first)
	}
	if first.replayKey != "" {
		t.Fatalf("soft identity must not enable reasoning replay: %#v", first)
	}
	if first.upstreamID != second.upstreamID {
		t.Fatalf("soft upstream drifted across turns: first=%#v second=%#v", first, second)
	}
	// Turn2 includes assistant → dual affinity; fallback half must match turn1 affinity.
	if second.affinityKey == first.affinityKey {
		// ok if assistant not extracted
	} else if !strings.Contains(second.affinityKey, first.affinityKey) {
		t.Fatalf("turn2 dual affinity must inherit turn1 fallback: first=%#v second=%#v", first, second)
	}
	// Different first user messages must remain isolated.
	other := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", []byte(`{"messages":[{"role":"user","content":"different"}]}`))
	if other.upstreamID == first.upstreamID {
		t.Fatal("different first user shared soft upstream id")
	}
}

func TestResolveBuildSessionIdentitySoftFromResponsesInput(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"cache me"}]}]}`)
	first := resolveBuildSessionIdentity(9, accountdomain.ProviderBuild, "grok-4.5", "", "", body)
	second := resolveBuildSessionIdentity(9, accountdomain.ProviderBuild, "grok-4.5", "", "", body)
	if first.upstreamID == "" || first != second || !first.soft {
		t.Fatalf("responses soft identity unstable: %#v %#v", first, second)
	}
	if first.replayKey != "" {
		t.Fatalf("responses soft identity must not enable reasoning replay: %#v", first)
	}
}

func TestResolveBuildSessionIdentitySoftIgnoresVolatileSystem(t *testing.T) {
	// Coding agents rewrite instructions/system every turn; soft identity must stay on first user only.
	a := resolveBuildSessionIdentity(3, accountdomain.ProviderBuild, "grok-4.5", "", "", []byte(`{"instructions":"stable system","input":[{"role":"user","content":"hello"}]}`))
	b := resolveBuildSessionIdentity(3, accountdomain.ProviderBuild, "grok-4.5", "", "", []byte(`{"instructions":"stable system","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`))
	c := resolveBuildSessionIdentity(3, accountdomain.ProviderBuild, "grok-4.5", "", "", []byte(`{"instructions":"OTHER system with date and git status","input":[{"role":"user","content":"hello"}]}`))
	if !a.soft || a.upstreamID == "" || a.upstreamID != b.upstreamID {
		t.Fatalf("soft session unstable across turns: %#v %#v", a, b)
	}
	// b has assistant so affinity may be dual; fallback must include a.affinityKey
	if a.affinityKey != b.affinityKey && !strings.Contains(b.affinityKey, a.affinityKey) {
		t.Fatalf("soft affinity lost turn1 fallback: a=%#v b=%#v", a, b)
	}
	if a.upstreamID != c.upstreamID {
		t.Fatalf("volatile system must not change soft session: a=%#v c=%#v", a, c)
	}
	otherUser := resolveBuildSessionIdentity(3, accountdomain.ProviderBuild, "grok-4.5", "", "", []byte(`{"instructions":"stable system","input":[{"role":"user","content":"different start"}]}`))
	if otherUser.upstreamID == a.upstreamID {
		t.Fatal("different first user must isolate soft session")
	}
}

func TestResolveBuildSessionIdentitySoftNormalizesWhitespace(t *testing.T) {
	a := resolveBuildSessionIdentity(3, accountdomain.ProviderBuild, "grok-4.5", "", "", []byte(`{"messages":[{"role":"user","content":"hello   world\nnext"}]}`))
	b := resolveBuildSessionIdentity(3, accountdomain.ProviderBuild, "grok-4.5", "", "", []byte("{\"messages\":[{\"role\":\"user\",\"content\":\"hello world next\"}]}"))
	if !a.soft || a.upstreamID == "" || a.upstreamID != b.upstreamID {
		t.Fatalf("soft whitespace normalize failed: a=%#v b=%#v", a, b)
	}
}

func TestComposeStickyAffinityKeyPinsUpstreamSession(t *testing.T) {
	soft := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", []byte(`{"messages":[{"role":"user","content":"hello sticky"}]}`))
	if soft.upstreamID == "" || soft.affinityKey == "" {
		t.Fatalf("soft identity incomplete: %#v", soft)
	}
	composed := composeStickyAffinityKey(soft)
	if composed == "" {
		t.Fatal("composed sticky empty")
	}
	// Ownership-only identity (previous_response_id path) still gets sticky via upstream id.
	owned := buildSessionIdentity{upstreamID: soft.upstreamID}
	ownedSticky := composeStickyAffinityKey(owned)
	if ownedSticky == "" {
		t.Fatal("upstream-only identity must still produce sticky key")
	}
	softPrimary := strings.Split(composed, affinityKeySeparator)[0]
	ownedPrimary := strings.Split(ownedSticky, affinityKeySeparator)[0]
	if softPrimary != ownedPrimary {
		t.Fatalf("upstream sticky primary mismatch: soft=%q owned=%q", softPrimary, ownedPrimary)
	}
}

// CPA SessionAffinitySelector: same explicit session identity → same sticky keys across turns.
func TestCPAStyleExplicitPromptCacheKeyStickyKeysStable(t *testing.T) {
	turn1 := resolveBuildSessionIdentity(11, accountdomain.ProviderBuild, "grok-4.5", "client-pck-abc", "", nil)
	turn2 := resolveBuildSessionIdentity(11, accountdomain.ProviderBuild, "grok-4.5", "client-pck-abc", "", []byte(`{"input":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`))
	if turn1.upstreamID == "" || turn1.soft || turn1.upstreamID != turn2.upstreamID {
		t.Fatalf("explicit prompt_cache_key must pin upstream across turns: t1=%#v t2=%#v", turn1, turn2)
	}
	if turn1.affinityKey != turn2.affinityKey {
		t.Fatalf("explicit affinity must not drift: t1=%q t2=%q", turn1.affinityKey, turn2.affinityKey)
	}
	k1 := stickyKeysFromAffinity(composeStickyAffinityKey(turn1))
	k2 := stickyKeysFromAffinity(composeStickyAffinityKey(turn2))
	if len(k1) == 0 || len(k2) == 0 {
		t.Fatalf("sticky keys empty: k1=%v k2=%v", k1, k2)
	}
	// Primary key (first) must match so store hit works without dual-key inheritance.
	if k1[0] != k2[0] {
		t.Fatalf("primary sticky key drifted: %q vs %q", k1[0], k2[0])
	}
	// Different client pck → different keys (CPA different sessions).
	other := stickyKeysFromAffinity(composeStickyAffinityKey(resolveBuildSessionIdentity(11, accountdomain.ProviderBuild, "grok-4.5", "other-pck", "", nil)))
	if other[0] == k1[0] {
		t.Fatal("different prompt_cache_key must not share sticky primary")
	}
}

// CPA extractMessageHashIDs dual inheritance: turn1 short key, turn2 full+fallback share store binding via fallback part.
func TestCPAStyleSoftDualAffinityStickyKeyInheritance(t *testing.T) {
	turn1Body := []byte(`{"messages":[{"role":"user","content":"cpa soft root message"}]}`)
	turn2Body := []byte(`{"messages":[{"role":"user","content":"cpa soft root message"},{"role":"assistant","content":"ack"},{"role":"user","content":"follow up"}]}`)
	t1 := resolveBuildSessionIdentity(42, accountdomain.ProviderBuild, "grok-4.5", "", "", turn1Body)
	t2 := resolveBuildSessionIdentity(42, accountdomain.ProviderBuild, "grok-4.5", "", "", turn2Body)
	if !t1.soft || !t2.soft || t1.upstreamID != t2.upstreamID {
		t.Fatalf("soft multi-turn upstream unstable: t1=%#v t2=%#v", t1, t2)
	}
	// turn2 dual affinity must contain turn1 affinity as fallback (CPA short-hash inherit).
	if t1.affinityKey == "" || (!strings.Contains(t2.affinityKey, t1.affinityKey) && t1.affinityKey != t2.affinityKey) {
		t.Fatalf("turn2 must inherit turn1 soft affinity fallback: t1=%q t2=%q", t1.affinityKey, t2.affinityKey)
	}
	keys1 := stickyKeysFromAffinity(composeStickyAffinityKey(t1))
	keys2 := stickyKeysFromAffinity(composeStickyAffinityKey(t2))
	// Intersection non-empty: stickyGetAny on turn2 finds turn1 binding.
	set := make(map[string]struct{}, len(keys1))
	for _, k := range keys1 {
		set[k] = struct{}{}
	}
	shared := false
	for _, k := range keys2 {
		if _, ok := set[k]; ok {
			shared = true
			break
		}
	}
	if !shared {
		t.Fatalf("no shared sticky keys for CPA dual inherit: keys1=%v keys2=%v", keys1, keys2)
	}
}

func TestStickyKeysFromAffinityIncludesRawPinAndHashedParts(t *testing.T) {
	// Mirrors CPA cache key stability: aff: raw pin first, then hashed siblings.
	dual := "primarydigest" + affinityKeySeparator + "fallbackdigest"
	keys := stickyKeysFromAffinity(dual)
	if len(keys) < 2 {
		t.Fatalf("expected aff pin + parts, got %v", keys)
	}
	if keys[0] != "aff:"+dual {
		t.Fatalf("raw aff pin must be first: %q", keys[0])
	}
	if stickyKeysFromAffinity("") != nil {
		t.Fatal("empty affinity must yield no keys")
	}
}
