package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const entrySep = "\n§\n"

var defaultLimits = map[string]int{"user": 1200, "memory": 2500, "project": 2500}

func home() string {
	if h := os.Getenv("AMEM_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".amem")
}

type project struct{ ID, Root string }

// detectProject maps a working directory to its git root (or the dir itself).
// The home directory is not a project: memory there is global.
func detectProject(cwd string) *project {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	h, _ := os.UserHomeDir()
	if real, err := filepath.EvalSymlinks(h); err == nil {
		h = real
	}
	if abs == h || filepath.Dir(abs) == abs {
		return nil
	}
	root := abs
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			root = d
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	sum := sha1.Sum([]byte(root))
	return &project{ID: filepath.Base(root) + "-" + hex.EncodeToString(sum[:4]), Root: root}
}

type store struct {
	Target string
	Path   string
	Limit  int
}

func limits() map[string]int {
	l := maps.Clone(defaultLimits)
	var cfg struct {
		Limits map[string]int `json:"limits"`
	}
	if b, err := os.ReadFile(filepath.Join(home(), "config.json")); err == nil && json.Unmarshal(b, &cfg) == nil {
		for k, v := range cfg.Limits {
			if v > 0 {
				l[k] = v
			}
		}
	}
	return l
}

func storeFor(target string, p *project) (store, error) {
	l := limits()
	switch target {
	case "user":
		return store{target, filepath.Join(home(), "USER.md"), l["user"]}, nil
	case "memory":
		return store{target, filepath.Join(home(), "MEMORY.md"), l["memory"]}, nil
	case "project":
		if p == nil {
			return store{}, errors.New("no project here (home dir or unknown cwd); use target \"memory\"")
		}
		return store{target, filepath.Join(home(), "projects", p.ID, "MEMORY.md"), l["project"]}, nil
	}
	return store{}, fmt.Errorf("unknown target %q (want user|memory|project)", target)
}

func (s store) load() ([]string, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseEntries(string(b)), nil
}

func parseEntries(raw string) []string {
	raw = "\n" + strings.ReplaceAll(raw, "\r\n", "\n") + "\n"
	var out []string
	for _, e := range strings.Split(raw, entrySep) {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func size(entries []string) int { return utf8.RuneCountInString(strings.Join(entries, entrySep)) }

type op struct {
	Op   string `json:"op"`
	Text string `json:"text,omitempty"`
	Old  string `json:"old,omitempty"`
}

type batch struct {
	Target string `json:"target"`
	Ops    []op   `json:"ops"`
}

func applyOps(entries []string, ops []op) ([]string, error) {
	out := slices.Clone(entries)
	for i, o := range ops {
		text := strings.TrimSpace(o.Text)
		if o.Op == "add" || o.Op == "replace" {
			if text == "" {
				return nil, fmt.Errorf("op %d (%s): text is required", i, o.Op)
			}
			for _, line := range strings.Split(text, "\n") {
				if strings.TrimSpace(line) == "§" {
					return nil, fmt.Errorf("op %d: a line containing only § is reserved as the entry separator", i)
				}
			}
		}
		switch o.Op {
		case "add":
			if !slices.Contains(out, text) {
				out = append(out, text)
			}
		case "replace", "remove":
			idx, err := findEntry(out, o.Old)
			if err != nil {
				return nil, fmt.Errorf("op %d (%s): %w", i, o.Op, err)
			}
			if o.Op == "replace" {
				out[idx] = text
			} else {
				out = slices.Delete(out, idx, idx+1)
			}
		default:
			return nil, fmt.Errorf("op %d: unknown op %q (want add|replace|remove)", i, o.Op)
		}
	}
	return out, nil
}

func findEntry(entries []string, old string) (int, error) {
	if old = strings.TrimSpace(old); old == "" {
		return -1, errors.New("old is required: a short unique substring of the entry")
	}
	match := -1
	for i, e := range entries {
		if strings.Contains(e, old) {
			if match >= 0 {
				return -1, fmt.Errorf("%q matches more than one entry; use a longer substring", old)
			}
			match = i
		}
	}
	if match < 0 {
		return -1, fmt.Errorf("no entry contains %q", old)
	}
	return match, nil
}

// saveBatches validates every batch before writing anything, so a rejected
// batch leaves all stores untouched.
func saveBatches(batches []batch, p *project) (string, error) {
	if len(batches) == 0 {
		return "", errors.New("nothing to save")
	}
	unlock, err := lock()
	if err != nil {
		return "", err
	}
	defer unlock()

	type pending struct {
		s       store
		entries []string
	}
	staged := map[string]*pending{}
	var order []string
	for _, b := range batches {
		pd, ok := staged[b.Target]
		if !ok {
			s, err := storeFor(b.Target, p)
			if err != nil {
				return "", err
			}
			entries, err := s.load()
			if err != nil {
				return "", err
			}
			pd = &pending{s, entries}
			staged[b.Target] = pd
			order = append(order, b.Target)
		}
		next, err := applyOps(pd.entries, b.Ops)
		if err != nil {
			return "", fmt.Errorf("%s: %w", b.Target, err)
		}
		pd.entries = next
	}
	for _, t := range order {
		pd := staged[t]
		before, _ := pd.s.load()
		if n := size(pd.entries); n > pd.s.Limit && n > size(before) {
			return "", fmt.Errorf("%s would be %d/%d chars; nothing was saved. In ONE batch, merge, shorten or remove stale entries to make room. Current entries:\n%s",
				t, n, pd.s.Limit, listing(before))
		}
	}
	var report []string
	for _, t := range order {
		pd := staged[t]
		if err := writeAtomic(pd.s.Path, strings.Join(pd.entries, entrySep)+"\n"); err != nil {
			return "", err
		}
		if t == "project" {
			_ = writeAtomic(filepath.Join(filepath.Dir(pd.s.Path), "path"), p.Root+"\n")
		}
		report = append(report, fmt.Sprintf("%s: %d entries, %d/%d chars", t, len(pd.entries), size(pd.entries), pd.s.Limit))
	}
	return "saved — " + strings.Join(report, "; "), nil
}

func listing(entries []string) string {
	if len(entries) == 0 {
		return "(empty)"
	}
	var b strings.Builder
	for i, e := range entries {
		fmt.Fprintf(&b, "[%d] %s\n", i, indent(e))
	}
	return b.String()
}

func indent(s string) string { return strings.ReplaceAll(s, "\n", "\n    ") }

func writeAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// lock is a portable advisory lock: an exclusive lock file, broken if stale.
func lock() (func(), error) {
	if err := os.MkdirAll(home(), 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(home(), ".lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) > 30*time.Second {
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("memory is locked by another process (%s)", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
