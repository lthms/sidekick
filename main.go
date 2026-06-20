package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := &http.Server{Addr: ":8000", Handler: newRPCHandler()}
	context.AfterFunc(ctx, func() { _ = srv.Shutdown(context.Background()) })

	slog.Info("companion server listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func newRPCHandler() http.Handler {
	registry := newRegistry()
	handleMCP := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server {
		pid, _ := strconv.Atoi(r.PathValue("pid"))
		return newMCPServer(registry, pid)
	}, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /{$}", handleRPC(registry))
	mux.Handle("GET /mcp/{pid}", handleMCP)
	mux.Handle("POST /mcp/{pid}", handleMCP)
	mux.HandleFunc("GET /listen/{pid}", handleListen(registry))

	return mux
}
