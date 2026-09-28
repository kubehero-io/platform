// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kubehero-io/platform/cli/kubehero/internal/config"
	"github.com/kubehero-io/platform/cli/kubehero/internal/mcpserver"
	"github.com/kubehero-io/platform/cli/kubehero/internal/rpc"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

func mcpSession(t *testing.T, f *fakeCP) *mcp.ClientSession {
	t.Helper()
	cl, err := rpc.New(&config.Config{Endpoint: f.srv.URL, Token: f.token})
	if err != nil {
		t.Fatal(err)
	}
	server := mcpserver.New(cl, "test")
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "kubehero-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestMCPListsReadOnlyTools(t *testing.T) {
	cs := mcpSession(t, newFakeCP(t, "tok"))
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must be annotated read-only", tool.Name)
		}
		if len(tool.Description) < 30 {
			t.Errorf("%s: description too thin", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("%s: no input schema", tool.Name)
		}
	}
	sort.Strings(names)
	want := append([]string(nil), mcpserver.ToolNames...)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v\nwant    %v", names, want)
	}
}

func TestMCPCallsTools(t *testing.T) {
	f := newFakeCP(t, "tok")
	cs := mcpSession(t, f)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_cost_allocation", Arguments: map[string]any{
		"window": "30d", "aggregate": []string{"namespace"}, "limit": 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", textOf(t, res))
	}
	var alloc map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &alloc); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if rows, _ := alloc["allocations"].([]any); len(rows) != 1 {
		t.Errorf("limit not applied: %v", alloc["allocations"])
	}
	if f.auths["GetAllocation"] != "Bearer tok" {
		t.Error("MCP tools must use the CLI's token")
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "investigate", Arguments: map[string]any{
		"question": "why did checkout's spend jump?", "window": "24h",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textOf(t, res), "checkout-api spend rose 44%") {
		t.Errorf("investigate result = %s", textOf(t, res))
	}
	if q := f.got("Investigate").(*kuberov1.InvestigateRequest).GetQuestion(); q != "why did checkout's spend jump?" {
		t.Errorf("question = %q", q)
	}

	// Invalid input is a tool error the model can read, not a crash.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "query_logs", Arguments: map[string]any{"query": "no selector"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(textOf(t, res), "stream selector") {
		t.Errorf("want a tool error, got %+v", res)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_alerts", Arguments: map[string]any{"state": "exploded"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("bad enum must be a tool error")
	}
}
