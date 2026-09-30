// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/kubehero-io/platform/cli/kubehero/internal/mcpserver"
)

func mcpCmd() *cobra.Command {
	var httpAddr string
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Serve KubeHero's read-only tools to AI assistants over MCP",
		Long: `Run a Model Context Protocol server exposing KubeHero as read-only tools:
` + strings.Join(mcpserver.ToolNames, ", ") + `.

Tools call the control plane (and advisor) with this CLI's configured
endpoint and token. Nothing can change cluster state; investigate and
get_briefing return guarded proposals a human applies through the
arming flow.

Transports: stdio (default — the host launches the process) or
streamable HTTP with --http. A bare port (--http :8765) binds
127.0.0.1 only; pass an explicit host (0.0.0.0:8765) to expose it, and
remember anyone who can reach it reads your fleet with your token.

Claude Code:
  claude mcp add kubehero -- kubehero mcp
  # or, against a running HTTP server:
  claude mcp add --transport http kubehero http://127.0.0.1:8765/

Claude Desktop (claude_desktop_config.json):
  {
    "mcpServers": {
      "kubehero": {
        "command": "kubehero",
        "args": ["mcp"],
        "env": { "KUBEHERO_ENDPOINT": "https://kubehero.example.com",
                 "KUBEHERO_TOKEN": "<token>" }
      }
    }
  }

See docs/mcp.md for details.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, _, err := signalClients()
			if err != nil {
				return err
			}
			server := mcpserver.New(cl, Version)
			ctx, stop := signal.NotifyContext(cmdContext(cmd), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			if httpAddr == "" {
				// stdout belongs to the protocol from here on.
				return server.Run(ctx, &mcp.StdioTransport{})
			}
			return serveMCPHTTP(ctx, cmd, server, httpAddr)
		},
	}
	c.Flags().StringVar(&httpAddr, "http", "", "Serve streamable HTTP on this address (e.g. :8765) instead of stdio")
	return c
}

func serveMCPHTTP(ctx context.Context, cmd *cobra.Command, server *mcp.Server, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--http %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	addr = net.JoinHostPort(host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mcpHTTPHandler(server), ReadHeaderTimeout: 10 * time.Second}
	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "KubeHero MCP server (streamable HTTP) on http://%s/\n", ln.Addr())
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		fmt.Fprintln(errOut, "WARNING: listening beyond loopback — anyone who can reach this port can read your fleet with your token.")
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sh)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// mcpHTTPHandler serves server over streamable HTTP. The SDK already
// refuses DNS-rebinding Host headers on loopback; this also refuses
// cross-origin browser requests, so a web page the user visits can't
// drive the server with their token. MCP clients send no Origin and pass.
func mcpHTTPHandler(server *mcp.Server) http.Handler {
	return http.NewCrossOriginProtection().Handler(
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
}
