package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) *project {
	t.Helper()
	t.Setenv("AMEM_HOME", t.TempDir())
	return &project{ID: "demo-1234", Root: "/tmp/demo"}
}

func TestParseEntries(t *testing.T) {
	got := parseEntries("§\na\n§\nb\nline2\r\n§\n\n§\n")
	if len(got) != 2 || got[0] != "a" || got[1] != "b\nline2" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyOps(t *testing.T) {
	out, err := applyOps([]string{"alpha one", "beta two"}, []op{
		{Op: "add", Text: "gamma"},
		{Op: "add", Text: "gamma"},
		{Op: "replace", Old: "beta", Text: "beta 2"},
		{Op: "remove", Old: "alpha"},
	})
	if err != nil || strings.Join(out, "|") != "beta 2|gamma" {
		t.Fatalf("got %q, %v", out, err)
	}
	for _, bad := range [][]op{
		{{Op: "remove", Old: "a"}},
		{{Op: "remove", Old: "zzz"}},
		{{Op: "replace", Old: "alpha", Text: ""}},
		{{Op: "add", Text: "x\n§\ny"}},
		{{Op: "nope"}},
	} {
		if _, err := applyOps([]string{"alpha", "beta"}, bad); err == nil {
			t.Errorf("expected error for %+v", bad)
		}
	}
}

func TestSaveLimitIsAtomic(t *testing.T) {
	p := setup(t)
	os.WriteFile(filepath.Join(home(), "config.json"), []byte(`{"limits":{"user":10}}`), 0o600)
	if _, err := saveBatches([]batch{{"memory", []op{{Op: "add", Text: "kept"}}}}, p); err != nil {
		t.Fatal(err)
	}
	_, err := saveBatches([]batch{
		{"memory", []op{{Op: "add", Text: "not written"}}},
		{"user", []op{{Op: "add", Text: "way more than ten chars"}}},
	}, p)
	if err == nil || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("want limit error, got %v", err)
	}
	s, _ := storeFor("memory", p)
	if got, _ := s.load(); len(got) != 1 {
		t.Fatalf("memory changed despite rejected batch: %q", got)
	}
	// Already over the limit (hand edit): shrinking is allowed, growing is not.
	u, _ := storeFor("user", p)
	os.WriteFile(u.Path, []byte("aaaaaaaaaaaa\n§\nbbbbbbbbbbbb\n"), 0o600)
	if _, err := saveBatches([]batch{{"user", []op{{Op: "remove", Old: "aaa"}}}}, p); err != nil {
		t.Fatalf("shrink rejected: %v", err)
	}
	if _, err := saveBatches([]batch{{"project", nil}}, nil); err == nil {
		t.Fatal("project target without project should fail")
	}
}

func TestSearchAndSnapshot(t *testing.T) {
	p := setup(t)
	writeNote(note{Slug: "vast-ops", Desc: "抢卡运维要点", Body: "RunPod 失败时切换 Vast"})
	appendLog("修好了抢卡脚本的超时", "claude", "", time.Now(), p)
	appendLog("other project work", "codex", "", time.Now(), &project{ID: "x", Root: "/x"})
	saveBatches([]batch{{"user", []op{{Op: "add", Text: "说中文"}}}, {"project", []op{{Op: "add", Text: "build: make"}}}}, p)

	out, _ := search("抢卡", 8)
	if !strings.Contains(out, "2 matches") || !strings.Contains(out, "[note] vast-ops") {
		t.Fatalf("search: %s", out)
	}
	if out, _ := search("姓名 runpod vast", 8); !strings.Contains(out, "1 matches") {
		t.Fatalf("any-term search: %s", out)
	}
	if out, _ := search("抢卡 runpod", 8); !strings.HasPrefix(strings.SplitN(out, "\n\n", 2)[1], "[note]") {
		t.Fatalf("more matched terms should rank first: %s", out)
	}
	snap := snapshot(p)
	for _, want := range []string{"说中文", "build: make", "修好了抢卡", "1 notes, 2 session logs"} {
		if !strings.Contains(snap, want) {
			t.Errorf("snapshot missing %q:\n%s", want, snap)
		}
	}
	if strings.Contains(snap, "other project work") {
		t.Error("snapshot leaked another project's log")
	}
}

func TestParseNoteFrontmatter(t *testing.T) {
	n := parseNote("---\nname: foo_bar\ndescription: \"desc: x\"\nmetadata:\n  type: user\n---\n\nbody\n", "file")
	if n.Slug != "foo_bar" || n.Desc != "desc: x" || n.Body != "body" {
		t.Fatalf("%+v", n)
	}
	if n := parseNote("plain", "file"); n.Slug != "file" || n.Body != "plain" {
		t.Fatalf("%+v", n)
	}
}

