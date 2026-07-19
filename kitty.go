// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Maintainer: Thomas Letan <lthms@soap.coffee>

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type KittyMCPServer struct {
	socket string
}

type SendTextInput struct {
	Text string `json:"text" jsonschema:"the text to type into the target kitty window"`
}

// kittyRC runs `kitty @ --to <socket> <args...>`. Central so every remote-control
// call shares one path (and one place to add auth / error handling later).
func (self *KittyMCPServer) kittyRC(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"@", "--to", self.socket}, args...)
	out, err := exec.CommandContext(ctx, "kitty", full...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("kitty @ %v: %w: %s", args, err, out)
	}
	return out, nil
}

func (self *KittyMCPServer) sendText(ctx context.Context, _ *mcp.CallToolRequest, in SendTextInput) (*mcp.CallToolResult, any, error) {
	// TODO: decide target window (caveat 2) and whether to append a carriage
	// return to auto-run (caveat 3 — default is NO, paste only). For now this
	// sends to the currently-focused window with no trailing CR.
	// TODO: prefer feeding `in.Text` over stdin (`send-text --stdin`) to avoid
	// kitty's escape-sequence interpretation of the argument form.
	if _, err := self.kittyRC(ctx, "send-text", in.Text); err != nil {
		return nil, nil, err
	}
	return textResult(fmt.Sprintf("sent %d bytes to kitty", len(in.Text))), nil, nil
}

func (self *KittyMCPServer) Kind() SupportedApp {
	return Kitty
}

func (self *KittyMCPServer) NewMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "sidekick for kitty",
		Version: "0.1.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_text",
		Description: "Type text into the user's kitty terminal window via kitty's remote-control protocol. Use this to hand the user a shell command they asked for; it is pasted, not executed (they press Enter).",
	}, self.sendText)

	return server
}

type KittyNotifyParams struct {
	Text string `json:"text"`
	Pwd  string `json:"pwd"`
}

func (self *KittyMCPServer) UnmarshalNotifyJSONParams(data []byte) (any, error) {
	var r KittyNotifyParams
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return r, nil
}
