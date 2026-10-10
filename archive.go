package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type logEntry struct {
	TS      string `json:"ts"`
	Session string `json:"session,omitempty"`
	Project string `json:"project,omitempty"`
	Path    string `json:"path,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Text    string `json:"text"`
}

func historyPath() string { return filepath.Join(home(), "history.jsonl") }
func notesDir() string    { return filepath.Join(home(), "notes") }

// appendLog adds a session summary; a later summary for the same session replaces it.
func appendLog(text, agent, session string, ts time.Time, p *project) (string, error) {
	if text = strings.TrimSpace(text); text == "" {
		return "", errors.New("empty log summary")
	}
	e := logEntry{TS: ts.Format(time.RFC3339), Session: session, Agent: agent, Text: text}
	if p != nil {
		e.Project, e.Path = p.ID, p.Root
	}
	line, _ := json.Marshal(e)
	unlock, err := lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if session != "" {
		if replaced, err := replaceSessionLog(session, line); err != nil || replaced {
			return "updated session summary" + projectSuffix(p), err
		}
	}
	f, err := os.OpenFile(historyPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return "", err
	}
	return "logged session summary" + projectSuffix(p), nil
}

func replaceSessionLog(session string, line []byte) (bool, error) {
	b, err := os.ReadFile(historyPath())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	kept := lines[:0]
	found := false
	for _, l := range lines {
		var e logEntry
		if json.Unmarshal([]byte(l), &e) == nil && e.Session == session {
			found = true
			continue
		}
		kept = append(kept, l)
	}
	if !found {
		return false, nil
	}
	return true, writeAtomic(historyPath(), strings.Join(append(kept, string(line)), "\n")+"\n")
}

func projectSuffix(p *project) string {
	if p == nil {
		return " (global)"
	}
	return " for " + p.Root
}

func readLogs() ([]logEntry, error) {
	f, err := os.Open(historyPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []logEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e logEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Text != "" {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

type note struct {
	Slug, Desc, Body string
}

var slugRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._-]*$`)

func notePath(slug string) (string, error) {
	if !slugRe.MatchString(slug) {
		return "", fmt.Errorf("invalid slug %q (letters, digits, . _ -)", slug)
	}
	return filepath.Join(notesDir(), slug+".md"), nil
}

func writeNote(n note) (string, error) {
	path, err := notePath(n.Slug)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(n.Body) == "" {
		return "", errors.New("empty note body")
	}
	desc := strings.Join(strings.Fields(n.Desc), " ")
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", n.Slug, desc, strings.TrimSpace(n.Body))
	unlock, err := lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := writeAtomic(path, content); err != nil {
		return "", err
	}
	return "saved note " + n.Slug, nil
}

// parseNote accepts plain markdown or markdown with name/description frontmatter.
func parseNote(raw, fallbackSlug string) note {
	n := note{Slug: fallbackSlug, Body: raw}
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	if rest, ok := strings.CutPrefix(raw, "---\n"); ok {
		if fm, body, ok := strings.Cut(rest, "\n---"); ok {
			n.Body = strings.TrimSpace(strings.TrimPrefix(body, "\n"))
			for _, line := range strings.Split(fm, "\n") {
				k, v, _ := strings.Cut(line, ":")
				v = strings.Trim(strings.TrimSpace(v), `"'`)
				switch strings.TrimSpace(k) {
				case "name":
					if slugRe.MatchString(v) {
						n.Slug = v
					}
				case "description":
					n.Desc = v
				}
			}
		}
	}
	return n
}

func readNotes() ([]note, error) {
	files, err := filepath.Glob(filepath.Join(notesDir(), "*.md"))
	if err != nil {
		return nil, err
	}
	var out []note
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		out = append(out, parseNote(string(b), strings.TrimSuffix(filepath.Base(f), ".md")))
	}
	return out, nil
}

func showNote(slug string) (string, error) {
	path, err := notePath(slug)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("no note %q; try `amem search`", slug)
	}
	return string(b), err
}

type hit struct {
	score float64
	ts    string
	head  string
	text  string
	terms []string
}

type doc struct {
	head, body, ts, meta, hay string
}

// search ranks notes and session logs by case-insensitive substring matches,
// weighting each term by rarity (IDF) so common words matter less. Any term may
// match. Substrings need no tokenizer, so CJK text works as-is.
func search(query string, limit int) (string, error) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return "", errors.New("empty query")
	}
	notes, err := readNotes()
	if err != nil {
		return "", err
	}
	logs, err := readLogs()
	if err != nil {
		return "", err
	}
	var docs []doc
	for _, n := range notes {
		meta := strings.ToLower(n.Slug + " " + n.Desc)
		docs = append(docs, doc{"[note] " + n.Slug + " — " + n.Desc, n.Body, "", meta, meta + " " + strings.ToLower(n.Body)})
	}
	for _, e := range logs {
		head := "[log " + e.TS[:min(10, len(e.TS))]
		if e.Path != "" {
			head += " " + e.Path
		}
		if e.Agent != "" {
			head += " · " + e.Agent
		}
		docs = append(docs, doc{head + "]", e.Text, e.TS, "", strings.ToLower(e.Project + " " + e.Text)})
	}
	idf := make([]float64, len(terms))
	for i, t := range terms {
		df := 0
		for _, d := range docs {
			if strings.Contains(d.hay, t) {
				df++
			}
		}
		idf[i] = math.Log(1 + float64(len(docs)+1)/float64(df+1))
	}
	var hits []hit
	for _, d := range docs {
		s := 0.0
		for i, t := range terms {
			if c := strings.Count(d.hay, t); c > 0 {
				w := idf[i] * (1 + math.Log(float64(c)))
				if strings.Contains(d.meta, t) {
					w *= 2
				}
				s += w
			}
		}
		if s > 0 {
			hits = append(hits, hit{s, d.ts, d.head, d.body, terms})
		}
	}
	if len(hits) == 0 {
		return "no matches for " + query, nil
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].ts > hits[j].ts
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%d matches", len(hits))
	if len(hits) > limit {
		fmt.Fprintf(&b, ", showing %d", limit)
		hits = hits[:limit]
	}
	b.WriteString(" (full note: amem show <slug>)\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "\n%s\n%s\n", h.head, snippet(h.text, h.terms, 400))
	}
	return b.String(), nil
}

func snippet(text string, terms []string, width int) string {
	r := []rune(text)
	if len(r) <= width {
		return text
	}
	lower := []rune(strings.ToLower(text))
	at := 0
	for _, t := range terms {
		if i := strings.Index(string(lower), t); i >= 0 {
			at = len([]rune(string(lower)[:i]))
			break
		}
	}
	start := min(max(0, at-width/3), len(r))
	end := min(len(r), start+width)
	s := string(r[start:end])
	if start > 0 {
		s = "…" + s
	}
	if end < len(r) {
		s += "…"
	}
	return s
}