func TestMCP(t *testing.T) {
	setup(t)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_save","arguments":{"target":"memory","ops":[{"op":"add","text":"hello"}]}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memory_save","arguments":{"target":"memory","ops":[{"op":"remove","old":"nope"}]}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"bogus"}`,
	}, "\n")
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("want 5 responses, got %d:\n%s", len(lines), out.String())
	}
	var r []map[string]any
	for _, l := range lines {
		var m map[string]any
		json.Unmarshal([]byte(l), &m)
		r = append(r, m)
	}
	if r[0]["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Error("protocol version not echoed")
	}
	if n := len(r[1]["result"].(map[string]any)["tools"].([]any)); n != len(mcpTools) {
		t.Errorf("tools/list returned %d", n)
	}
	if r[2]["result"].(map[string]any)["isError"] != nil {
		t.Errorf("save failed: %v", r[2])
	}
	if r[3]["result"].(map[string]any)["isError"] != true {
		t.Errorf("bad remove should be a tool error: %v", r[3])
	}
	if r[4]["error"] == nil {
		t.Error("unknown method should error")
	}
}

func TestCodexHookHash(t *testing.T) {
	// Same algorithm verified against a fingerprint Codex 0.160 wrote after /hooks approval.
	got := codexHookHash("session_start", map[string]any{"type": "command", "command": "/usr/local/bin/amem hook"})
	if got != "sha256:67b940e8d380d6732d930d36afa146cb05dce9d07ab8e18e0dfd09aacda1db7f" {
		t.Fatalf("hash drifted from Codex: %s", got)
	}
}

func TestTOMLTables(t *testing.T) {
	doc := "model = \"x\"\n\n[hooks.state.'/a/hooks.json:stop:0:0']\ntrusted_hash = \"old\"\nenabled = false\n\n[other]\nk = 1\n"
	doc = setTOMLTable(doc, `hooks.state."/a/hooks.json:stop:0:0"`, [][2]string{{"trusted_hash", `"new"`}, {"enabled", "true"}})
	doc = setTOMLTable(doc, "mcp_servers.amem", [][2]string{{"command", `"amem"`}})
	doc = setTOMLTable(doc, "mcp_servers.amem", [][2]string{{"command", `"amem2"`}})
	want := "model = \"x\"\n\n[hooks.state.'/a/hooks.json:stop:0:0']\ntrusted_hash = \"new\"\nenabled = true\n\n[other]\nk = 1\n\n[mcp_servers.amem]\ncommand = \"amem2\"\n"
	if doc != want {
		t.Fatalf("got:\n%s\nwant:\n%s", doc, want)
	}
	for line, key := range map[string]string{
		`[a."b.c".'d']`:  "a\x00b.c\x00d",
		` [ a . b ] # x`: "a\x00b",
		`[[arr]]`:        "",
		`k = "[x]"`:      "",
	} {
		if got := tomlTableKey(line); got != key {
			t.Errorf("%s: got %q want %q", line, got, key)
		}
	}
}

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	// Never touch the real user's CLIs: no `claude` binary, fake home.
	t.Setenv("PATH", "")
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	claude, codex := filepath.Join(dir, "claude"), filepath.Join(dir, "codex")
	os.MkdirAll(claude, 0o700)
	os.MkdirAll(codex, 0o700)
	os.WriteFile(filepath.Join(claude, "settings.json"), []byte(`{"model":"x","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"other"}]},{"hooks":[{"type":"command","command":"/old/amem hook"}]}]}}`), 0o600)
	os.WriteFile(filepath.Join(codex, "config.toml"), []byte("model = \"gpt\"\n"), 0o600)
	for range 2 {
		if _, err := installClaude(claude, "/bin/amem"); err != nil {
			t.Fatal(err)
		}
		if _, err := installCodex(codex, "/bin/amem"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(claude, "settings.json"))
	s := string(b)
	if strings.Count(s, `"/bin/amem hook"`) != 1 || strings.Count(s, "/bin/amem hook stop --agent claude") != 1 ||
		strings.Contains(s, "/old/amem") || !strings.Contains(s, `"other"`) || !strings.Contains(s, `"model"`) || strings.Count(s, "Bash(amem:*)") != 1 {
		t.Fatalf("claude settings wrong:\n%s", s)
	}
	if baks, _ := filepath.Glob(filepath.Join(claude, "settings.json.bak-amem-*")); len(baks) != 1 {
		t.Fatalf("want 1 backup, got %d", len(baks))
	}
	cfg, _ := os.ReadFile(filepath.Join(codex, "config.toml"))
	hooksPath := filepath.Join(codex, "hooks.json")
	for _, want := range []string{
		`[hooks.state.` + tomlString(hooksPath+":session_start:0:0") + `]`,
		`[hooks.state.` + tomlString(hooksPath+":stop:0:0") + `]`,
		`default_tools_approval_mode = "approve"`,
		`model = "gpt"`,
	} {
		if strings.Count(string(cfg), want) != 1 {
			t.Errorf("config.toml should contain %q once:\n%s", want, cfg)
		}
	}
	if strings.Count(string(cfg), "enabled = true") != 2 {
		t.Errorf("hooks not enabled:\n%s", cfg)
	}
	mcp, _ := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if !strings.Contains(string(mcp), `"command": "/bin/amem"`) || !strings.Contains(s, `"mcp__amem"`) {
		t.Errorf("claude MCP server not registered:\n%s", mcp)
	}
}

