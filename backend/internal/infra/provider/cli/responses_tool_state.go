package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxBuildToolAliasLength       = 128
	maxToolSearchDescriptionBytes = 16 << 10
)

type responsesToolKind uint8

const (
	responsesFunctionTool responsesToolKind = iota
	responsesCustomTool
	responsesToolSearch
	responsesApplyPatchTool
)

type responsesToolIdentity struct {
	Kind      responsesToolKind
	Namespace string
	Name      string
}

func (i responsesToolIdentity) key() string {
	return fmt.Sprintf("%d\x00%s\x00%s", i.Kind, i.Namespace, i.Name)
}

// responsesToolCompatibility 保存一次请求内的工具别名和响应恢复状态；实例不得跨请求复用。
type responsesToolCompatibility struct {
	aliases             map[string]responsesToolIdentity
	identityAliases     map[string]string
	visibleTools        []any
	deferredSurfaces    []string
	clientSearchTool    map[string]any
	clientSearchParam   string
	serverSearchEager   bool
	streamCalls         map[string]*responsesStreamCall
	// omittedCallIDs tracks function/custom tool calls dropped for empty name/call_id
	// (CPA-style) so matching *_output history items can be dropped too.
	omittedCallIDs      map[string]struct{}
	legacyLocalShell    bool
	nativeShell         bool
	webSearchDisabled   bool
	compactionRequested bool
	warnings            []string
	warningSet          map[string]struct{}
	changed             bool
}

// responsesRequestError 表示可直接映射为 OpenAI 错误结构的 Provider 请求错误。
type responsesRequestError struct {
	Message string
	Param   string
	Code    string
}

func (e *responsesRequestError) Error() string { return e.Message }

func newResponsesToolCompatibility() *responsesToolCompatibility {
	return &responsesToolCompatibility{
		aliases:         make(map[string]responsesToolIdentity),
		identityAliases: make(map[string]string),
		streamCalls:     make(map[string]*responsesStreamCall),
		omittedCallIDs:  make(map[string]struct{}),
		warningSet:      make(map[string]struct{}),
	}
}

// omitMalformedToolCallHistory drops function_call / custom_tool_call items that
// lack name or call_id, matching CLIProxyAPI normalizeXAIInputCustomToolCalls:
// bad history is skipped instead of failing the whole request with 400.
func (c *responsesToolCompatibility) omitMalformedToolCallHistory(item map[string]any) bool {
	if c == nil || item == nil {
		return false
	}
	name := strings.TrimSpace(stringField(item, "name"))
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if name != "" && callID != "" {
		return false
	}
	c.changed = true
	c.addWarning("empty_tool_call_history_omitted")
	if callID != "" {
		if c.omittedCallIDs == nil {
			c.omittedCallIDs = make(map[string]struct{})
		}
		c.omittedCallIDs[callID] = struct{}{}
	}
	return true
}

func (c *responsesToolCompatibility) omitDroppedToolCallOutput(item map[string]any) bool {
	if c == nil || item == nil || len(c.omittedCallIDs) == 0 {
		return false
	}
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if callID == "" {
		return false
	}
	if _, ok := c.omittedCallIDs[callID]; !ok {
		return false
	}
	c.changed = true
	c.addWarning("empty_tool_call_history_omitted")
	return true
}

func (c *responsesToolCompatibility) alias(identity responsesToolIdentity) string {
	key := identity.key()
	if alias, exists := c.identityAliases[key]; exists {
		return alias
	}
	base := identity.Name
	if identity.Kind == responsesToolSearch {
		base = "grok2api_tool_search"
	} else if identity.Kind == responsesApplyPatchTool {
		base = "grok2api_apply_patch"
	} else if identity.Namespace != "" {
		separator := "__"
		if strings.HasSuffix(identity.Namespace, separator) {
			separator = ""
		}
		base = identity.Namespace + separator + identity.Name
	}
	alias := truncateToolAlias(base, key)
	if existing, collision := c.aliases[alias]; collision && existing.key() != key {
		alias = hashedToolAlias(base, key)
	}
	c.aliases[alias] = identity
	c.identityAliases[key] = alias
	return alias
}

func truncateToolAlias(base, key string) string {
	if len(base) <= maxBuildToolAliasLength {
		return base
	}
	return hashedToolAlias(base, key)
}

func hashedToolAlias(base, key string) string {
	suffix := "__" + shortToolHash(key)
	limit := maxBuildToolAliasLength - len(suffix)
	if len(base) > limit {
		base = base[:limit]
	}
	return base + suffix
}

func shortToolHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:9]
}

func stringField(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}

func cloneJSONArray(values []any) []any {
	cloned := make([]any, len(values))
	for index, value := range values {
		cloned[index] = cloneJSONValue(value)
	}
	return cloned
}

func cloneJSONObject(value map[string]any) map[string]any {
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = cloneJSONValue(item)
	}
	return cloned
}

func cloneJSONValue(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned any
	if json.Unmarshal(data, &cloned) != nil {
		return value
	}
	return cloned
}
