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
	metaToolInvoke      = "mcpjungle__invoke"
	metaToolDescribe    = "mcpjungle__describe"
	metaToolListServers = "mcpjungle__list_servers"
	metaToolListTools   = "mcpjungle__list_tools"
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
func (m *MCPService) initializeLazyHandlers() {
	m.lazyMcpProxyServer.AddTool(mcp.NewTool(
		metaToolListServers,
		mcp.WithDescription("List enabled MCP servers available to this client without initializing upstream servers."),
	), m.lazyListServersHandler)
	m.lazyMcpProxyServer.AddTool(mcp.NewTool(
		metaToolListTools,
		mcp.WithDescription("List enabled tools and input schemas for one available MCP server without initializing it."),
		mcp.WithString("server", mcp.Required()),
	), m.lazyListToolsHandler)
	m.lazyMcpProxyServer.AddTool(mcp.NewTool(
		metaToolDescribe,
		mcp.WithDescription("Return an enabled MCP tool's schema without initializing its upstream server."),
		mcp.WithString("server", mcp.Required()),
		mcp.WithString("tool", mcp.Required()),
	), m.lazyDescribeHandler)
	m.lazyMcpProxyServer.AddTool(mcp.NewTool(
		metaToolInvoke,
		mcp.WithDescription("Invoke an enabled MCP tool. Only this operation initializes an upstream server."),
		mcp.WithString("server", mcp.Required()),
		mcp.WithString("tool", mcp.Required()),
		mcp.WithObject("args", mcp.AdditionalProperties(true)),
	), m.lazyInvokeHandler)
}

func (m *MCPService) lazyListServersHandler(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	servers, err := m.ListMcpServers()
	if err != nil {
		return mcp.NewToolResultErrorf("failed to list MCP servers: %v", err), nil
	}
	allowed, err := m.lazyAllowedTools(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	serverNames := make(map[string]bool)
	for name := range allowed {
		serverName, _, _ := splitServerToolName(name)
		serverNames[serverName] = true
	}
	result := make([]map[string]string, 0, len(servers))
	for _, serverModel := range servers {
		if !serverModel.Enabled || !serverNames[serverModel.Name] || authorizeProxyServerAccess(ctx, serverModel.Name) != nil {
			continue
		}
		result = append(result, map[string]string{"name": serverModel.Name, "description": serverModel.Description})
	}
	sort.Slice(result, func(i, j int) bool { return result[i]["name"] < result[j]["name"] })
	return lazyJSONResult(result)
}

func (m *MCPService) lazyListToolsHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	serverName := req.GetString("server", "")
	if serverName == "" {
		return mcp.NewToolResultError("`server` is required"), nil
	}
	if err := m.authorizeLazyServer(ctx, serverName); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	tools, err := m.ListToolsByServer(serverName)
	if err != nil {
		return mcp.NewToolResultErrorf("failed to list tools for %s: %v", serverName, err), nil
	}
	allowed, err := m.lazyAllowedTools(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool.Enabled && allowed[tool.Name] {
			result = append(result, lazyToolMetadata(tool))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i]["name"].(string) < result[j]["name"].(string) })
	return lazyJSONResult(result)
}

func (m *MCPService) lazyDescribeHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	return lazyJSONResult(lazyToolMetadata(*tool))
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
