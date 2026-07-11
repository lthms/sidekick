package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/neovim/go-client/nvim"
)

type NvimMCPServer struct {
	endpoint string
}

type ReadBufferInput struct {
	Buffer int  `json:"buffer" jsonschema:"the neovim buffer id to read"`
	Start  *int `json:"start,omitempty" jsonschema:"first line to read, 1-based inclusive; omit to start at the top"`
	End    *int `json:"end,omitempty" jsonschema:"last line to read, 1-based inclusive; omit to read to the end"`
}

type WriteBufferInput struct {
	Buffer  int    `json:"buffer" jsonschema:"the neovim buffer id to write"`
	Content string `json:"content" jsonschema:"the replacement content; lines are separated by \\n"`
	Start   *int   `json:"start,omitempty" jsonschema:"first line to replace, 1-based inclusive; omit (with end) to replace the whole buffer"`
	End     *int   `json:"end,omitempty" jsonschema:"last line to replace, 1-based inclusive; use start-1 to insert before start without removing any line"`
}

type GlobInput struct {
	Pattern string `json:"pattern" jsonschema:"glob matched against opened buffers' paths, relative to the session cwd; a leading **/ matches zero or more directories, so **/*.go matches main.go too. Examples: **/*.go, src/*.ts"`
}

type GrepInput struct {
	Pattern string `json:"pattern" jsonschema:"regular expression to search for (RE2 syntax)"`
	Include string `json:"include,omitempty" jsonschema:"optional glob restricting which opened buffers are searched, matched against their path relative to the session cwd; a leading **/ matches zero or more directories, so **/*.go also includes root-level files. Example: **/*.go"`
}

type ListBuffersInput struct{}

type OpenBufferInput struct {
	Path string `json:"path" jsonschema:"path to the file to open as a buffer; relative paths resolve against the nvim session's working directory"`
}

type SaveBufferInput struct {
	Buffer int `json:"buffer" jsonschema:"the neovim buffer id to save to disk"`
}

type JumpInput struct {
	Buffer  int    `json:"buffer" jsonschema:"the neovim buffer id to make active in the current window"`
	Line    int    `json:"line" jsonschema:"1-based line to place the cursor on"`
	Column  *int   `json:"column,omitempty" jsonschema:"1-based column to place the cursor on; defaults to 1"`
	Tagname string `json:"tagname,omitempty" jsonschema:"label shown in the tag stack (:tags) for this jump; defaults to the target location"`
}

func (self *NvimMCPServer) dial() (*nvim.Nvim, error) {
	v, err := nvim.Dial(self.endpoint)
	if err != nil {
		return nil, fmt.Errorf("dial nvim at %s: %w", self.endpoint, err)
	}

	return v, nil
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

func (self *NvimMCPServer) readBuffer(_ context.Context, _ *mcp.CallToolRequest, in ReadBufferInput) (*mcp.CallToolResult, any, error) {
	v, err := self.dial()
	if err != nil {
		return nil, nil, err
	}
	defer v.Close()

	start := 0
	if in.Start != nil {
		start = max(*in.Start-1, 0)
	}
	end := -1
	if in.End != nil {
		end = *in.End
	}

	lines, err := v.BufferLines(nvim.Buffer(in.Buffer), start, end, true)
	if err != nil {
		return nil, nil, fmt.Errorf("read buffer %d: %w", in.Buffer, err)
	}

	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%6d\t%s\n", start+i+1, string(l))
	}

	return textResult(b.String()), nil, nil
}

func (self *NvimMCPServer) writeBuffer(_ context.Context, _ *mcp.CallToolRequest, in WriteBufferInput) (*mcp.CallToolResult, any, error) {
	v, err := self.dial()
	if err != nil {
		return nil, nil, err
	}
	defer v.Close()

	start, end := 0, -1
	if in.Start != nil || in.End != nil {
		if in.Start == nil || in.End == nil {
			return nil, nil, fmt.Errorf("start and end must be provided together")
		}
		start = max(*in.Start-1, 0)
		end = *in.End
	}

	// Empty content deletes the range (or empties the buffer); otherwise split
	// into lines. SetBufferLines treats a nil replacement as a deletion. A single
	// trailing newline marks the last line's EOL rather than an extra empty line,
	// so strip one before splitting — otherwise "A\nB\n" would yield three lines.
	replacement := [][]byte{}
	if in.Content != "" {
		for l := range strings.SplitSeq(strings.TrimSuffix(in.Content, "\n"), "\n") {
			replacement = append(replacement, []byte(l))
		}
	}

	if err := v.SetBufferLines(nvim.Buffer(in.Buffer), start, end, true, replacement); err != nil {
		return nil, nil, fmt.Errorf("write buffer %d: %w", in.Buffer, err)
	}

	return textResult(fmt.Sprintf("wrote %d lines to buffer %d", len(replacement), in.Buffer)), nil, nil
}

