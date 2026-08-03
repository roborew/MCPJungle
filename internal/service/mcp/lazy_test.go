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

func TestLazyProxyServer_RegistersFixedMetaTools(t *testing.T) {
	service := lazyTestService(t)
	tools := service.LazyMcpProxyServer().ListTools()
	assert.Len(t, tools, 4)
	assert.Contains(t, tools, metaToolListServers)
	assert.Contains(t, tools, metaToolListTools)
	assert.Contains(t, tools, metaToolDescribe)
	assert.Contains(t, tools, metaToolInvoke)
}

func TestLazyDiscovery_ListsOnlyEnabledPersistedMetadata(t *testing.T) {
	service := lazyTestService(t)
	alpha := createLazyTestServer(t, service, "alpha")
	beta := createLazyTestServer(t, service, "beta")
	createLazyTestTool(t, service, alpha, "search", true)
	createLazyTestTool(t, service, alpha, "disabled", false)
	createLazyTestTool(t, service, beta, "lookup", true)
	ctx := context.WithValue(context.Background(), "mode", model.ModeDev)

	servers, err := service.lazyListServersHandler(ctx, lazyRequest(metaToolListServers, nil))
	require.NoError(t, err)
	var serverResult []map[string]string
	lazyResultJSON(t, servers, &serverResult)
	assert.Equal(t, []string{"alpha", "beta"}, []string{serverResult[0]["name"], serverResult[1]["name"]})

	tools, err := service.lazyListToolsHandler(ctx, lazyRequest(metaToolListTools, map[string]any{"server": "alpha"}))
	require.NoError(t, err)
	var toolResult []map[string]any
	lazyResultJSON(t, tools, &toolResult)
	require.Len(t, toolResult, 1)
	assert.Equal(t, "alpha__search", toolResult[0]["name"])
	assert.NotEmpty(t, toolResult[0]["input_schema"])
}

func TestLazyDiscovery_DescribeUsesRegistryWithoutUpstreamInitialization(t *testing.T) {
	service := lazyTestService(t)
	serverModel := createLazyTestServer(t, service, "offline")
	createLazyTestTool(t, service, serverModel, "inspect", true)
	ctx := context.WithValue(context.Background(), "mode", model.ModeDev)

	result, err := service.lazyDescribeHandler(ctx, lazyRequest(metaToolDescribe, map[string]any{"server": "offline", "tool": "inspect"}))
	require.NoError(t, err)
	var metadata map[string]any
	lazyResultJSON(t, result, &metadata)
	assert.Equal(t, "offline__inspect", metadata["name"])
}