func TestUninstall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", "")
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	t.Setenv("AMEM_HOME", filepath.Join(dir, "data"))
	os.MkdirAll(filepath.Join(dir, "claude"), 0o700)
	os.MkdirAll(filepath.Join(dir, "codex"), 0o700)
	settings := `{"permissions":{"allow":["Bash(ls:*)"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"other"}]}]}}`
	os.WriteFile(filepath.Join(dir, "claude", "settings.json"), []byte(settings), 0o600)
	os.WriteFile(filepath.Join(dir, "codex", "config.toml"), []byte("model = \"gpt\"\n\n[other]\nk = 1\n"), 0o600)
	if _, err := installClaude(configDir("claude"), "/bin/amem"); err != nil {
		t.Fatal(err)
	}
	if _, err := installCodex(configDir("codex"), "/bin/amem"); err != nil {
		t.Fatal(err)
	}
	if _, err := uninstall(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "claude", "settings.json"))
	if strings.Contains(string(b), "amem") || !strings.Contains(string(b), `"other"`) || !strings.Contains(string(b), "Bash(ls:*)") {
		t.Fatalf("claude settings after uninstall:\n%s", b)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "codex", "config.toml"))
	if strings.Contains(string(cfg), "amem") || strings.Contains(string(cfg), "hooks.state") || !strings.Contains(string(cfg), "[other]") {
		t.Fatalf("codex config after uninstall:\n%s", cfg)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "claude", ".claude.json")); strings.Contains(string(b), "amem") {
		t.Fatalf("claude MCP server left behind:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude", "commands", "save.md")); err == nil {
		t.Fatal("/save command left behind")
	}
}

func TestStopHook(t *testing.T) {
	setup(t)
	os.WriteFile(filepath.Join(home(), "config.json"), []byte(`{"review_every":3}`), 0o600)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	turn := func(transcript string, active bool) bool {
		f, _ := os.OpenFile(tr, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		f.WriteString(transcript + "\n")
		f.Close()
		in, _ := json.Marshal(hookInput{SessionID: "s1", TranscriptPath: tr, StopHookActive: active})
		out, err := stopHook("claude", bytes.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(out, `"decision":"block"`)
	}
	if turn(`{"text":"hi"}`, false) || turn(`{"text":"chat"}`, false) {
		t.Fatal("no review expected before N turns without edits")
	}
	if !turn(`{"text":"chat"}`, false) {
		t.Fatal("review expected every 3 turns")
	}
	if turn(`{"text":"saved"}`, true) {
		t.Fatal("must not block while the review turn stops")
	}
	if !turn(`{"type":"tool_use","name":"Edit"}`, false) {
		t.Fatal("review expected right after an edit")
	}
	turn(`{}`, true)
	if turn(`{"text":"plain"}`, false) {
		t.Fatal("old edits must not retrigger")
	}
}

func TestStopHookSeesShellEdits(t *testing.T) {
	setup(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git unavailable: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("pre-existing"), 0o600)
	in := hookInput{SessionID: "g1", Cwd: repo}
	recordBaseline(in)
	stop := func() bool {
		b, _ := json.Marshal(in)
		out, err := stopHook("codex", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(out, `"decision":"block"`)
	}
	if stop() {
		t.Fatal("pre-existing dirty tree must not count as an edit")
	}
	os.WriteFile(filepath.Join(repo, "new.txt"), []byte("hi"), 0o600)
	if !stop() {
		t.Fatal("a file written outside any edit tool must trigger a review")
	}
	in.StopHookActive = true
	stop()
	in.StopHookActive = false
	if stop() {
		t.Fatal("unchanged tree after review must not retrigger")
	}
	os.WriteFile(filepath.Join(repo, "new.txt"), []byte("hello"), 0o600)
	if !stop() {
		t.Fatal("modifying an already-dirty file must trigger")
	}
}

func TestSessionLogReplaces(t *testing.T) {
	p := setup(t)
	appendLog("first", "claude", "s1", time.Now(), p)
	appendLog("other", "claude", "s2", time.Now(), p)
	appendLog("second", "claude", "s1", time.Now(), p)
	logs, _ := readLogs()
	if len(logs) != 2 || logs[1].Text != "second" || logs[0].Text != "other" {
		t.Fatalf("%+v", logs)
	}
}
