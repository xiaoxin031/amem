package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func schema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var str = map[string]any{"type": "string"}

var cwdProp = map[string]any{"type": "string", "description": "Project directory; defaults to the server's working directory."}

var mcpTools = []map[string]any{
	{"name": "memory_load", "description": "Return the memory snapshot (user profile, global and project memory, recent sessions). Call at session start if it is not already in context.",
		"inputSchema": schema(map[string]any{"cwd": cwdProp})},
	{"name": "memory_save", "description": "Update size-capped memory that is auto-loaded every session. Apply ALL changes for a target in one call; ops are atomic and the limit is checked on the result. " +
		"Targets: user (who the user is, preferences, corrections), memory (cross-project facts), project (this repo). Keep entries terse and durable; replace stale entries rather than adding. " +
		"If over the limit, nothing is saved and current entries are returned: merge/shorten/remove in the same call.",
		"inputSchema": schema(map[string]any{
			"target": map[string]any{"type": "string", "enum": []string{"user", "memory", "project"}},
			"ops": map[string]any{"type": "array", "items": schema(map[string]any{
				"op":   map[string]any{"type": "string", "enum": []string{"add", "replace", "remove"}},
				"text": map[string]any{"type": "string", "description": "Entry text for add/replace."},
				"old":  map[string]any{"type": "string", "description": "Unique substring of the entry to replace/remove."},
			}, "op")},
			"cwd": cwdProp,
		}, "target", "ops")},
	{"name": "memory_log", "description": "Append a 3-8 line session summary (what was done, files changed, decisions and why, TODOs) to searchable history. Use when the user asks to save the session.",
		"inputSchema": schema(map[string]any{"summary": str, "agent": str, "cwd": cwdProp,
			"session": map[string]any{"type": "string", "description": "Session id; a later summary with the same id replaces the earlier one."}}, "summary")},
	{"name": "memory_search", "description": "Search notes and past session logs (case-insensitive substrings; any word may match, more matched words rank higher). Use before re-exploring or asking the user something they may have told you before.",
		"inputSchema": schema(map[string]any{"query": str, "limit": map[string]any{"type": "integer"}}, "query")},
	{"name": "memory_show", "description": "Read a full note by slug.", "inputSchema": schema(map[string]any{"slug": str}, "slug")},
	{"name": "memory_note", "description": "Create or overwrite a long reference note (runbook, inventory). Searchable, not auto-loaded.",
		"inputSchema": schema(map[string]any{"slug": str, "description": str, "body": str}, "slug", "description", "body")},
}

func serveMCP(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		var m rpcMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
			continue
		}
		if len(m.ID) == 0 || m.Method == "" {
			continue
		}
		result, rpcErr := dispatch(m)
		resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func dispatch(m rpcMsg) (any, map[string]any) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "amem", "version": version},
			"instructions":    "amem: persistent memory shared by the user's AI tools. If no <amem> snapshot is in your context, call memory_load first. Search it before asking the user something they may have said before; save durable facts with memory_save.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, map[string]any{"code": -32602, "message": err.Error()}
		}
		text, err := callTool(p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil
	}
	return nil, map[string]any{"code": -32601, "message": "method not found: " + m.Method}
}

func callTool(name string, raw json.RawMessage) (string, error) {
	var a struct {
		Cwd, Target, Summary, Agent, Session, Query, Slug, Description, Body string
		Ops                                                                  []op
		Limit                                                                int
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
	}
	dir := a.Cwd
	if strings.TrimSpace(dir) == "" {
		dir = cwd()
	}
	p := detectProject(dir)
	switch name {
	case "memory_load":
		return snapshot(p), nil
	case "memory_save":
		return saveBatches([]batch{{Target: a.Target, Ops: a.Ops}}, p)
	case "memory_log":
		return appendLog(a.Summary, a.Agent, a.Session, time.Now(), p)
	case "memory_search":
		if a.Limit <= 0 {
			a.Limit = 8
		}
		return search(a.Query, a.Limit)
	case "memory_show":
		return showNote(a.Slug)
	case "memory_note":
		return writeNote(note{Slug: a.Slug, Desc: a.Description, Body: a.Body})
	}
	return "", fmt.Errorf("unknown tool %q", name)
}
