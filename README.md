# claude-knowledge-vault

> Local-first semantic + keyword search over your own Claude Code
> conversation history. Reads `~/.claude/projects/*.jsonl`, indexes
> into SQLite FTS5, exposes MCP + dashboard + slash. 100 % offline,
> no external embeddings.

**Status — PoC v1 in progress.** Core engine (parse · chunk · store ·
search · indexer · MCP · cmd) and dashboard (HTTP API · embedded UI ·
turn-detail drawer) are done and tested. Packaging shim, slash
command, release workflow, and the README/docs you're reading still
have placeholders. The full handoff is in
[**STATUS.md**](./STATUS.md).

## Quick start (source build, works today)

```bash
git clone https://github.com/0xmhha/claude-knowledge-vault.git
cd claude-knowledge-vault

# build the binary (Go 1.25.2+)
( cd go && go build -o ../kvault ./cmd/kvault )

# one-shot index of your conversation history
./kvault --once index --plugin-data /tmp/kv

# one-shot search
./kvault --once search --plugin-data /tmp/kv --query "the thing"

# or open the web dashboard
./kvault --port 0 --plugin-data /tmp/kv     # prints a URL, Ctrl-C to stop
```

5 commands, zero config files, zero external accounts. The full T-T.5
README will replace this stub with reference tables, ASCII screenshot,
security model, and roadmap.

## What's done · what's next

See [STATUS.md](./STATUS.md) for the live snapshot:

- 18 commits, core 9/9, dashboard 5/5, critical path 7/7
- Coverage: 8 internal packages average 87.6 %
- 9 tasks remaining (packaging 3 + tests/docs 6) with per-task spec,
  acceptance, dependencies — ready to pick up on any machine

The design rationale (why BM25 only in v1, why `modernc.org/sqlite`,
why no hook capture yet) lives in [PLAN.md](./PLAN.md). The 24-task
graph + critical path + parallel batches live in
[IMPL_PLAN.md](./IMPL_PLAN.md).

## License

MIT. Sibling plugin: [claude-env-sync](https://github.com/0xmhha/claude-env-sync) —
the operational infrastructure (run.sh / mcp / dashboard scaffolding /
hooks / lint config) this project forks from. The FTS5 chunking
algorithm is adapted from [context-mode](https://github.com/mksglu/context-mode).
See LICENSE.
