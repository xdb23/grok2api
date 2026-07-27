package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// buildPromptCacheRoute records internal tools added to route this request through the cache-capable path.
// injectedToolTypes restores the client's original visible tool list during response processing.
type buildPromptCacheRoute struct {
	filterXSearch       bool
	injectedToolTypes   map[string]struct{}
	clientDeclaredTools map[string]struct{}
}

func prepareBuildPromptCacheRoute(body []byte, operation, model, promptCacheKey string, allowClientTools bool) ([]byte, buildPromptCacheRoute, error) {
	route := buildPromptCacheRoute{
		injectedToolTypes:   make(map[string]struct{}),
		clientDeclaredTools: make(map[string]struct{}),
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, route, fmt.Errorf("解析 Build prompt cache 请求: %w", err)
	}
	if payload == nil {
		payload = make(map[string]json.RawMessage)
	}
	tools, err := buildCacheRouteTools(payload)
	if err != nil {
		return nil, route, err
	}
	for _, rawTool := range tools {
		kind, name := buildCacheToolIdentity(rawTool)
		if kind == "function" || kind == "custom" {
			if name != "" {
				route.clientDeclaredTools[name] = struct{}{}
			}
		}
		if kind == "x_search" {
			route.filterXSearch = true
		}
	}

	// Hide upstream internal subcalls even when the client explicitly declares x_search.
	// Cache routing itself applies only to plain-text conversations with a stable cache session identity.
	// prompt_cache_key is account-scoped on xAI: sticky rebind after quota switch keeps the same key so
	// the new account rebuilds cache on subsequent turns (first post-switch turn is cold).
	if strings.TrimSpace(promptCacheKey) == "" || !isBuildCacheConversationOperation(operation) || isBuildCacheMediaModel(model) || hasBuildCacheToolType(tools, "image_generation") {
		return body, route, nil
	}

	if len(tools) == 0 {
		// Align with CPA: inject only native x_search for the Build free-tier cache path.
		// Dual web_search+x_search changed the tools prefix vs CPA and could miss shared
		// multi-turn cache when clients alternate tool-free / tool-bearing turns.
		tools = append(tools, json.RawMessage(`{"type":"x_search"}`))
		payload["tool_choice"] = mustJSON("none")
		route.injectedToolTypes["x_search"] = struct{}{}
		route.filterXSearch = true
	} else if !hasBuildCacheToolType(tools, "x_search") {
		// CPA always appends x_search when cache identity exists (including pure function tools).
		// filterXSearch hides internal subcalls. allowClientTools kept for call-site compatibility.
		_ = allowClientTools
		tools = append(tools, json.RawMessage(`{"type":"x_search"}`))
		route.injectedToolTypes["x_search"] = struct{}{}
		route.filterXSearch = true
	} else {
		// Already has x_search: filter internals. Only reshape when x_search is duplicated
		// or not already the sole trailing entry — avoid thrashing a stable tools prefix.
		route.filterXSearch = true
		xSearchCount := 0
		lastIsXSearch := false
		for index, rawTool := range tools {
			kind, _ := buildCacheToolIdentity(rawTool)
			if kind == "x_search" {
				xSearchCount++
				lastIsXSearch = index == len(tools)-1
			}
		}
		if xSearchCount != 1 || !lastIsXSearch {
			tools = stabilizeBuildCacheXSearchTrailing(tools)
		}
	}
	payload["tools"] = mustJSON(tools)
	if _, injected := route.injectedToolTypes["x_search"]; injected {
		updatedChoice, choiceErr := appendBuildCacheXSearchToAllowedTools(payload["tool_choice"])
		if choiceErr != nil {
			return nil, route, choiceErr
		}
		if len(updatedChoice) > 0 {
			payload["tool_choice"] = updatedChoice
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, route, fmt.Errorf("编码 Build prompt cache 请求: %w", err)
	}
	return encoded, route, nil
}

func appendBuildCacheXSearchToAllowedTools(raw json.RawMessage) (json.RawMessage, error) {
	if isEmptyJSON(raw) {
		return nil, nil
	}
	var choice map[string]json.RawMessage
	if json.Unmarshal(raw, &choice) != nil || choice == nil {
		return nil, nil
	}
	var choiceType string
	_ = json.Unmarshal(choice["type"], &choiceType)
	if strings.TrimSpace(choiceType) != "allowed_tools" {
		return nil, nil
	}
	var allowed []json.RawMessage
	if json.Unmarshal(choice["tools"], &allowed) != nil {
		return nil, nil
	}
	for _, item := range allowed {
		kind, _ := buildCacheToolIdentity(item)
		if kind == "x_search" {
			return nil, nil
		}
	}
	allowed = append(allowed, json.RawMessage(`{"type":"x_search"}`))
	choice["tools"] = mustJSON(allowed)
	return json.Marshal(choice)
}

func buildCacheRouteTools(payload map[string]json.RawMessage) ([]json.RawMessage, error) {
	raw, exists := payload["tools"]
	if !exists || isEmptyJSON(raw) {
		return nil, nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil, &responsesRequestError{Message: "tools 必须是数组", Param: "tools", Code: "invalid_parameter"}
	}
	return tools, nil
}

func buildCacheToolIdentity(raw json.RawMessage) (kind, name string) {
	var tool struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tool) != nil {
		return "", ""
	}
	return strings.TrimSpace(tool.Type), strings.TrimSpace(tool.Name)
}

func hasBuildCacheToolType(tools []json.RawMessage, kind string) bool {
	for _, rawTool := range tools {
		toolType, _ := buildCacheToolIdentity(rawTool)
		if toolType == kind {
			return true
		}
	}
	return false
}

// stabilizeBuildCacheXSearchTrailing collapses duplicate x_search entries and moves a single
// x_search to the end so multi-turn tool lists share a stable cache-route suffix.
func stabilizeBuildCacheXSearchTrailing(tools []json.RawMessage) []json.RawMessage {
	if len(tools) == 0 {
		return tools
	}
	stable := make([]json.RawMessage, 0, len(tools))
	var xSearch json.RawMessage
	for _, rawTool := range tools {
		kind, _ := buildCacheToolIdentity(rawTool)
		if kind == "x_search" {
			if len(xSearch) == 0 {
				xSearch = rawTool
			}
			continue
		}
		stable = append(stable, rawTool)
	}
	if len(xSearch) == 0 {
		return tools
	}
	return append(stable, xSearch)
}

func isBuildCacheConversationOperation(operation string) bool {
	switch strings.TrimSpace(operation) {
	case "", "responses", "chat", "messages":
		return true
	default:
		return false
	}
}

func isBuildCacheMediaModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(model, "image") || strings.Contains(model, "imagine") || strings.Contains(model, "video")
}
