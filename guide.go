package main

import "strings"

// saveGuide is the manual save procedure (/save, $save, `amem help save`).
// Agents get the MCP tools: no shell quoting, no sandbox or permission prompts.
func saveGuide(agent string) string {
	if agent != "" {
		return `Save this session to amem memory, then tell the user in one line what was saved.

Use the amem MCP tools:
1. memory_log: a 3-8 line summary of the session (what was done, files changed,
   decisions and why, open TODOs). Pass cwd = the project directory.
2. memory_save: durable facts, ONE call per target, terse, one fact per entry.
   target user = who the user is, preferences, corrections
          memory = cross-project facts (machines, accounts, tools, quirks)
          project = this repo: layout, commands, conventions, gotchas (pass cwd)
   Replace or remove stale/overlapping entries in the same call instead of piling
   up. If over the limit nothing is saved and current entries are returned:
   merge or drop some and retry.
3. memory_note: long reference material (runbooks, inventories); searchable,
   not auto-loaded.
Skip what is trivial or easy to rediscover. Write in the user's language.
If the amem tools are unavailable, use the CLI shown by ` + "`" + cli() + ` help save` + "`" + `.`
	}
	return strings.NewReplacer("amem ", cli()+" ", "<agent>", "<your-name>").Replace(`Save this session to amem memory, then tell the user in one line what was saved.

1. Session summary (searchable; the latest few are shown at session start):
   amem log --agent <agent> <<'EOF'
   3-8 lines: what was done, files changed, decisions and why, open TODOs.
   EOF

2. Durable facts (loaded every session, size-capped), ONE batch:
   amem save <<'EOF'
   [{"target":"project","ops":[
     {"op":"add","text":"..."},
     {"op":"replace","old":"unique substring","text":"..."},
     {"op":"remove","old":"unique substring"}]}]
   EOF
   targets: user    = who the user is, preferences, corrections
            memory  = cross-project facts (machines, accounts, tools, quirks)
            project = this repo: layout, commands, conventions, gotchas
   One fact per entry, terse. Replace stale or overlapping entries instead of
   adding. If a store would exceed its limit nothing is saved and the current
   entries are listed: merge, shorten or remove in the same batch and retry.

3. Long reference material (runbooks, inventories): searchable, not auto-loaded:
   amem note <slug> --desc "one line" <<'EOF'
   ...
   EOF

Skip what is trivial or easy to rediscover. Write in the user's language.`)
}
