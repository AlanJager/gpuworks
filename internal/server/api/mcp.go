package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/mcp"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// The MCP endpoint serves the assistant's read-only tools to an outside
// agent (Claude Code, Cursor, ...): the mcp package speaks the protocol, and
// the tools read the platform API as the caller - a platform token's user -
// redacted and shaped as the assistant's reads are. An administrator turns
// it on; it is off by default.

// mcpCallsPerUser is how many tool calls one user may have in flight: each
// is an API read, and an agent can fire many at once.
const mcpCallsPerUser = 4

// mcpInstructions is what an MCP client is told about this server.
const mcpInstructions = "gpuworks GPU cluster platform, read-only. Start with cluster_overview; find an endpoint with search_api, read it with call_api " +
	"(use jq to keep only what you need); find time-series metric names with search_metrics."

func (a *apiHandlers) mcpEnabled() bool {
	return a.d.Store != nil && a.d.Store.GetSetting(store.SettingMCPEnabled) == "true"
}

func (a *apiHandlers) mcp(w http.ResponseWriter, r *http.Request) {
	if !a.mcpEnabled() {
		writeProductError(w, http.StatusForbidden, ErrorMCPDisabled, "the MCP service is turned off", nil)
		return
	}
	body, err := readAll(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	reply := a.mcpServer(r).Handle(r.Context(), body)
	if reply == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(reply)
}

// mcpServer is the protocol server for one request: the built-in tools,
// run as the request's caller.
func (a *apiHandlers) mcpServer(r *http.Request) mcp.Server {
	builtin := aihelper.BuiltinTools()
	tools := make([]mcp.Tool, 0, len(builtin))
	for _, t := range builtin {
		tools = append(tools, mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	claims, _ := a.claimsFromRequest(r)
	prefix := ""
	if t := requestToken(r); t != nil {
		prefix = t.Prefix
	}
	return mcp.Server{
		Name: "gpuworks", Version: version.Version, Instructions: mcpInstructions, Tools: tools,
		Call: func(ctx context.Context, name string, args json.RawMessage) (string, bool) {
			tool, _ := aihelper.LookupTool(name)
			if a.d.AI == nil {
				return "the assistant's tools are not available on this server", true
			}
			if !a.mcpCalls.acquire(claims.Username) {
				return "too many tool calls in flight; wait for one to finish", true
			}
			defer a.mcpCalls.release(claims.Username)
			start := time.Now().UTC()
			result, errText := tool.Run(ctx, a.d.AI.ToolEnv(a.apiReaderFor(r)), args)
			if a.d.Log != nil {
				a.d.Log.Info("mcp tool call", "user", claims.Username, "token", prefix, "tool", name, "ms", time.Since(start).Milliseconds(), "error", errText)
			}
			return result, errText != ""
		},
	}
}

// mcpStatus says whether MCP serves and how to reach it.
func (a *apiHandlers) mcpStatus(w http.ResponseWriter, r *http.Request) {
	tools := aihelper.BuiltinTools()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	writeJSON(w, http.StatusOK, dto.MCPStatus{Enabled: a.mcpEnabled(), Path: "/api/v1/mcp", ProtocolVersion: mcp.ProtocolVersions[0], Tools: names})
}

// putMCPSettings turns MCP on or off (administrators only).
func (a *apiHandlers) putMCPSettings(w http.ResponseWriter, r *http.Request) {
	var req dto.MCPSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	value := "false"
	if req.Enabled {
		value = "true"
	}
	if err := a.d.Store.SetSetting(store.SettingMCPEnabled, value); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.d.Log != nil {
		claims, _ := a.claimsFromRequest(r)
		a.d.Log.Info("mcp service switched", "user", claims.Username, "enabled", req.Enabled)
	}
	a.mcpStatus(w, r)
}
