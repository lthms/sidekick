package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/alecthomas/kong"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var cli struct {
	Port int `help:"Port to listen on." short:"p" default:"8000" env:"COMPANION_PORT"`
}

func main() {
	kong.Parse(&cli, kong.Name("companion"),
		kong.Description("companion daemon"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := &http.Server{Addr: fmt.Sprintf(":%d", cli.Port), Handler: newRPCHandler()}
	context.AfterFunc(ctx, func() { _ = srv.Shutdown(context.Background()) })

	slog.Info("companion server listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func newRPCHandler() http.Handler {
	registry := newRegistry()
	handleMCP := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		pid, _ := strconv.Atoi(r.PathValue("pid"))
		mcp, _ := registry.mcp(pid)
		return mcp.NewMCPServer()
	}, &mcp.StreamableHTTPOptions{Stateless: true})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /{$}", handleRPC(registry))
	mux.Handle("POST /mcp/{pid}", handleMCP)
	mux.HandleFunc("GET /listen/{pid}", handleListen(registry))

	return mux
}
