package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Claude Code edit tools, and Codex apply_patch payloads.
var editRe = regexp.MustCompile(`"name":\s*"(Edit|Write|MultiEdit|NotebookEdit)"|\*\*\* Begin Patch`)

const reviewIntro = `[amem auto-save] Before stopping, review the conversation since the last auto-save. Keep only what a future session would need: user preferences or corrections, decisions and why, gotchas, project conventions, environment facts. Skip anything trivial or easy to rediscover.
`

const reviewSteps = `- If there are durable facts, call the amem tool memory_save once per target with cwd=%[3]q (terse, one fact per entry; replace or merge stale entries instead of piling up).
- Always call memory_log with session=%[1]q, agent=%[2]q, cwd=%[3]q and a 3-6 line summary of the whole session (what was done, files changed, decisions, open TODOs); it replaces the previous one.
(If the amem tools are missing, run ` + "`%[4]s help save`" + ` and use the CLI.)
`

const reviewOutro = `Write in the user's language. Do not repeat your previous answer; finish with one short line naming what was remembered.`

func reviewPrompt(agent, session, cwd string) string {
	return reviewIntro + fmt.Sprintf(reviewSteps, session, agent, cwd, cli()) + reviewOutro
}

type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	StopHookActive bool   `json:"stop_hook_active"`
}

type reviewState struct {
	Offset int64  `json:"offset"`
	Turns  int    `json:"turns"`
	Edited bool   `json:"edited"`
	Tree   string `json:"tree,omitempty"`
}

func statePath(session string) string {
	return filepath.Join(home(), "state", sanitize(session)+".json")
}

func loadState(session string) (reviewState, bool) {
	var st reviewState
	b, err := os.ReadFile(statePath(session))
	if err != nil {
		pruneState(filepath.Dir(statePath(session)))
		return st, false
	}
	return st, json.Unmarshal(b, &st) == nil
}

func saveState(session string, st reviewState) error {
	b, _ := json.Marshal(st)
	return writeAtomic(statePath(session), string(b))
}

// recordBaseline remembers the working tree at session start so the first
// Stop can tell whether this session changed files. Resumes keep the old one.
func recordBaseline(in hookInput) error {
	if in.SessionID == "" {
		return nil
	}
	if _, ok := loadState(in.SessionID); ok {
		return nil
	}
	return saveState(in.SessionID, reviewState{Tree: treeFingerprint(in.Cwd)})
}

// treeFingerprint hashes `git status` plus size/mtime of every changed file, so
// any edit (editor tool, patch, sed, redirect, script) changes it. "" outside git.
func treeFingerprint(dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	top := dir
	if b, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		top = strings.TrimSpace(string(b))
	}
	h := sha256.New()
	h.Write(out)
	for _, rec := range strings.Split(string(out), "\x00") {
		if len(rec) > 3 {
			if fi, err := os.Stat(filepath.Join(top, rec[3:])); err == nil {
				fmt.Fprintf(h, "%s %d %d\n", rec[3:], fi.Size(), fi.ModTime().UnixNano())
			}
		}
	}
	return "git:" + hex.EncodeToString(h.Sum(nil))
}

func reviewEvery() int {
	var cfg struct {
		ReviewEvery *int `json:"review_every"`
	}
	if b, err := os.ReadFile(filepath.Join(home(), "config.json")); err == nil && json.Unmarshal(b, &cfg) == nil && cfg.ReviewEvery != nil {
		return *cfg.ReviewEvery
	}
	return 8
}

// stopHook decides after each turn whether the agent should save memory:
// right after a turn that edited files, or every N turns otherwise.
// The review runs inside the live session, so it reuses its prompt cache.
func stopHook(agent string, r io.Reader) (string, error) {
	var in hookInput
	if b, err := io.ReadAll(r); err != nil || json.Unmarshal(b, &in) != nil || in.SessionID == "" {
		return "", nil
	}
	every := reviewEvery()
	if every < 0 {
		return "", nil
	}
	if in.Cwd == "" {
		in.Cwd = cwd()
	}
	st, known := loadState(in.SessionID)
	if tree := treeFingerprint(in.Cwd); tree != st.Tree {
		st.Edited = st.Edited || (known && tree != "")
		st.Tree = tree
	}

	var size int64
	if in.TranscriptPath != "" {
		if f, err := os.Open(in.TranscriptPath); err == nil {
			if fi, err := f.Stat(); err == nil {
				size = fi.Size()
				if size > st.Offset {
					f.Seek(st.Offset, io.SeekStart)
					if b, err := io.ReadAll(f); err == nil && editRe.Match(b) {
						st.Edited = true
					}
				}
			}
			f.Close()
		}
	}
	st.Offset = max(st.Offset, size)

	out := ""
	switch {
	case in.StopHookActive:
		st.Turns, st.Edited = 0, false
	default:
		st.Turns++
		if st.Edited || (every > 0 && st.Turns >= every) {
			st.Turns, st.Edited = 0, false
			b, _ := json.Marshal(map[string]string{"decision": "block", "reason": reviewPrompt(agent, in.SessionID, in.Cwd)})
			out = string(b)
		}
	}
	return out, saveState(in.SessionID, st)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, s)
}

func pruneState(dir string) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		if st, err := os.Stat(f); err == nil && time.Since(st.ModTime()) > 14*24*time.Hour {
			os.Remove(f)
		}
	}
}
