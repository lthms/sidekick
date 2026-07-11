package main

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SupportedEditor int

const (
	Nvim SupportedEditor = iota
)

func (e SupportedEditor) String() string {
	switch e {
	case Nvim:
		return "nvim"
	default:
		return fmt.Sprintf("SupportedEditor(%d)", int(e))
	}
}

var encodings = map[string]SupportedEditor{"nvim": Nvim}

func (e *SupportedEditor) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	t, ok := encodings[s]
	if !ok {
		return fmt.Errorf("unknown editor: %q", s)
	}
	*e = t
	return nil
}

type EditorMCPSever interface {
	Kind() SupportedEditor
	NewMCPServer() *mcp.Server
	UnmarshalNotifyJSONParams(data []byte) (any, error)
}
