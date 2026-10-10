package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const hookTimeout = 10

var installers = map[string]func(dir, exe string) ([]string, error){
	"claude": installClaude,
	"codex":  installCodex,
}

func configDir(target string) string {
	dir := os.Getenv(map[string]string{"claude": "CLAUDE_CONFIG_DIR", "codex": "CODEX_HOME"}[target])
	if dir == "" {
		h, _ := os.UserHomeDir()
		dir = filepath.Join(h, "."+target)
	}
	// Codex keys hook trust by the canonical hooks.json path.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

func install(target string) (string, error) {
	fn, ok := installers[target]
	if !ok {
		return "", fmt.Errorf("unknown target %q (want claude|codex; other tools: run `amem mcp` as an MCP server)", target)
	}
	exe, err := selfPath()
	if err != nil {
		return "", err
	}
	steps, err := fn(configDir(target), exe)
	return strings.Join(steps, "\n"), err
}

// installDetected wires every supported CLI whose config dir exists.
func installDetected() (string, error) {
	var out []string
	for _, t := range []string{"claude", "codex"} {
		if _, err := os.Stat(configDir(t)); err != nil {
			continue
		}
		msg, err := install(t)
		if err != nil {
			return strings.Join(out, "\n"), fmt.Errorf("%s: %w", t, err)
		}
		out = append(out, msg)
	}
	if len(out) == 0 {
		return "", errors.New("found no Claude Code (~/.claude) or Codex (~/.codex) config; install Claude Code or Codex first, or run `amem mcp` as an MCP server for other tools")
	}
	return strings.Join(out, "\n") + "\n\nDone. New sessions load memory and save on their own.", nil
}

func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// cli is how agents should invoke amem: the bare name when it resolves to this
// binary on PATH, otherwise the absolute path.
func cli() string {
	exe, err := selfPath()
	if err != nil {
		return "amem"
	}
	if p, err := exec.LookPath("amem"); err == nil {
		if r, err := filepath.EvalSymlinks(p); err == nil && r == exe {
			return "amem"
		}
	}
	return shellQuote(exe)
}

// shellQuote makes a path safe for sh, cmd and PowerShell alike. Forward
// slashes keep Windows paths intact under Git Bash.
func shellQuote(s string) string {
	s = filepath.ToSlash(s)
	if strings.ContainsAny(s, " '\"\t&;()$`") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func hookCommands(exe, agent string) map[string]string {
	q := shellQuote(exe)
	return map[string]string{
		"SessionStart": q + " hook",
		"Stop":         q + " hook stop --agent " + agent,
	}
}

func installClaude(dir, exe string) ([]string, error) {
	path := filepath.Join(dir, "settings.json")
	root, err := readJSON(path)
	if err != nil {
		return nil, err
	}
	for event, cmd := range hookCommands(exe, "claude") {
		mergeHook(root, event, map[string]any{"type": "command", "command": cmd, "timeout": hookTimeout})
	}
	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	allow, _ := perms["allow"].([]any)
	for _, rule := range []string{"mcp__amem", "Bash(amem:*)", "Bash(" + shellQuote(exe) + ":*)"} {
		if !slices.Contains(allow, any(rule)) {
			allow = append(allow, rule)
		}
	}
	perms["allow"] = allow
	root["permissions"] = perms
	msg, err := writeJSON(path, root)
	if err != nil {
		return nil, err
	}
	steps := []string{"claude: " + msg}
	msg, err = addClaudeMCP(dir, exe)
	if err != nil {
		return steps, err
	}
	steps = append(steps, "claude: "+msg)
	cmd := "---\ndescription: Save this session to amem memory\n---\n\n" + saveGuide("claude")
	msg, err = writeWithBackup(filepath.Join(dir, "commands", "save.md"), cmd)
	if err != nil {
		return steps, err
	}
	return append(steps, "claude: "+msg+" (/save)"), nil
}

func installCodex(dir, exe string) ([]string, error) {
	hooksPath := filepath.Join(dir, "hooks.json")
	root, err := readJSON(hooksPath)
	if err != nil {
		return nil, err
	}
	type trust struct{ key, hash string }
	var trusts []trust
	for event, cmd := range hookCommands(exe, "codex") {
		handler := map[string]any{"type": "command", "command": cmd, "timeout": hookTimeout}
		if event == "SessionStart" {
			// Never spill the snapshot to a side file: the agent must see it.
			handler["additionalContextLimit"] = 0
		}
		gi, hi := mergeHook(root, event, handler)
		label := map[string]string{"SessionStart": "session_start", "Stop": "stop"}[event]
		trusts = append(trusts, trust{
			key:  fmt.Sprintf("%s:%s:%d:%d", hooksPath, label, gi, hi),
			hash: codexHookHash(label, handler),
		})
	}
	msg, err := writeJSON(hooksPath, root)
	if err != nil {
		return nil, err
	}
	steps := []string{"codex: " + msg}

	cfgPath := filepath.Join(dir, "config.toml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return steps, err
	}
	toml := string(cfg)
	slices.SortFunc(trusts, func(a, b trust) int { return strings.Compare(a.key, b.key) })
	for _, t := range trusts {
		toml = setTOMLTable(toml, `hooks.state.`+tomlString(t.key), [][2]string{
			{"trusted_hash", tomlString(t.hash)},
			{"enabled", "true"},
		})
	}
	server := [][2]string{
		{"command", tomlString(exe)},
		{"args", `["mcp"]`},
		{"default_tools_approval_mode", `"approve"`},
	}
	// Codex starts MCP servers with a filtered environment.
	if v := os.Getenv("AMEM_HOME"); v != "" {
		server = append(server, [2]string{"env", "{ AMEM_HOME = " + tomlString(v) + " }"})
	}
	toml = setTOMLTable(toml, "mcp_servers.amem", server)
	msg, err = writeWithBackup(cfgPath, toml)
	if err != nil {
		return steps, err
	}
	steps = append(steps, "codex: "+msg+" (hooks trusted, MCP tools auto-approved)")

	skill := "---\nname: save\ndescription: Save this session to amem memory. Use when the user says save, /save, or asks to remember something.\n---\n\n" + saveGuide("codex")
	msg, err = writeWithBackup(filepath.Join(dir, "skills", "save", "SKILL.md"), skill)
	if err != nil {
		return steps, err
	}
	return append(steps, "codex: "+msg+" ($save)"), nil
}

// addClaudeMCP registers the amem MCP server at user scope, through the
// official CLI when present, else by editing .claude.json directly.
func addClaudeMCP(dir, exe string) (string, error) {
	env := map[string]any{}
	if v := os.Getenv("AMEM_HOME"); v != "" {
		env["AMEM_HOME"] = v
	}
	if bin, err := exec.LookPath("claude"); err == nil {
		exec.Command(bin, "mcp", "remove", "--scope", "user", "amem").Run()
		// -e is variadic, so the server name must come before it.
		args := []string{"mcp", "add", "--scope", "user", "amem"}
		for k, v := range env {
			args = append(args, "-e", k+"="+v.(string))
		}
		args = append(args, "--", exe, "mcp")
		if out, err := exec.Command(bin, args...).CombinedOutput(); err == nil {
			return "registered MCP server amem (user scope, tools pre-approved)", nil
		} else if len(out) > 0 {
			fmt.Fprintf(os.Stderr, "amem: claude mcp add failed, editing config directly: %s", out)
		}
	}
	path := claudeJSON(dir)
	root, err := readJSON(path)
	if err != nil {
		return "", err
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["amem"] = map[string]any{"type": "stdio", "command": exe, "args": []any{"mcp"}, "env": env}
	root["mcpServers"] = servers
	msg, err := writeJSON(path, root)
	return msg + " (MCP server amem)", err
}

// uninstall removes everything install added. Memory data in AMEM_HOME stays.
func uninstall() (string, error) {
	var out []string
	note := func(msg string, err error) error {
		if err == nil && msg != "" {
			out = append(out, msg)
		}
		return err
	}

	claude := configDir("claude")
	if _, err := os.Stat(claude); err == nil {
		path := filepath.Join(claude, "settings.json")
		root, err := readJSON(path)
		if err != nil {
			return "", err
		}
		removeHooks(root)
		if perms, _ := root["permissions"].(map[string]any); perms != nil {
			allow, _ := perms["allow"].([]any)
			perms["allow"] = slices.DeleteFunc(allow, func(r any) bool {
				s, _ := r.(string)
				return s == "mcp__amem" || strings.HasPrefix(s, "Bash(amem:") || strings.Contains(s, "/amem:")
			})
		}
		if err := note(writeJSON(path, root)); err != nil {
			return "", err
		}
		if bin, err := exec.LookPath("claude"); err == nil && exec.Command(bin, "mcp", "remove", "--scope", "user", "amem").Run() == nil {
			out = append(out, "removed Claude MCP server amem")
		} else if path := claudeJSON(claude); fileExists(path) {
			root, err := readJSON(path)
			if err != nil {
				return "", err
			}
			if servers, _ := root["mcpServers"].(map[string]any); servers != nil && servers["amem"] != nil {
				delete(servers, "amem")
				if err := note(writeJSON(path, root)); err != nil {
					return "", err
				}
			}
		}
		out = append(out, removeIfOurs(filepath.Join(claude, "commands", "save.md")))
	}

	codex := configDir("codex")
	if _, err := os.Stat(codex); err == nil {
		hooksPath := filepath.Join(codex, "hooks.json")
		root, err := readJSON(hooksPath)
		if err != nil {
			return "", err
		}
		removeHooks(root)
		if err := note(writeJSON(hooksPath, root)); err != nil {
			return "", err
		}
		cfgPath := filepath.Join(codex, "config.toml")
		if b, err := os.ReadFile(cfgPath); err == nil {
			doc := removeTOMLTable(string(b), "mcp_servers.amem")
			for _, ev := range []string{"session_start", "stop"} {
				doc = removeTOMLTablesWhere(doc, func(key []string) bool {
					return len(key) == 3 && key[0] == "hooks" && key[1] == "state" &&
						strings.HasPrefix(key[2], hooksPath+":"+ev+":") && !hookExists(root, ev, key[2])
				})
			}
			if err := note(writeWithBackup(cfgPath, doc)); err != nil {
				return "", err
			}
		}
		out = append(out, removeIfOurs(filepath.Join(codex, "skills", "save", "SKILL.md")))
	}
	return strings.Join(slices.DeleteFunc(out, func(s string) bool { return s == "" }), "\n") +
		"\n\namem is unhooked. Your memory is still in " + home() + " (delete it yourself if you want).", nil
}

func removeHooks(root map[string]any) {
	hooks, _ := root["hooks"].(map[string]any)
	for event, v := range hooks {
		groups, _ := v.([]any)
		var kept []any
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			hs = slices.DeleteFunc(hs, func(x any) bool {
				hm, _ := x.(map[string]any)
				return hm != nil && isAmemHook(hm)
			})
			if len(hs) > 0 || gm == nil {
				if gm != nil {
					gm["hooks"] = hs
				}
				kept = append(kept, g)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
}

// hookExists reports whether a non-amem handler still occupies a trust key, so
// uninstall leaves state for other hooks alone.
func hookExists(root map[string]any, eventLabel, key string) bool {
	event := map[string]string{"session_start": "SessionStart", "stop": "Stop"}[eventLabel]
	hooks, _ := root["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	for gi, g := range groups {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for hi := range hs {
			if strings.HasSuffix(key, fmt.Sprintf(":%s:%d:%d", eventLabel, gi, hi)) {
				return true
			}
		}
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func removeIfOurs(path string) string {
	if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), "amem") {
		if os.Remove(path) == nil {
			os.Remove(filepath.Dir(path))
			return "removed " + path
		}
	}
	return ""
}

func removeTOMLTable(doc, header string) string {
	want := tomlTableKey("[" + header + "]")
	return removeTOMLTablesWhere(doc, func(key []string) bool { return strings.Join(key, "\x00") == want })
}

func removeTOMLTablesWhere(doc string, match func(key []string) bool) string {
	lines := strings.SplitAfter(doc, "\n")
	var out []string
	skip := false
	for _, l := range lines {
		if k := tomlTableKey(l); k != "" {
			skip = match(strings.Split(k, "\x00"))
		}
		if !skip {
			out = append(out, l)
		}
	}
	return strings.TrimRight(strings.Join(out, ""), "\n") + "\n"
}

// claudeJSON is where Claude Code keeps user-scope MCP servers.
func claudeJSON(dir string) string {
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(filepath.Dir(dir), ".claude.json")
}

// codexHookHash reproduces Codex's hook trust fingerprint: sha256 over the
// key-sorted compact JSON of {event_name, hooks:[normalized handler]}.
func codexHookHash(eventLabel string, handler map[string]any) string {
	h := map[string]any{"async": false, "timeout": 600}
	for k, v := range handler {
		h[k] = v
	}
	if h["additionalContextLimit"] == 2500 {
		delete(h, "additionalContextLimit")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]any{"event_name": eventLabel, "hooks": []any{h}})
	sum := sha256.Sum256(bytes.TrimRight(buf.Bytes(), "\n"))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func readJSON(path string) (map[string]any, error) {
	root := map[string]any{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(bytes.TrimSpace(b)) == 0) {
		return root, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return root, nil
}

func writeJSON(path string, root map[string]any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return "", err
	}
	return writeWithBackup(path, buf.String())
}

// mergeHook puts handler into root.hooks[event], replacing an earlier amem
// handler in place or appending a new group. It returns the handler's indexes.
func mergeHook(root map[string]any, event string, handler map[string]any) (int, int) {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	groups, _ := hooks[event].([]any)
	for gi, g := range groups {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for hi, x := range hs {
			if hm, _ := x.(map[string]any); hm != nil && isAmemHook(hm) {
				hs[hi] = handler
				return gi, hi
			}
		}
	}
	hooks[event] = append(groups, map[string]any{"hooks": []any{handler}})
	return len(groups), 0
}

func isAmemHook(h map[string]any) bool {
	c, _ := h["command"].(string)
	f := strings.Fields(strings.ReplaceAll(c, `"`, " "))
	return len(f) > 1 && strings.TrimSuffix(filepath.Base(f[0]), ".exe") == "amem" && slices.Contains(f, "hook")
}

func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// setTOMLTable makes [header] contain exactly kv, replacing an existing table
// body (up to the next table header) or appending a new table.
func setTOMLTable(doc, header string, kv [][2]string) string {
	var body strings.Builder
	for _, p := range kv {
		body.WriteString(p[0] + " = " + p[1] + "\n")
	}
	want := tomlTableKey("[" + header + "]")
	lines := strings.SplitAfter(doc, "\n")
	for i, l := range lines {
		if k := tomlTableKey(l); k == "" || k != want {
			continue
		}
		end := i + 1
		for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "[") {
			end++
		}
		tail := ""
		if end < len(lines) {
			tail = "\n"
		}
		return strings.Join(lines[:i+1], "") + body.String() + tail + strings.Join(lines[end:], "")
	}
	if doc != "" && !strings.HasSuffix(doc, "\n") {
		doc += "\n"
	}
	if doc != "" {
		doc += "\n"
	}
	return doc + "[" + header + "]\n" + body.String()
}

// tomlTableKey canonicalizes a "[a.\"b\".'c']" header line to its key path so
// differently quoted spellings of the same table compare equal. "" if not a header.
func tomlTableKey(line string) string {
	s := strings.TrimSpace(line)
	if i := strings.LastIndex(s, "]"); strings.HasPrefix(s, "[") && !strings.HasPrefix(s, "[[") && i > 0 {
		rest := strings.TrimSpace(s[i+1:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return ""
		}
		s = s[1:i]
	} else {
		return ""
	}
	var parts []string
	for s = strings.TrimSpace(s); s != ""; {
		var part string
		switch s[0] {
		case '"':
			end := 1
			for end < len(s) && (s[end] != '"' || s[end-1] == '\\') {
				end++
			}
			if end >= len(s) || json.Unmarshal([]byte(s[:end+1]), &part) != nil {
				return ""
			}
			s = s[end+1:]
		case '\'':
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return ""
			}
			part, s = s[1:end+1], s[end+2:]
		default:
			end := strings.IndexByte(s, '.')
			if end < 0 {
				end = len(s)
			}
			part, s = strings.TrimSpace(s[:end]), s[end:]
		}
		parts = append(parts, part)
		s = strings.TrimSpace(s)
		if s != "" {
			if s[0] != '.' {
				return ""
			}
			s = strings.TrimSpace(s[1:])
		}
	}
	return strings.Join(parts, "\x00")
}

func writeWithBackup(path, content string) (string, error) {
	old, err := os.ReadFile(path)
	switch {
	case err == nil && string(old) == content:
		return "unchanged " + path, nil
	case err == nil:
		bak := path + ".bak-amem-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(bak, old, 0o600); err != nil {
			return "", err
		}
		if err := writeAtomic(path, content); err != nil {
			return "", err
		}
		return "updated " + path + " (backup " + filepath.Base(bak) + ")", nil
	case errors.Is(err, os.ErrNotExist):
		return "created " + path, writeAtomic(path, content)
	}
	return "", err
}
