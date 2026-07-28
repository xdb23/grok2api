package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

const buildSessionIdentityVersion = "v3"

type buildSessionIdentity struct {
	// upstreamID is sent as prompt_cache_key and x-grok-conv-id and must remain stable across turns.
	upstreamID string
	// affinityKey controls account stickiness and is isolated by model to avoid cross-model collisions.
	// Soft multi-turn may encode primary+fallback (CPA dual-key inheritance) so turn-2+ can resolve
	// the turn-1 binding without rotating the upstream prompt_cache_key.
	affinityKey string
	// replayKey is derived only from explicit client session signals; soft anchors must not drive encrypted reasoning replay.
	replayKey string
	// soft indicates a fallback identity derived from message content when no explicit session is available.
	soft bool
}

// affinityKeySeparator joins primary+fallback soft affinity digests (must not appear in hex digests).
const affinityKeySeparator = "\x1e"

// resolveBuildSessionIdentity derives a stable Grok Build session identity:
// 1. Prefer explicit client session signals, isolated by client key, provider, and model.
// 2. Fall back to system/instructions and the first user message when no explicit signal exists.
// 3. Return an empty identity when no signal exists; never generate a random session ID per request.
func resolveBuildSessionIdentity(clientKeyID uint64, provider accountdomain.Provider, upstreamModel, explicitKey, sessionSeed string, body []byte) buildSessionIdentity {
	// Prefer Claude Code and Codex session signals extracted by the transport layer.
	// body.prompt_cache_key is only a fallback when no stronger header or session signal exists.
	seed := strings.TrimSpace(sessionSeed)
	if seed == "" {
		seed = strings.TrimSpace(explicitKey)
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if clientKeyID == 0 || provider == "" || model == "" {
		return buildSessionIdentity{}
	}
	if seed != "" {
		upstreamSource := fmt.Sprintf("grok2api:build-session:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, seed)
		affinitySource := fmt.Sprintf("grok2api:build-affinity:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, seed)
		replaySource := fmt.Sprintf("grok2api:build-replay:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, seed)
		return buildSessionIdentity{
			upstreamID:  digestUUID(upstreamSource),
			affinityKey: hexDigest(affinitySource),
			replayKey:   hexDigest(replaySource),
		}
	}
	// Soft fallback when the client sends no session header / prompt_cache_key.
	// IMPORTANT: do NOT hash system/instructions — coding agents rewrite system every turn
	// (cwd, git, date, open files). Including system made soft keys thrash, so existing
	// multi-turn chats never warmed xAI prompt cache while brand-new chats looked fine.
	// CPA avoids this by always using an explicit Claude/Codex session id.
	// Soft upstream key is anchored only on the first user message (stable across turns).
	// Soft affinity uses CPA-style dual keys: fallback=firstUser, primary=firstUser+firstAssistant
	// when assistant exists, so turn-2+ inherits the turn-1 account binding without rotating
	// the upstream prompt_cache_key (which would cold-start xAI cache).
	// Whitespace is normalized so trivial formatting drift does not split the cache key.
	_, firstUser, firstAssistant := extractMessageAnchors(body)
	firstUser = normalizeSoftUserAnchor(firstUser, 200)
	firstAssistant = normalizeSoftUserAnchor(firstAssistant, 120)
	if firstUser == "" {
		return buildSessionIdentity{}
	}
	const softAnchorVersion = "v6-first-user-dual-aff"
	upstreamSource := fmt.Sprintf("grok2api:build-soft-session:%s:%s:%d:%s:%s:%s", buildSessionIdentityVersion, softAnchorVersion, clientKeyID, provider, model, firstUser)
	fallbackAffinity := hexDigest(fmt.Sprintf("grok2api:build-soft-affinity:%s:%s:%d:%s:%s:%s", buildSessionIdentityVersion, softAnchorVersion, clientKeyID, provider, model, firstUser))
	affinityKey := fallbackAffinity
	if firstAssistant != "" {
		primaryAffinity := hexDigest(fmt.Sprintf("grok2api:build-soft-affinity:%s:%s:%d:%s:%s:%s|a:%s", buildSessionIdentityVersion, softAnchorVersion, clientKeyID, provider, model, firstUser, firstAssistant))
		affinityKey = primaryAffinity + affinityKeySeparator + fallbackAffinity
	}
	return buildSessionIdentity{
		upstreamID:  digestUUID(upstreamSource),
		affinityKey: affinityKey,
		soft:        true,
	}
}

// composeStickyAffinityKey builds sticky store keys that always track the upstream
// prompt_cache_key (x-grok-conv-id). Soft dual-affinity digests alone can miss across
// turns; pinning by upstreamID matches what xAI uses for cache and what sub2api multi-turn
// needs when no explicit session header is forwarded.
func composeStickyAffinityKey(identity buildSessionIdentity) string {
	parts := make([]string, 0, 4)
	if id := strings.TrimSpace(identity.upstreamID); id != "" {
		// Stable primary: same key the CLI adapter injects as prompt_cache_key.
		parts = append(parts, hexDigest("grok2api:build-sticky-upstream:v1:"+id))
	}
	if identity.affinityKey != "" {
		for _, part := range strings.Split(identity.affinityKey, affinityKeySeparator) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	// Dedup while preserving order (upstream first).
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return strings.Join(out, affinityKeySeparator)
}

// normalizeSoftUserAnchor stabilizes soft session keys across minor client formatting
// differences (CRLF vs LF, repeated spaces/tabs) without changing semantic content.
func normalizeSoftUserAnchor(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(value))
	prevSpace := false
	for _, r := range value {
		switch r {
		case ' ', '\t', '\n', '\r', '\u00a0':
			if prevSpace {
				continue
			}
			b.WriteByte(' ')
			prevSpace = true
		default:
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return truncateAnchor(b.String(), maxRunes)
}

func digestUUID(source string) string {
	digest := sha256.Sum256([]byte(source))
	hexID := hex.EncodeToString(digest[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexID[0:8], hexID[8:12], hexID[12:16], hexID[16:20], hexID[20:32])
}

func hexDigest(source string) string {
	digest := sha256.Sum256([]byte(source))
	return hex.EncodeToString(digest[:])
}

func truncateAnchor(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" || maxRunes <= 0 {
		return value
	}
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes])
}

// extractMessageAnchors extracts stable prefix anchors from Chat, Messages, and Responses request bodies.
// It uses only system, the first user message, and an optional first assistant message to avoid hash drift across turns.
func extractMessageAnchors(body []byte) (system, firstUser, firstAssistant string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return "", "", ""
	}
	// Top-level system or instructions fields provide a stable system anchor for OpenAI Responses and Chat.
	if raw, ok := root["instructions"]; ok {
		system = flattenMessageContent(raw)
	}
	if system == "" {
		if raw, ok := root["system"]; ok {
			system = flattenMessageContent(raw)
		}
	}
	if raw, ok := root["messages"]; ok {
		msgSystem, msgUser, msgAssistant := anchorsFromRoleMessages(raw)
		if system == "" {
			system = msgSystem
		}
		firstUser, firstAssistant = msgUser, msgAssistant
		if firstUser != "" {
			return system, firstUser, firstAssistant
		}
	}
	if raw, ok := root["input"]; ok {
		inSystem, inUser, inAssistant := anchorsFromResponsesInput(raw)
		if system == "" {
			system = inSystem
		}
		if firstUser == "" {
			firstUser = inUser
		}
		if firstAssistant == "" {
			firstAssistant = inAssistant
		}
	}
	return system, firstUser, firstAssistant
}

func anchorsFromRoleMessages(raw json.RawMessage) (system, firstUser, firstAssistant string) {
	var messages []map[string]json.RawMessage
	if json.Unmarshal(raw, &messages) != nil {
		return "", "", ""
	}
	for _, msg := range messages {
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		content := flattenMessageContent(msg["content"])
		if content == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system":
			if system == "" {
				system = content
			}
		case "user":
			if firstUser == "" {
				firstUser = content
			}
		case "assistant":
			if firstAssistant == "" {
				firstAssistant = content
			}
		}
		if system != "" && firstUser != "" && firstAssistant != "" {
			break
		}
	}
	return system, firstUser, firstAssistant
}

func anchorsFromResponsesInput(raw json.RawMessage) (system, firstUser, firstAssistant string) {
	// Shorthand form: input is a direct string.
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return "", strings.TrimSpace(asString), ""
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return "", "", ""
	}
	for _, item := range items {
		var typeName, role string
		_ = json.Unmarshal(item["type"], &typeName)
		_ = json.Unmarshal(item["role"], &role)
		typeName = strings.TrimSpace(typeName)
		role = strings.ToLower(strings.TrimSpace(role))
		// Top-level instructions handle the system anchor; this branch extracts messages.
		if typeName != "" && typeName != "message" {
			continue
		}
		content := flattenMessageContent(item["content"])
		if content == "" {
			// Support content objects whose text field is a string.
			var text string
			if json.Unmarshal(item["text"], &text) == nil {
				content = strings.TrimSpace(text)
			}
		}
		if content == "" {
			continue
		}
		switch role {
		case "system", "developer":
			if system == "" {
				system = content
			}
		case "user":
			if firstUser == "" {
				firstUser = content
			}
		case "assistant":
			if firstAssistant == "" {
				firstAssistant = content
			}
		default:
			// Treat role-less plain-text input items as user input.
			if role == "" && firstUser == "" && (typeName == "" || typeName == "message") {
				firstUser = content
			}
		}
		if firstUser != "" && firstAssistant != "" {
			break
		}
	}
	// Use top-level instructions as a system fallback.
	return system, firstUser, firstAssistant
}

func flattenMessageContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return strings.TrimSpace(asString)
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		switch strings.TrimSpace(partType) {
		case "", "text", "input_text", "output_text":
			var text string
			if json.Unmarshal(part["text"], &text) == nil && strings.TrimSpace(text) != "" {
				if builder.Len() > 0 {
					builder.WriteByte('\n')
				}
				builder.WriteString(strings.TrimSpace(text))
			}
		}
	}
	return builder.String()
}
