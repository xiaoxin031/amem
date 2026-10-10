package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// version is set by release builds (-X main.version=...); `go install` builds
// fall back to the module version.
var version = "dev"

func init() {
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
}

const usage = `amem — one small, bounded memory for every AI coding CLI

  amem inject                     print the session-start snapshot
  amem hook                       SessionStart hook: inject the snapshot (Claude Code / Codex)
  amem hook stop --agent A        Stop hook: ask the agent to auto-save after edits / every N turns
  amem save [file]                apply JSON batches from file or stdin (see: amem help save)
  amem add <target> <text>        add one entry   (target: user|memory|project)
  amem rm <target> <substring>    remove the entry containing substring
  amem ls [target]                list entries with usage
  amem log [--agent A] [--session ID] [text]
                                  append a session summary (same session replaces)
  amem note <slug> [--desc D]     save a reference note (body from stdin)
  amem show <slug>                print a note
  amem search <words> [-n N]      search notes and session logs
  amem import <file|dir>...       import markdown files as notes
  amem mcp                        run as an MCP server (stdio)
  amem install [claude|codex]     wire hooks + MCP + /save into that CLI, or every one found (backs up first)
  amem uninstall                  remove all of that again (memory data is kept)

Data lives in $AMEM_HOME (default ~/.amem).`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	out, err := run(os.Args[1], os.Args[2:])
	if out != "" {
		fmt.Println(strings.TrimRight(out, "\n"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amem:", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) (string, error) {
	p := detectProject(cwd())
	switch cmd {
	case "inject":
		return snapshot(p), nil
	case "hook":
		// Hooks must never break the host CLI: report problems on stderr, exit 0.
		var out string
		var err error
		if len(args) > 0 && args[0] == "stop" {
			agent, _ := flagVal(args[1:], "--agent")
			out, err = stopHook(agent, os.Stdin)
		} else {
			out, err = hook()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "amem:", err)
		}
		return out, nil
	case "save":
		raw, err := input(args)
		if err != nil {
			return "", err
		}
		batches, err := parseBatches(raw)
		if err != nil {
			return "", err
		}
		return saveBatches(batches, p)
	case "add", "rm":
		if len(args) < 2 {
			return "", fmt.Errorf("usage: amem %s <target> <text>", cmd)
		}
		o := op{Op: "add", Text: strings.Join(args[1:], " ")}
		if cmd == "rm" {
			o = op{Op: "remove", Old: o.Text}
		}
		return saveBatches([]batch{{Target: args[0], Ops: []op{o}}}, p)
	case "ls":
		return list(args, p)
	case "log":
		agent, args := flagVal(args, "--agent")
		session, args := flagVal(args, "--session")
		date, args := flagVal(args, "--date")
		ts := time.Now()
		if date != "" {
			t, err := time.ParseInLocation("2006-01-02", date, time.Local)
			if err != nil {
				return "", fmt.Errorf("--date wants YYYY-MM-DD: %w", err)
			}
			ts = t
		}
		text, err := textArg(args)
		if err != nil {
			return "", err
		}
		return appendLog(text, agent, session, ts, p)
	case "note":
		desc, args := flagVal(args, "--desc")
		if len(args) != 1 {
			return "", errors.New("usage: amem note <slug> [--desc D] < body")
		}
		body, err := input(nil)
		if err != nil {
			return "", err
		}
		return writeNote(note{Slug: args[0], Desc: desc, Body: body})
	case "show":
		if len(args) != 1 {
			return "", errors.New("usage: amem show <slug>")
		}
		return showNote(args[0])
	case "search":
		n, args := flagVal(args, "-n")
		limit, _ := strconv.Atoi(n)
		if limit <= 0 {
			limit = 8
		}
		return search(strings.Join(args, " "), limit)
	case "import":
		return importNotes(args)
	case "mcp":
		return "", serveMCP(os.Stdin, os.Stdout)
	case "install":
		if len(args) > 1 {
			return "", errors.New("usage: amem install [claude|codex]")
		}
		if len(args) == 1 {
			return install(args[0])
		}
		return installDetected()
	case "uninstall":
		return uninstall()
	case "help", "-h", "--help":
		if len(args) > 0 && args[0] == "save" {
			return saveGuide(""), nil
		}
		return usage, nil
	case "version", "--version":
		return version, nil
	}
	return "", fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
}

func parseBatches(raw string) ([]batch, error) {
	raw = strings.TrimSpace(raw)
	var batches []batch
	if strings.HasPrefix(raw, "{") {
		var b batch
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		batches = []batch{b}
	} else if err := json.Unmarshal([]byte(raw), &batches); err != nil {
		return nil, fmt.Errorf("invalid JSON (want [{\"target\":...,\"ops\":[...]}]): %w", err)
	}
	return batches, nil
}

func list(args []string, p *project) (string, error) {
	targets := []string{"user", "memory", "project"}
	if len(args) > 0 {
		targets = args[:1]
	}
	var b strings.Builder
	for _, t := range targets {
		s, err := storeFor(t, p)
		if err != nil {
			if len(args) > 0 {
				return "", err
			}
			continue
		}
		entries, err := s.load()
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s  %d/%d chars  %s\n%s\n", t, size(entries), s.Limit, s.Path, listing(entries))
	}
	return b.String(), nil
}

func importNotes(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("usage: amem import <file|dir> [more]")
	}
	var files []string
	for _, p := range paths {
		if st, err := os.Stat(p); err != nil {
			return "", err
		} else if st.IsDir() {
			md, _ := filepath.Glob(filepath.Join(p, "*.md"))
			for _, f := range md {
				if filepath.Base(f) != "MEMORY.md" {
					files = append(files, f)
				}
			}
		} else {
			files = append(files, p)
		}
	}
	var done []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		n := parseNote(string(b), strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)))
		if _, err := writeNote(n); err != nil {
			return "", fmt.Errorf("%s: %w", f, err)
		}
		done = append(done, n.Slug)
	}
	return fmt.Sprintf("imported %d notes: %s", len(done), strings.Join(done, ", ")), nil
}

func flagVal(args []string, name string) (string, []string) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], append(args[:i:i], args[i+2:]...)
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, append(args[:i:i], args[i+1:]...)
		}
	}
	return "", args
}

func textArg(args []string) (string, error) {
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}
	return input(nil)
}

// input reads a file argument, or stdin when it is piped.
func input(args []string) (string, error) {
	if len(args) > 0 && args[0] != "-" {
		b, err := os.ReadFile(args[0])
		return string(b), err
	}
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		return "", errors.New("expected input on stdin")
	}
	b, err := io.ReadAll(os.Stdin)
	return string(b), err
}

// hook emits SessionStart additionalContext, the format shared by Claude Code and Codex.
func hook() (string, error) {
	dir := cwd()
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		var in hookInput
		if b, err := io.ReadAll(os.Stdin); err == nil && json.Unmarshal(b, &in) == nil {
			if in.Cwd != "" {
				dir = in.Cwd
			} else {
				in.Cwd = dir
			}
			if err := recordBaseline(in); err != nil {
				fmt.Fprintln(os.Stderr, "amem:", err)
			}
		}
	}
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "SessionStart",
		"additionalContext": snapshot(detectProject(dir)),
	}})
	return string(out), err
}
