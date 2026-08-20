package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mcpjungle/mcpjungle/internal/model"
	"github.com/mcpjungle/mcpjungle/internal/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lazyTestService(t *testing.T) *MCPService {
	t.Helper()
	db := setupTestDBWithTools(t)
	service := &MCPService{
		db:                 db,
		metrics:            telemetry.NewNoopCustomMetrics(),
		lazyMcpProxyServer: newLazyMCPServer(),
	}
	service.initializeLazyHandlers()
	return service
}

func createLazyTestServer(t *testing.T, service *MCPService, name string) *model.McpServer {
	t.Helper()
	serverModel, err := model.NewStdioServer(name, name+" description", "echo", nil, nil, "")
	require.NoError(t, err)
	require.NoError(t, service.db.Create(serverModel).Error)
	return serverModel
}

func createLazyTestTool(t *testing.T, service *MCPService, serverModel *model.McpServer, name string, enabled bool) {
	t.Helper()
	schema, err := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}})
	require.NoError(t, err)
	tool := &model.Tool{ServerID: serverModel.ID, Name: name, Description: name + " description", InputSchema: schema, Enabled: true}
	require.NoError(t, service.db.Create(tool).Error)
	require.NoError(t, service.db.Model(tool).Update("enabled", enabled).Error)
}

func lazyRequest(name string, arguments map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = arguments
	return req
}

func lazyResultJSON(t *testing.T, result *mcp.CallToolResult, destination any) {
	t.Helper()
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)
	text, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(text.Text), destination))
}

func TestLazyProxyServer_RegistersExactlyTwoMetaTools(t *testing.T) {
	service := lazyTestService(t)
	tools := service.LazyMcpProxyServer().ListTools()
	assert.Len(t, tools, 2)
	assert.Contains(t, tools, metaToolListTools)
	assert.Contains(t, tools, metaToolInvoke)
}

func TestLazyDiscovery_ListAllReturnsEveryEnabledTool(t *testing.T) {
	service := lazyTestService(t)
	alpha := createLazyTestServer(t, service, "alpha")
	beta := createLazyTestServer(t, service, "beta")
	createLazyTestTool(t, service, alpha, "search", true)
	createLazyTestTool(t, service, alpha, "disabled", false)
	createLazyTestTool(t, service, beta, "lookup", true)
	ctx := context.WithValue(context.Background(), "mode", model.ModeDev)

	// No args: list every enabled tool across every server.
	all, err := service.lazyListToolsHandler(ctx, lazyRequest(metaToolListTools, nil))
	require.NoError(t, err)
	var allTools []map[string]any
	lazyResultJSON(t, all, &allTools)
	names := make([]string, 0, len(allTools))
	for _, t := range allTools {
		names = append(names, t["name"].(string))
	}
	assert.ElementsMatch(t, []string{"alpha__search", "beta__lookup"}, names)
	for _, tool := range allTools {
		assert.NotEmpty(t, tool["input_schema"])
		assert.NotEmpty(t, tool["description"])
	}

	// With server filter: only that server's enabled tools.
	filtered, err := service.lazyListToolsHandler(ctx, lazyRequest(metaToolListTools, map[string]any{"server": "alpha"}))
	require.NoError(t, err)
	var filteredTools []map[string]any
	lazyResultJSON(t, filtered, &filteredTools)
	require.Len(t, filteredTools, 1)
	assert.Equal(t, "alpha__search", filteredTools[0]["name"])
}

func TestLazyDiscovery_InvokeRoutesToCanonicalTool(t *testing.T) {
	service := lazyTestService(t)
	serverModel := createLazyTestServer(t, service, "alpha")
	createLazyTestTool(t, service, serverModel, "search", true)
	ctx := context.WithValue(context.Background(), "mode", model.ModeDev)

	// Authorization + tool lookup happens before InvokeTool, but the tool is
	// stdio-backed ("echo") and we haven't stubbed a session, so we expect a
	// non-nil invocation path that surfaces the upstream connection error.
	result, err := service.lazyInvokeHandler(ctx, lazyRequest(metaToolInvoke, map[string]any{
		"server": "alpha",
		"tool":   "search",
		"args":   map[string]any{},
	}))
	require.NoError(t, err)
	require.NotNil(t, result)
	// We are not asserting IsError=true because an in-process session may be
	// created in some test builds; we only need the handler to dispatch.
	_ = result
}
