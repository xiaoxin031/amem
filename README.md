<div align="center">

# amem

**Lightweight, automatic memory for AI coding agents.**
One small memory shared by Claude Code, Codex, and any MCP client. It loads at a fixed size and saves itself after you change code.

[![ci](https://github.com/xiaoxin031/amem/actions/workflows/ci.yml/badge.svg)](https://github.com/xiaoxin031/amem/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/xiaoxin031/amem)](https://github.com/xiaoxin031/amem/releases)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

English · [中文](README.zh-CN.md)

</div>

```sh
curl -fsSL https://raw.githubusercontent.com/xiaoxin031/amem/main/install.sh | sh
```

That's the whole setup. The next Claude Code or Codex session already knows who you are and what this repo is about, and keeps that knowledge current as you work.

---

## Why

AI coding agents forget everything between sessions. The usual fixes have problems:

| | append-only `memories.md` / `/save` | vendor memory | **amem** |
|---|---|---|---|
| Context cost per session | grows forever (we measured **117 KB** on one real project) | hidden | **fixed, about 5 KB** |
| Saving | you type `/save` | automatic, one tool only | **automatic, after edits** |
| Shared across tools | no | no | **Claude Code, Codex, any MCP client** |
| Stale facts | pile up | ? | **replaced or merged, hard size cap** |
| Your data | markdown | vendor cloud | **plain text in `~/.amem`** |

amem keeps a small, curated memory that is always loaded, and puts everything else in an archive the agent searches only when it needs to.

## What you get

- **Fixed-size context.** At session start the agent gets your profile, global notes, this repo's notes, and its last 3 session summaries. A size limit on each store keeps the total near 5 KB no matter how much history builds up.
- **Saving without `/save`.** When the agent changes files (editor tools, patches, `sed`, redirects; detected from git state) or every 8 turns, it is asked to record what matters. This runs inside the same session and reuses the prompt cache, so the cost is one short reply.
- **Memory that gets better, not bigger.** Writes are atomic batches of add / replace / remove. If a store would go over its limit, nothing is written and the agent gets the current entries back, so it has to merge or drop stale ones.
- **One memory, every tool.** Claude Code and Codex share the same store. Something one agent learns, the other knows next session.
- **Recall on demand.** Past session summaries and long notes are searchable (`amem search`, or the `memory_search` tool). Ranking weights rare words above common ones, and substring matching handles Chinese, Japanese and Korean without a tokenizer.
- **No setup.** The installer registers the hooks and pre-trusts them in Codex, adds an auto-approved MCP server so writes never hit a sandbox or permission prompt, and backs up every file it changes.
- **Small and boring.** One static Go binary (~3 MB), no runtime, no database server, no API key, no network. A snapshot takes about 12 ms. Runs on macOS, Linux and Windows (x64 and ARM).

## How it works

```
                session start                         after a turn that edited files
                      │                                         │
   ┌──────────────────▼──────────────────┐     ┌────────────────▼────────────────┐
   │ SessionStart hook → snapshot         │     │ Stop hook → "save what matters"  │
   │  USER.md      who you are    ≤1200   │     │  memory_save  durable facts      │
   │  MEMORY.md    global facts   ≤2500   │     │  memory_log   1 summary/session  │
   │  project      this repo      ≤2500   │     └────────────────┬────────────────┘
   │  + last 3 session summaries          │                      │
   └──────────────────────────────────────┘                      ▼
                                                 ~/.amem   (plain text, yours)
          memory_search / amem search  ◀──────  history.jsonl · notes/*.md
```

```
~/.amem/
  USER.md                          who you are, preferences, rules   ┐
  MEMORY.md                        cross-project facts               ├ loaded every session
  projects/<repo>-<hash>/MEMORY.md this repo's conventions, gotchas  ┘
  history.jsonl                    one summary per session           ┐ searched, never
  notes/*.md                       runbooks, long references         ┘ loaded wholesale
```

The design follows the memory system of [hermes-agent](https://github.com/NousResearch/hermes-agent): capped stores, a snapshot frozen at session start so the prompt cache stays valid, and an archive kept out of the prompt.

## Install

**macOS / Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/xiaoxin031/amem/main/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/xiaoxin031/amem/main/install.ps1 | iex
```

**From a clone** (builds from source if Go is installed, otherwise downloads a release):

```sh
git clone https://github.com/xiaoxin031/amem && cd amem && ./install.sh
```

The installer puts `amem` in `~/.local/bin` and runs `amem install`, which detects Claude Code and Codex and sets them up:

| | Claude Code | Codex |
|---|---|---|
| Snapshot at start | `SessionStart` hook | `SessionStart` hook, pre-trusted |
| Auto-save | `Stop` hook | `Stop` hook, pre-trusted |
| Memory tools | MCP server `amem`, pre-allowed | MCP server `amem`, auto-approved |
| Manual save | `/save` | `$save` |

Every file it changes gets a timestamped `.bak-amem-*` backup first. Installed Claude or Codex later? Run `amem install` again; it is idempotent.

### Other tools (Cursor, Cline, Gemini CLI, opencode, …)

Add the MCP server:

```json
{ "mcpServers": { "amem": { "command": "amem", "args": ["mcp"] } } }
```

It provides `memory_load`, `memory_save`, `memory_log`, `memory_search`, `memory_show` and `memory_note`. Tools without lifecycle hooks can't save on their own, so add one line to their rules file: *"Call memory_load at the start; after changing code, call memory_save and memory_log."*

## Everyday use

You don't need to do anything. If you want to look:

```sh
amem ls                          # what's remembered, with usage per store
amem search deploy staging       # past sessions and notes
amem inject                      # exactly what the agent sees at session start
amem add user "Prefers short answers, no emojis"
amem rm project "old build command"
amem note db-runbook --desc "Postgres failover" < runbook.md
amem import ./old-notes/         # bring existing markdown in as searchable notes
```

## Configuration

Optional. `~/.amem/config.json`:

```json
{
  "limits": { "user": 1200, "memory": 2500, "project": 2500 },
  "review_every": 8
}
```

| Key | Meaning |
|---|---|
| `limits` | Character cap per store. This is what keeps the snapshot small. |
| `review_every` | Auto-save interval in turns when nothing was edited. `0` = only after edits, `-1` = never. |

`AMEM_HOME` moves the data directory. `CLAUDE_CONFIG_DIR` and `CODEX_HOME` are respected.

## FAQ

**Does auto-save cost a lot of tokens?**
No. It is one extra reply in the same session, served from the prompt cache. It runs only after turns that changed files, or every 8 turns.

**Is my data sent anywhere?**
No. amem makes no network calls. Memory is plain text under `~/.amem`, which you can read, edit, or put in git.

**What about secrets?**
The agent is told to save durable facts, not raw data. Memory is plain text, so read `amem ls` now and then and remove anything you don't want kept.

**How do I remove it?**
`amem uninstall` takes out the hooks, MCP servers, permissions and `/save`. Your memory data stays until you delete `~/.amem`.

## Development

```sh
go test ./...                    # unit + install/uninstall tests, no network
go build -o amem .
```

Tagging `v*` builds release binaries for macOS, Linux and Windows (amd64 and arm64).

## License

[MIT](LICENSE)