// cwd returns the working directory of the neovim session.
func (self *NvimMCPServer) cwd() (string, error) {
	v, err := self.dial()
	if err != nil {
		return "", err
	}
	defer v.Close()

	var dir string
	if err := v.Eval("getcwd()", &dir); err != nil {
		return "", fmt.Errorf("get nvim cwd: %w", err)
	}
	return dir, nil
}

// globToRegexp translates a shell-style glob (with **, *, ?) into an anchored regexp.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// "**/" matches any run of leading directories, including
				// none, so "**/*.go" matches both main.go and src/foo.go.
				// Consume the trailing slash too and make the whole segment
				// optional; otherwise the literal "/" forces at least one
				// directory and root-level files silently never match.
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// bufRef pairs a loaded neovim buffer with its path relative to the session cwd.
type bufRef struct {
	buf nvim.Buffer
	rel string
}

// openBuffers returns the loaded buffers that are backed by a file, with each
// name expressed relative to the session cwd. If include is non-nil, only
// buffers whose relative path matches it are returned. These are the buffers
// glob and grep operate over.
func (self *NvimMCPServer) openBuffers(v *nvim.Nvim, include *regexp.Regexp) ([]bufRef, error) {
	root, err := self.cwd()
	if err != nil {
		return nil, err
	}

	bufs, err := v.Buffers()
	if err != nil {
		return nil, fmt.Errorf("list buffers: %w", err)
	}

	var refs []bufRef
	for _, buf := range bufs {
		loaded, err := v.IsBufferLoaded(buf)
		if err != nil {
			return nil, fmt.Errorf("buffer %d loaded: %w", int(buf), err)
		}
		if !loaded {
			continue
		}

		name, err := v.BufferName(buf)
		if err != nil {
			return nil, fmt.Errorf("name of buffer %d: %w", int(buf), err)
		}
		if name == "" {
			continue
		}

		rel := name
		if r, err := filepath.Rel(root, name); err == nil {
			rel = r
		}
		if include != nil && !include.MatchString(rel) {
			continue
		}

		refs = append(refs, bufRef{buf: buf, rel: rel})
	}
	return refs, nil
}

func (self *NvimMCPServer) glob(_ context.Context, _ *mcp.CallToolRequest, in GlobInput) (*mcp.CallToolResult, any, error) {
	re, err := globToRegexp(in.Pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid pattern %q: %w", in.Pattern, err)
	}

	v, err := self.dial()
	if err != nil {
		return nil, nil, err
	}
	defer v.Close()

	refs, err := self.openBuffers(v, re)
	if err != nil {
		return nil, nil, err
	}

	if len(refs) == 0 {
		return textResult("no matches"), nil, nil
	}

	var matches []string
	for _, ref := range refs {
		matches = append(matches, ref.rel)
	}
	return textResult(strings.Join(matches, "\n")), nil, nil
}

