// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// VSCodeMCPServer drives a running VSCode window through the HTTP server its
// extension (vscode/src/extension.ts) listens on. Each operation is one POST
// of a {"op": …, …} body to that endpoint; the extension dispatches on the
// "op" field and answers with a JSON object, or with {"error": …} when the
// operation failed. An HTTP body needs no quoting anywhere, so unlike the
// Emacs side nothing is base64-encoded.
//
// Buffers are identified by their file path relative to the session root
// (VSCode has no buffer number, and a document's URI is too verbose to hand
// around); a file outside the root keeps its absolute path. Line ranges follow
// the nvim API convention: start is 0-based inclusive, end is 0-based
// exclusive, and end == -1 means "through the last line".
type VSCodeMCPServer struct {
	endpoint string
	// root is this session's directory, reported as the cwd: the first
	// workspace folder of the window that registered. Like the Emacs socket's
	// root it is pinned at registration rather than read back per operation —
	// that is what keeps buffer identities, which are relative to it, stable
	// for the whole session.
	root string
}

// vscodeRequest is the JSON body sent to the extension. Only the fields
// relevant to a given op are populated; the extension reads what it needs per
// op.
type vscodeRequest struct {
	Op    string `json:"op"`
	Buf   string `json:"buf"`
	Path  string `json:"path"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	// edit: Previous is the content the range is expected to still hold, and
	// Content the lines to put in its place.
	Previous []string `json:"previous"`
	Content  []string `json:"content"`
	Line     int      `json:"line"`
	Col      int      `json:"col"`
	// ask: Prompt is the question put to the user, Choices the selectable
	// answers offered in the quick pick.
	Prompt  string   `json:"prompt"`
	Choices []string `json:"choices"`
}

type vscodeBuf struct {
	Path string `json:"path"`
}

// vscodeOpTimeout bounds every operation but ask. An editor operation that
// takes seconds means the extension is gone, and claude is better served by a
// prompt error than by a call that never returns; ask legitimately blocks for
// as long as the user thinks, so it opts out.
const vscodeOpTimeout = 5 * time.Second

// post sends req to the extension and decodes the reply into out, which may be
// nil to discard it. An {"error": …} reply becomes a Go error, so a caller only
// ever sees a successful operation.
func (self *VSCodeMCPServer) post(client *http.Client, req vscodeRequest, out any) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}

	resp, err := client.Post(self.endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("post %s to vscode at %s: %w", req.Op, self.endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("vscode %s: %w", req.Op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vscode %s: %s: %s", req.Op, resp.Status, strings.TrimSpace(string(body)))
	}

	var fail struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &fail); err == nil && fail.Error != "" {
		return fmt.Errorf("vscode %s: %s", req.Op, fail.Error)
	}

	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (self *VSCodeMCPServer) call(req vscodeRequest, out any) error {
	return self.post(&http.Client{Timeout: vscodeOpTimeout}, req, out)
}

// callBlocking runs an op that waits on the user, with no client timeout.
func (self *VSCodeMCPServer) callBlocking(req vscodeRequest, out any) error {
	return self.post(&http.Client{}, req, out)
}

func (self *VSCodeMCPServer) cwd() (string, error) {
	return self.root, nil
}

// rel expresses an absolute path as a buffer id, i.e. relative to the session
// root and slash-separated, whatever the host separator is: glob patterns are
// matched against these ids, and "**/*.go" would miss "src\main.go". A path
// outside the root is returned unchanged, still absolute.
func (self *VSCodeMCPServer) rel(path string) string {
	if self.root == "" {
		return path
	}
	rel, err := filepath.Rel(self.root, path)
	// Only a leading ".." SEGMENT escapes the root: a plain HasPrefix would
	// also reject a file legitimately named "..config".
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.ToSlash(rel)
}

func (self *VSCodeMCPServer) buffers() ([]vscodeBuf, error) {
	var resp struct {
		Buffers []vscodeBuf `json:"buffers"`
	}
	if err := self.call(vscodeRequest{Op: "list-buffers"}, &resp); err != nil {
		return nil, err
	}
	return resp.Buffers, nil
}

func (self *VSCodeMCPServer) readLines(buf string, start, end int) ([]string, error) {
	var resp struct {
		Lines []string `json:"lines"`
	}
	err := self.call(vscodeRequest{Op: "read-lines", Buf: buf, Start: start, End: end}, &resp)
	return resp.Lines, err
}

type VSCodeReadBufferInput struct {
	Buffer string `json:"buffer" jsonschema:"the buffer to read, as a path relative to the session root"`
	Start  *int   `json:"start,omitempty" jsonschema:"first line to read, 1-based inclusive; omit to start at the top"`
	End    *int   `json:"end,omitempty" jsonschema:"last line to read, 1-based inclusive; omit to read to the end"`
}

type VSCodeEditBufferInput struct {
	Buffer   string `json:"buffer" jsonschema:"the buffer to edit, as a path relative to the session root"`
	Start    *int   `json:"start,omitempty" jsonschema:"first line to replace, 1-based inclusive; omit to start at the top of the buffer"`
	Previous string `json:"previous" jsonschema:"the content currently expected at the replaced range, exactly as last read (lines separated by \\n). The edit is rejected if the live buffer no longer matches it, and the replaced range length is derived from this. Omit to insert at start without removing any line"`
	Content  string `json:"content" jsonschema:"the replacement content; lines are separated by \\n"`
}

type VSCodeSaveBufferInput struct {
	Buffer string `json:"buffer" jsonschema:"the buffer to save to disk, as a path relative to the session root"`
}

type VSCodeJumpInput struct {
	Buffer string `json:"buffer" jsonschema:"the buffer to show in the active editor group, as a path relative to the session root"`
	Line   int    `json:"line" jsonschema:"1-based line to place the cursor on"`
	Column *int   `json:"column,omitempty" jsonschema:"1-based column to place the cursor on; defaults to 1"`
}

func (self *VSCodeMCPServer) readBuffer(_ context.Context, _ *mcp.CallToolRequest, in VSCodeReadBufferInput) (*mcp.CallToolResult, any, error) {
	start := 0
	if in.Start != nil {
		start = max(*in.Start-1, 0)
	}
	end := -1
	if in.End != nil {
		end = *in.End
	}

	lines, err := self.readLines(in.Buffer, start, end)
	if err != nil {
		return nil, nil, fmt.Errorf("read buffer %s: %w", in.Buffer, err)
	}

	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%6d\t%s\n", start+i+1, l)
	}

	return textResult(b.String()), nil, nil
}

func (self *VSCodeMCPServer) editBuffer(_ context.Context, _ *mcp.CallToolRequest, in VSCodeEditBufferInput) (*mcp.CallToolResult, any, error) {
	start := 0
	if in.Start != nil {
		start = max(*in.Start-1, 0)
	}

	previous := splitContentLines(in.Previous)
	replacement := splitContentLines(in.Content)

	// The extension answers { ok, reason? }; a false ok means the live buffer
	// no longer matches `previous`, i.e. the user edited the range under us.
	var res struct {
		Ok     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	req := vscodeRequest{Op: "edit", Buf: in.Buffer, Start: start, Previous: previous, Content: replacement}
	if err := self.call(req, &res); err != nil {
		return nil, nil, fmt.Errorf("edit buffer %s: %w", in.Buffer, err)
	}
	if !res.Ok {
		return nil, nil, fmt.Errorf("edit buffer %s rejected: %s", in.Buffer, res.Reason)
	}

	return textResult(fmt.Sprintf("wrote %d lines to buffer %s", len(replacement), in.Buffer)), nil, nil
}

// openBuffers returns the session's opened buffers as buffer ids. If include is
// non-nil, only the buffers whose id matches it are returned. These are the
// buffers glob and grep operate over.
func (self *VSCodeMCPServer) openBuffers(include *regexp.Regexp) ([]string, error) {
	bufs, err := self.buffers()
	if err != nil {
		return nil, err
	}

	var refs []string
	for _, buf := range bufs {
		rel := self.rel(buf.Path)
		// Scope to the session's root. A multi-root window lists buffers from
		// every folder, so drop those the root cannot be dropped from — rel
		// leaves such a path absolute.
		if self.root != "" && filepath.IsAbs(rel) {
			continue
		}
		if include != nil && !include.MatchString(rel) {
			continue
		}

		refs = append(refs, rel)
	}
	return refs, nil
}

func (self *VSCodeMCPServer) glob(_ context.Context, _ *mcp.CallToolRequest, in GlobInput) (*mcp.CallToolResult, any, error) {
	re, err := globToRegexp(in.Pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid pattern %q: %w", in.Pattern, err)
	}

	refs, err := self.openBuffers(re)
	if err != nil {
		return nil, nil, err
	}

	if len(refs) == 0 {
		return textResult("no matches"), nil, nil
	}

	return textResult(strings.Join(refs, "\n")), nil, nil
}

func (self *VSCodeMCPServer) grep(_ context.Context, _ *mcp.CallToolRequest, in GrepInput) (*mcp.CallToolResult, any, error) {
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid pattern %q: %w", in.Pattern, err)
	}

	var include *regexp.Regexp
	if in.Include != "" {
		include, err = globToRegexp(in.Include)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid include %q: %w", in.Include, err)
		}
	}

	refs, err := self.openBuffers(include)
	if err != nil {
		return nil, nil, err
	}

	const maxMatches = 500
	var out []string
	for _, ref := range refs {
		lines, err := self.readLines(ref, 0, -1)
		if err != nil {
			return nil, nil, fmt.Errorf("read buffer %s: %w", ref, err)
		}
		for n, line := range lines {
			if re.MatchString(line) {
				out = append(out, fmt.Sprintf("%s:%d:%s", ref, n+1, line))
				if len(out) >= maxMatches {
					break
				}
			}
		}
		if len(out) >= maxMatches {
			break
		}
	}

	if len(out) == 0 {
		return textResult("no matches"), nil, nil
	}
	text := strings.Join(out, "\n")
	if len(out) >= maxMatches {
		text += fmt.Sprintf("\n(truncated at %d matches)", maxMatches)
	}
	return textResult(text), nil, nil
}

func (self *VSCodeMCPServer) listBuffers(_ context.Context, _ *mcp.CallToolRequest, _ ListBuffersInput) (*mcp.CallToolResult, any, error) {
	bufs, err := self.buffers()
	if err != nil {
		return nil, nil, err
	}

	var b strings.Builder
	for _, buf := range bufs {
		fmt.Fprintf(&b, "%s\t%s\n", self.rel(buf.Path), buf.Path)
	}

	if b.Len() == 0 {
		return textResult("no buffers"), nil, nil
	}
	return textResult(b.String()), nil, nil
}

func (self *VSCodeMCPServer) openBuffer(_ context.Context, _ *mcp.CallToolRequest, in OpenBufferInput) (*mcp.CallToolResult, any, error) {
	path := in.Path
	if !filepath.IsAbs(path) {
		dir, err := self.cwd()
		if err != nil {
			return nil, nil, err
		}
		path = filepath.Join(dir, path)
	}

	if _, err := os.Stat(path); err != nil {
		return nil, nil, fmt.Errorf("open buffer: %w", err)
	}

	// The extension echoes back the path VSCode resolved the document to, which
	// is what the buffer id has to be derived from: a symlinked or
	// case-insensitive path would otherwise name a buffer no other tool call
	// can find again.
	var resp struct {
		Path string `json:"path"`
	}
	if err := self.call(vscodeRequest{Op: "open", Path: path}, &resp); err != nil {
		return nil, nil, err
	}

	return textResult(fmt.Sprintf("buffer %s\t%s", self.rel(resp.Path), resp.Path)), nil, nil
}

func (self *VSCodeMCPServer) saveBuffer(_ context.Context, _ *mcp.CallToolRequest, in VSCodeSaveBufferInput) (*mcp.CallToolResult, any, error) {
	if err := self.call(vscodeRequest{Op: "save", Buf: in.Buffer}, nil); err != nil {
		return nil, nil, fmt.Errorf("save buffer %s: %w", in.Buffer, err)
	}

	return textResult(fmt.Sprintf("saved buffer %s", in.Buffer)), nil, nil
}

func (self *VSCodeMCPServer) jump(_ context.Context, _ *mcp.CallToolRequest, in VSCodeJumpInput) (*mcp.CallToolResult, any, error) {
	col := 1
	if in.Column != nil {
		col = *in.Column
	}

	if err := self.call(vscodeRequest{Op: "jump", Buf: in.Buffer, Line: in.Line, Col: col}, nil); err != nil {
		return nil, nil, fmt.Errorf("jump to buffer %s: %w", in.Buffer, err)
	}

	return textResult(fmt.Sprintf("jumped to buffer %s at %d:%d (Go Back / Alt+Left returns)", in.Buffer, in.Line, col)), nil, nil
}

// VSCodeXrefInput locates the identifier a definition or reference query runs
// on. VSCode's language servers resolve symbols semantically, so this is what
// glob/grep can't do: follow a name to where it's defined or used.
type VSCodeXrefInput struct {
	Buffer string `json:"buffer" jsonschema:"the buffer containing the identifier, as a path relative to the session root"`
	Line   int    `json:"line" jsonschema:"1-based line the identifier is on"`
	Column *int   `json:"column,omitempty" jsonschema:"1-based column landing on the identifier; defaults to 1. The position must sit on the symbol for the language server to resolve it"`
}

// vscodeLocation is one result of a definition or reference query: a
// file-backed definition or use site.
type vscodeLocation struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Summary string `json:"summary"`
}

// xref runs op ("find-definition" or "find-references") at the requested
// position and renders the resulting locations as "path:line: summary" lines,
// with the path relative to the session root (like grep) when it can be.
func (self *VSCodeMCPServer) xref(op string, in VSCodeXrefInput) (*mcp.CallToolResult, any, error) {
	col := 1
	if in.Column != nil {
		col = *in.Column
	}

	var resp struct {
		Locations []vscodeLocation `json:"locations"`
	}
	if err := self.call(vscodeRequest{Op: op, Buf: in.Buffer, Line: in.Line, Col: col}, &resp); err != nil {
		return nil, nil, err
	}

	if len(resp.Locations) == 0 {
		return textResult("no results"), nil, nil
	}

	const maxLocations = 200
	var out []string
	for i, loc := range resp.Locations {
		if i >= maxLocations {
			break
		}
		out = append(out, fmt.Sprintf("%s:%d:\t%s", self.rel(loc.File), loc.Line, loc.Summary))
	}

	text := strings.Join(out, "\n")
	if len(resp.Locations) > maxLocations {
		text += fmt.Sprintf("\n(truncated at %d results)", maxLocations)
	}
	return textResult(text), nil, nil
}

func (self *VSCodeMCPServer) findDefinition(_ context.Context, _ *mcp.CallToolRequest, in VSCodeXrefInput) (*mcp.CallToolResult, any, error) {
	return self.xref("find-definition", in)
}

func (self *VSCodeMCPServer) findReferences(_ context.Context, _ *mcp.CallToolRequest, in VSCodeXrefInput) (*mcp.CallToolResult, any, error) {
	return self.xref("find-references", in)
}

type VSCodeDiagnosticsInput struct {
	Buffer string `json:"buffer" jsonschema:"the buffer to report diagnostics for, as a path relative to the session root"`
}

// vscodeDiagnostic is one problem a language server reported.
type vscodeDiagnostic struct {
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

func (self *VSCodeMCPServer) diagnostics(_ context.Context, _ *mcp.CallToolRequest, in VSCodeDiagnosticsInput) (*mcp.CallToolResult, any, error) {
	var resp struct {
		Diagnostics []vscodeDiagnostic `json:"diagnostics"`
		// Analyzed tells the two readings of an empty list apart: false means
		// no language server has reported on the document yet.
		Analyzed bool `json:"analyzed"`
	}
	if err := self.call(vscodeRequest{Op: "diagnostics", Buf: in.Buffer}, &resp); err != nil {
		return nil, nil, fmt.Errorf("diagnostics for %s: %w", in.Buffer, err)
	}

	if len(resp.Diagnostics) == 0 {
		// Reporting an unanalyzed document as problem-free would invent an
		// answer, which is exactly what these tools exist not to do — a
		// document opened by open_buffer has no editor showing it, so its
		// language server may not have looked at it yet.
		if !resp.Analyzed {
			return textResult("no diagnostics available yet (language server may still be analyzing this document)"), nil, nil
		}
		return textResult("no diagnostics"), nil, nil
	}

	// Language servers report in no guaranteed order; sort by position so the
	// output reads top-to-bottom like the buffer.
	sort.SliceStable(resp.Diagnostics, func(i, j int) bool {
		a, b := resp.Diagnostics[i], resp.Diagnostics[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})

	var out []string
	for _, d := range resp.Diagnostics {
		out = append(out, fmt.Sprintf("%d:%d: %s: %s", d.Line, d.Column, d.Severity, d.Message))
	}
	return textResult(strings.Join(out, "\n")), nil, nil
}

// VSCodeAskInput is a multiple-choice question put to the user. VSCode renders
// the choices in a quick pick and the call blocks until one is picked; the pick
// comes back as the tool result. Use this to let the user decide instead of
// guessing.
type VSCodeAskInput struct {
	Question string   `json:"question" jsonschema:"the question to put to the user"`
	Choices  []string `json:"choices" jsonschema:"the selectable answers to offer; the user picks exactly one"`
}

func (self *VSCodeMCPServer) ask(_ context.Context, _ *mcp.CallToolRequest, in VSCodeAskInput) (*mcp.CallToolResult, any, error) {
	if len(in.Choices) == 0 {
		return nil, nil, fmt.Errorf("ask requires at least one choice")
	}

	var resp struct {
		Answer string `json:"answer"`
	}
	if err := self.callBlocking(vscodeRequest{Op: "ask", Prompt: in.Question, Choices: in.Choices}, &resp); err != nil {
		return nil, nil, fmt.Errorf("ask user: %w", err)
	}

	return textResult(resp.Answer), nil, nil
}

func (self *VSCodeMCPServer) Kind() SupportedApp {
	return Vscode
}

func (self *VSCodeMCPServer) NewMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "sidekick for vscode",
		Version: "0.1.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_buffer",
		Description: "Read a VSCode buffer, optionally a line range. Output is prefixed with 1-based line numbers (like cat -n). The buffer id is the file path relative to the session root, reported by the /listen monitor as the \"buf\" field.",
	}, self.readBuffer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "edit_buffer",
		Description: "Edit content of a VSCode buffer, guarded against clobbering concurrent edits. Replace `previous` at `start` with `content` — the edit applies only if the live buffer still matches its previous content; otherwise it is rejected and you should re-read and retry. The replaced range length is derived from `previous`. `start` is 1-based; omit it to start at the top. The buffer id is the \"buf\" field from /listen. The buffer is left unsaved, like a user edit: call save_buffer to write it to disk.",
	}, self.editBuffer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "glob",
		Description: "List the session's opened buffers whose path matches a glob pattern. Paths are matched relative to the VSCode session's root: * and ? stay within a path segment, ** spans segments, and a leading \"**/\" matches zero or more directories (so \"**/*.go\" matches both main.go and src/foo.go). Only opened buffers are considered — the open editor tabs plus whatever open_buffer brought in; it does not walk the filesystem. Use open_buffer first to bring a file into scope.",
	}, self.glob)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "grep",
		Description: "Search the contents of the session's opened buffers with a regular expression. Optionally restrict to buffers whose path matches an include glob (relative to the session's root). Only opened buffers are searched — the open editor tabs plus whatever open_buffer brought in; it does not walk the filesystem, so use open_buffer first to bring a file into scope. Returns path:line:text.",
	}, self.grep)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_buffers",
		Description: "List the VSCode session's opened buffers as \"buffer<TAB>absolute path\" lines. The first field is the buffer id the other tools take.",
	}, self.listBuffers)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "open_buffer",
		Description: "Open a file as a VSCode buffer so it can be read/edited, without opening a tab or moving the user's focus. Path may be relative to the session's root. Returns the new buffer id. Use this instead of the Read/Write tools when a file you need isn't already a buffer.",
	}, self.openBuffer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "save_buffer",
		Description: "Save (write to disk) a VSCode buffer. Use after edit_buffer to persist changes. The buffer id is the \"buf\" field reported by the /listen monitor.",
	}, self.saveBuffer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "jump",
		Description: "Show a buffer in the active editor group and move the cursor to a line (and optional column). VSCode records where the user was in its navigation history, so they can press Alt+Left (Go Back) to return — use this to take the user to a location you found (e.g. \"the function doing X\"). It is the only tool that moves the user's focus. The buffer id is the \"buf\" field from /listen, or from open_buffer/list_buffers.",
	}, self.jump)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "find_definition",
		Description: "Find where the symbol at a buffer location is defined, using the language server VSCode has attached to the file. Put line/column on the identifier. Returns path:line: summary for each definition. Prefer this over grep for \"where is X defined\" — it understands the language, not just the text.",
	}, self.findDefinition)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "find_references",
		Description: "Find where the symbol at a buffer location is referenced, using the language server VSCode has attached to the file. Put line/column on the identifier. Returns path:line: summary for each use site. Prefer this over grep for \"who calls X\" — it resolves the symbol semantically rather than matching text.",
	}, self.findReferences)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "buffer_diagnostics",
		Description: "List the diagnostics (errors, warnings, information, hints) VSCode already knows about for a buffer, from its language servers — the same problems shown in the Problems panel. Returns line:column: severity: message. Use it to see what a language server reports before and after your edits, instead of running an external build. A buffer no language server has reported on yet is called out as such rather than as problem-free.",
	}, self.diagnostics)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "ask",
		Description: "Put a multiple-choice question to the user in VSCode and get their pick back. VSCode shows the choices in a quick pick and this call blocks until the user selects one; the selected string is returned as the result. Dismissing the question fails the call instead of answering it, so you never act on a choice the user did not make. Use it whenever you would otherwise guess between concrete alternatives — offer the options and let the user decide.",
	}, self.ask)

	return server
}

type VSCodeNotifyParams struct {
	Buf  string `json:"buf"`
	File string `json:"file"`
}

func (self *VSCodeMCPServer) UnmarshalNotifyJSONParams(data []byte) (any, error) {
	var r VSCodeNotifyParams
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}

	return r, nil
}
