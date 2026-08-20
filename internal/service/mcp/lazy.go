package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/mcpjungle/mcpjungle/internal/model"
	"github.com/mcpjungle/mcpjungle/pkg/version"
)

const (
	metaToolInvoke    = "mcpjungle__invoke"
	metaToolListTools = "mcpjungle__list_tools"
)

type lazyContextKey string

const lazyGroupContextKey lazyContextKey = "lazy_group"

// ToolGroupResolver is the subset of ToolGroupService used by lazy mode.
type ToolGroupResolver interface {
	ResolveEffectiveTools(name string) ([]string, error)
}

// WithLazyGroup binds a lazy request to an optional tool group.
func (m *MCPService) WithLazyGroup(ctx context.Context, group string) context.Context {
	return context.WithValue(ctx, lazyGroupContextKey, group)
}

func lazyGroupFromContext(ctx context.Context) string {
	group, _ := ctx.Value(lazyGroupContextKey).(string)
	return group
}

func newLazyMCPServer() *server.MCPServer {
	return server.NewMCPServer(
		"MCPJungle lazy MCP proxy",
		version.GetVersion(),
		server.WithToolCapabilities(true),
	)
}

// initializeLazyHandlers registers the fixed lazy tool set once at startup.
//
// The lazy proxy exposes exactly two meta-tools so an agent has a clear,
// two-step workflow:
//
//  1. Call `mcpjungle__list_tools` to discover every enabled tool across
//     every registered MCP server (no upstream connection is opened).
//     Pass an optional `server` argument to restrict the listing to a single
//     server.
//  2. Call `mcpjungle__invoke` with the chosen `server` and `tool` (the same
//     `server__tool` form returned by list_tools) to actually execute the
//     tool. This is the only operation that initializes an upstream server.
func (m *MCPService) initializeLazyHandlers() {
	m.lazyMcpProxyServer.AddTool(mcp.NewTool(
		metaToolListTools,
		mcp.WithDescription(
			"REQUIRED FIRST STEP. You MUST call `mcpjungle__list_tools` before calling "+
				"`mcpjungle__invoke`. This is the only way to find the exact server and tool names "+
				"available to you; do not guess them. "+
				"Returns every enabled tool across every registered MCP server, each with its "+
				"canonical `server__tool` name, description, and input schema. "+
				"Pass an optional `server` argument to restrict the listing to a single MCP server. "+
				"This call does NOT initialize any upstream server and is cheap to repeat.",
		),
		mcp.WithString("server",
			mcp.Description("Optional. Restrict the listing to a single MCP server name."),
		),
	), m.lazyListToolsHandler)
	m.lazyMcpProxyServer.AddTool(mcp.NewTool(
		metaToolInvoke,
		mcp.WithDescription(
			"Execute a tool on a registered MCP server. This is the ONLY operation that opens "+
				"a connection to an upstream server. "+
				"DO NOT call this tool until you have first called `mcpjungle__list_tools` to "+
				"discover the exact `server` and `tool` names. Never invent, abbreviate, or guess "+
				"server or tool names — the names returned by list_tools are the only valid values. "+
				"Use the canonical `server__tool` form from list_tools; pass the bare tool name "+
				"alongside its `server`.",
		),
		mcp.WithString("server", mcp.Required(),
			mcp.Description("MCP server name copied verbatim from a `mcpjungle__list_tools` result (the part before `__` in the canonical tool name). Must not be guessed."),
		),
		mcp.WithString("tool", mcp.Required(),
			mcp.Description("Tool name copied verbatim from a `mcpjungle__list_tools` result (the part after `__` in the canonical tool name). Must not be guessed."),
		),
		mcp.WithObject("args",
			mcp.Description("Arguments matching the upstream tool's input schema."),
			mcp.AdditionalProperties(true),
		),
	), m.lazyInvokeHandler)
}