func (self *NvimMCPServer) grep(_ context.Context, _ *mcp.CallToolRequest, in GrepInput) (*mcp.CallToolResult, any, error) {
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

	v, err := self.dial()
	if err != nil {
		return nil, nil, err
	}
	defer v.Close()

	refs, err := self.openBuffers(v, include)
	if err != nil {
		return nil, nil, err
	}

	const maxMatches = 500
	var out []string
	for _, ref := range refs {
		lines, err := v.BufferLines(ref.buf, 0, -1, true)
		if err != nil {
			return nil, nil, fmt.Errorf("read buffer %d: %w", int(ref.buf), err)
		}
		for n, line := range lines {
			if re.Match(line) {
				out = append(out, fmt.Sprintf("%s:%d:%s", ref.rel, n+1, string(line)))
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

func (self *NvimMCPServer) listBuffers(_ context.Context, _ *mcp.CallToolRequest, _ ListBuffersInput) (*mcp.CallToolResult, any, error) {
	v, err := self.dial()
	if err != nil {
		return nil, nil, err
	}
	defer v.Close()

	bufs, err := v.Buffers()
	if err != nil {
		return nil, nil, fmt.Errorf("list buffers: %w", err)
	}

	var b strings.Builder
	for _, buf := range bufs {
		name, err := v.BufferName(buf)
		if err != nil {
			return nil, nil, fmt.Errorf("name of buffer %d: %w", int(buf), err)
		}
		if name == "" {
			name = "[No Name]"
		}
		fmt.Fprintf(&b, "%d\t%s\n", int(buf), name)
	}

	if b.Len() == 0 {
		return textResult("no buffers"), nil, nil
	}
	return textResult(b.String()), nil, nil
}

func (self *NvimMCPServer) openBuffer(_ context.Context, _ *mcp.CallToolRequest, in OpenBufferInput) (*mcp.CallToolResult, any, error) {
	v, err := self.dial()
	if err != nil {
		return nil, nil, err
	}
	defer v.Close()

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

	// bufadd() creates (or returns the existing) buffer for the file without
	// touching the current window; bufload() reads it into memory so the buffer
	// id is usable for read/write. Neither changes the active buffer.
	var bufnr int
	if err := v.Eval(fmt.Sprintf("bufadd(%q)", path), &bufnr); err != nil {
		return nil, nil, fmt.Errorf("bufadd %s: %w", path, err)
	}
	if err := v.Command(fmt.Sprintf("call bufload(%d)", bufnr)); err != nil {
		return nil, nil, fmt.Errorf("bufload %d: %w", bufnr, err)
	}
	// Setting 'buflisted' so the buffer shows up in :ls and can be
	// visited normally.
	if err := v.Command(fmt.Sprintf("call setbufvar(%d, '&buflisted', 1)", bufnr)); err != nil {
		return nil, nil, fmt.Errorf("set buflisted %d: %w", bufnr, err)
	}

	return textResult(fmt.Sprintf("buffer %d\t%s", bufnr, path)), nil, nil
}

func (self *NvimMCPServer) saveBuffer(_ context.Context, _ *mcp.CallToolRequest, in SaveBufferInput) (*mcp.CallToolResult, any, error) {
	v, err := self.dial()
	if err != nil {
		return nil, nil, err
	}
	defer v.Close()

	// nvim_buf_call runs :write in the target buffer's context, so the active
	// buffer and window are left untouched. It is only reachable through the Lua
	// API (vim.api) — in nvim 0.12 the nvim_* functions are no longer exposed as
	// Vimscript-callable functions, so "call nvim_buf_call(...)" fails with E117.
	const save = `vim.api.nvim_buf_call(..., function() vim.cmd('write') end)`
	if err := v.ExecLua(save, nil, in.Buffer); err != nil {
		return nil, nil, fmt.Errorf("save buffer %d: %w", in.Buffer, err)
	}

	return textResult(fmt.Sprintf("saved buffer %d", in.Buffer)), nil, nil
}

func (self *NvimMCPServer) jump(_ context.Context, _ *mcp.CallToolRequest, in JumpInput) (*mcp.CallToolResult, any, error) {
	v, err := self.dial()
	if err != nil {
		return nil, nil, err
	}
	defer v.Close()

	win, err := v.CurrentWindow()
	if err != nil {
		return nil, nil, fmt.Errorf("current window: %w", err)
	}

	// Record where the cursor is now so <C-t> can return here. The tag stack
	// "from" position is [bufnr, lnum, col, off] with a 1-based column, while
	// nvim_win_get_cursor reports a 0-based column.
	curbuf, err := v.CurrentBuffer()
	if err != nil {
		return nil, nil, fmt.Errorf("current buffer: %w", err)
	}
	cursor, err := v.WindowCursor(win)
	if err != nil {
		return nil, nil, fmt.Errorf("window cursor: %w", err)
	}

	tagname := in.Tagname
	if tagname == "" {
		tagname = fmt.Sprintf("buf%d:%d", in.Buffer, in.Line)
	}
	item := map[string]any{
		"tagname": tagname,
		"from":    []int{int(curbuf), cursor[0], cursor[1] + 1, 0},
	}
	// Action "t" truncates anything above the current entry and pushes ours,
	// leaving curidx one past the top — exactly what a real tag jump does, so
	// <C-t> pops straight back to the recorded position.
	if err := v.Call("settagstack", nil, int(win), map[string]any{"items": []any{item}}, "t"); err != nil {
		return nil, nil, fmt.Errorf("push tag stack: %w", err)
	}

	col := 1
	if in.Column != nil {
		col = *in.Column
	}

	if err := v.SetCurrentBuffer(nvim.Buffer(in.Buffer)); err != nil {
		return nil, nil, fmt.Errorf("activate buffer %d: %w", in.Buffer, err)
	}
	if err := v.SetWindowCursor(win, [2]int{in.Line, col - 1}); err != nil {
		return nil, nil, fmt.Errorf("set cursor: %w", err)
	}

	return textResult(fmt.Sprintf("jumped to buffer %d at %d:%d (<C-t> returns)", in.Buffer, in.Line, col)), nil, nil
}

func (self *NvimMCPServer) Kind() SupportedEditor {
	return Nvim
}

func (self *NvimMCPServer) NewMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "companion for nvim",
		Version: "0.1.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_buffer",
		Description: "Read a neovim buffer, optionally a line range. Output is prefixed with 1-based line numbers (like cat -n). The buffer id is the \"buf\" field reported by the /listen monitor.",
	}, self.readBuffer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "write_buffer",
		Description: "Write content into a neovim buffer. With no line range, replaces the whole buffer; with start/end (1-based, inclusive), replaces only that range — set them equal to insert-replace a single line, or end = start-1 to insert without removing. The buffer id is the \"buf\" field from /listen.",
	}, self.writeBuffer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "glob",
		Description: "List the session's opened (loaded, file-backed) buffers whose path matches a glob pattern. Paths are matched relative to the neovim session's working directory: * and ? stay within a path segment, ** spans segments, and a leading \"**/\" matches zero or more directories (so \"**/*.go\" matches both main.go and src/foo.go). Only opened buffers are considered — it does not walk the filesystem. Use open_buffer first to bring a file into scope.",
	}, self.glob)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "grep",
		Description: "Search the contents of the session's opened (loaded, file-backed) buffers with a relfular expression. Optionally restrict to buffers whose path matches an include glob (relative to the session's working directory). Only opened buffers are searched — it does not walk the filesystem; use open_buffer first to bring a file into scope. Returns path:line:text.",
	}, self.grep)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_buffers",
		Description: "List the neovim session's buffers as \"bufnr<TAB>name\" lines (includes unlisted/unloaded buffers, like \":ls!\"). Use the bufnr with read_buffer/write_buffer.",
	}, self.listBuffers)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "open_buffer",
		Description: "Open a file as a neovim buffer so it can be read/written, without changing the active buffer or window. Path may be relative to the session's working directory. Returns the new buffer id. Use this instead of the Read/Write tools when a file you need isn't already a buffer.",
	}, self.openBuffer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "save_buffer",
		Description: "Save (write to disk) a neovim buffer, like \":write\". Use after write_buffer to persist changes. The buffer id is the \"buf\" field reported by the /listen monitor.",
	}, self.saveBuffer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "jump",
		Description: "Make a buffer the active buffer in the current window and move the cursor to a line (and optional column). Pushes the prior location onto the tag stack first, so the user can press <C-t> to jump back — use this to take the user to a location you found (e.g. \"the function doing X\"). The buffer id is the \"buf\" field from /listen, or from open_buffer/list_buffers.",
	}, self.jump)

	return server
}

type NvimNotifyParams struct {
	Buf  int    `json:"buf"`
	File string `json:"file"`
}

func (self *NvimMCPServer) UnmarshalNotifyJSONParams(data []byte) (any, error) {
	var r NvimNotifyParams
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}

	return r, nil

}
