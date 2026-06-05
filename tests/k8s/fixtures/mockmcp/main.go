// Command mockmcp is a minimal in-cluster Streamable-HTTP MCP server used by the
// k8s tests so a real MCP client (bifrost uses mark3labs/mcp-go's
// transport.NewStreamableHTTP) can connect, initialize, and list tools without
// an external dependency.
//
// It serves the MCP endpoint at /mcp (the mcp-go default) and a /healthz probe.
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	addr := ":9000"
	if v := os.Getenv("MOCKMCP_ADDR"); v != "" {
		addr = v
	}

	s := server.NewMCPServer("mock-mcp", "0.1.0")
	// One trivial tool so tools/list returns something during connect.
	s.AddTool(
		mcp.NewTool("ping", mcp.WithDescription("no-op mock tool that returns pong")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("pong"), nil
		},
	)

	streamable := server.NewStreamableHTTPServer(s) // endpoint path defaults to /mcp
	mux := http.NewServeMux()
	mux.Handle("/mcp", streamable)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	log.Printf("mock-mcp: serving streamable MCP on %s/mcp", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("mock-mcp: %v", err)
	}
}