// lazyListToolsHandler returns the enabled tools the caller can invoke.
//
// With no `server` argument it returns every enabled tool across every
// registered MCP server (the canonical "directory" call). With `server=<name>`
// it restricts the listing to that single server. The handler never opens a
// connection to any upstream MCP server, so it is cheap to call repeatedly.
func (m *MCPService) lazyListToolsHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serverName := req.GetString("server", "")

	allowed, err := m.lazyAllowedTools(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var tools []model.Tool
	switch {
	case serverName != "":
		if err := m.authorizeLazyServer(ctx, serverName); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		tools, err = m.ListToolsByServer(serverName)
		if err != nil {
			return mcp.NewToolResultErrorf("failed to list tools for %s: %v", serverName, err), nil
		}
	default:
		tools, err = m.ListTools()
		if err != nil {
			return mcp.NewToolResultErrorf("failed to list tools: %v", err), nil
		}
	}

	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if !tool.Enabled || !allowed[tool.Name] {
			continue
		}
		// When listing globally, also drop tools whose parent server is not
		// accessible in the current auth/group context.
		if serverName == "" {
			if _, _, ok := splitServerToolName(tool.Name); !ok {
				continue
			}
			parent, _, _ := splitServerToolName(tool.Name)
			if authorizeProxyServerAccess(ctx, parent) != nil {
				continue
			}
		}
		result = append(result, lazyToolMetadata(tool))
	}
	sort.Slice(result, func(i, j int) bool { return result[i]["name"].(string) < result[j]["name"].(string) })
	return lazyJSONResult(result)
}

func (m *MCPService) lazyInvokeHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serverName, toolName := req.GetString("server", ""), req.GetString("tool", "")
	if serverName == "" || toolName == "" {
		return mcp.NewToolResultError("`server` and `tool` are required"), nil
	}
	canonical := mergeServerToolNames(serverName, toolName)
	if err := m.authorizeLazyTool(ctx, serverName, canonical); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	tool, err := m.GetTool(canonical)
	if err != nil || !tool.Enabled {
		return mcp.NewToolResultErrorf("tool %s is unavailable", canonical), nil
	}
	args, _ := req.GetArguments()["args"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	result, err := m.InvokeTool(ctx, canonical, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if result.IsError {
		return mcp.NewToolResultError(fmt.Sprint(result.Content)), nil
	}
	return lazyJSONResult(result)
}

func (m *MCPService) lazyAllowedTools(ctx context.Context) (map[string]bool, error) {
	if group := lazyGroupFromContext(ctx); group != "" {
		if m.toolGroupService == nil {
			return nil, fmt.Errorf("tool groups are unavailable")
		}
		tools, err := m.toolGroupService.ResolveEffectiveTools(group)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve tool group %q: %w", group, err)
		}
		allowed := make(map[string]bool, len(tools))
		for _, tool := range tools {
			allowed[tool] = true
		}
		return allowed, nil
	}
	tools, err := m.ListTools()
	if err != nil {
		return nil, fmt.Errorf("failed to list MCP tools: %w", err)
	}
	allowed := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool.Enabled {
			allowed[tool.Name] = true
		}
	}
	return allowed, nil
}

func (m *MCPService) authorizeLazyServer(ctx context.Context, serverName string) error {
	serverModel, err := m.GetMcpServer(serverName)
	if err != nil || !serverModel.Enabled {
		return fmt.Errorf("MCP server %s is unavailable", serverName)
	}
	if err := authorizeProxyServerAccess(ctx, serverName); err != nil {
		return err
	}
	allowed, err := m.lazyAllowedTools(ctx)
	if err != nil {
		return err
	}
	for toolName := range allowed {
		parent, _, _ := splitServerToolName(toolName)
		if parent == serverName {
			return nil
		}
	}
	return fmt.Errorf("MCP server %s is unavailable in this tool group", serverName)
}

func (m *MCPService) authorizeLazyTool(ctx context.Context, serverName, canonicalTool string) error {
	if err := m.authorizeLazyServer(ctx, serverName); err != nil {
		return err
	}
	allowed, err := m.lazyAllowedTools(ctx)
	if err != nil {
		return err
	}
	if !allowed[canonicalTool] {
		return fmt.Errorf("tool %s is unavailable in this tool group", canonicalTool)
	}
	return nil
}

func lazyToolMetadata(tool model.Tool) map[string]any {
	var schema any = map[string]any{}
	if len(tool.InputSchema) > 0 {
		_ = json.Unmarshal(tool.InputSchema, &schema)
	}
	return map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": schema}
}

func lazyJSONResult(value any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(data)), nil
}
