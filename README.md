# claude-code-statusline

A 2-line statusline for [Claude Code](https://docs.claude.com/en/docs/claude-code) showing session location, working-branch diff, linked PR size, model, context window, 5h/7d rate-limit progress bars, and today's API cost.

```
📍 abc12345-test | 📁 ~/projects/my-repo | 🌿 feature/foo +12 -3 | #1234 +500 -200
🤖 Opus 4.7 | ctx ██░░░░░░░░ 28% | 5h ███░░░░░░░ 35% ~23:30 | 7d ██████░░░░ 62% ~6/22(月)18:00 | $9.42 today
```

## What's shown

**Line 1 — location**

| Segment | Source |
|---|---|
| `📍 <session_id>` | stdin `session_id` (useful to resume a frozen session) |
| `📁 <path>` | `workspace.project_dir` (short-formed with `~`) |
| `🌿 <branch> +ins -del` | `git branch --show-current` + `git diff HEAD --shortstat` (working-tree changes, staged + unstaged, untracked excluded) |
| `\| #<PR> +ins -del` | `gh pr view --json number,baseRefName` + `git diff origin/<base>...HEAD --shortstat` (only when an open PR exists for the branch; 60 s cached) |

**Line 2 — model & limits & today**

| Segment | Source |
|---|---|
| `🤖 <model>` | stdin `model.display_name` |
| `ctx ██░░ XX%` | stdin `context_window.remaining_percentage` |
| `5h / 7d ██░░ XX% ~time` | stdin `rate_limits.{five_hour,seven_day}` (`used_percentage` + `resets_at` shown in JST) |
| `$X.XX today` | Recomputed from `~/.claude/projects/**/*.jsonl` using a local `PRICING` table |

Colors (green/yellow/red) are applied to all progress bars by usage threshold (<40 / <70 / >=70).

## Requirements

- `bash`, `jq`, `git`
- [Go](https://go.dev/dl/) (>= 1.21) — required to build the `calc-daily-cost` helper binary used by `show-statusline.sh` for daily-cost computation and rate-limit formatting.
- [`gh`](https://cli.github.com/) — optional; only needed if you want the `#<PR> +ins -del` segment on feature branches.

No Python / `uv` / Node runtime dependencies.

## Install

1. Copy the scripts into `~/.claude/hooks/`:

   ```bash
   mkdir -p ~/.claude/hooks
   cp show-statusline.sh calc-daily-cost.go ~/.claude/hooks/
   chmod +x ~/.claude/hooks/show-statusline.sh
   ```

2. Build the Go helper binary:

   ```bash
   cd ~/.claude/hooks && go build -o calc-daily-cost ./calc-daily-cost.go
   ```

3. Register the statusline in `~/.claude/settings.json`:

   ```json
   {
     "statusLine": {
       "type": "command",
       "command": "$HOME/.claude/hooks/show-statusline.sh"
     }
   }
   ```

4. Restart Claude Code (or `/restart`).

## `calc-daily-cost` helper binary

A single Go binary exposes two subcommands that `show-statusline.sh` shells out to:

- `calc-daily-cost daily-cost` — Scans `~/.claude/projects/**/*.jsonl` and prints `{"daily_cost": ..., "burn_rate_cost_per_hour": ...}`. Uses **per-file incremental rescan**: only files whose `(mtime, size)` changed since the last invocation are reparsed. On a 500 MB / 1700-file transcript directory the hot path is ~20 ms instead of ~3 s for a full rescan.
- `calc-daily-cost rate-limits` — Reads the statusline-input JSON from stdin and prints a single line summarising `rate_limits.{five_hour,seven_day}` as colored progress bars with JST-formatted reset times.

Static binary, no runtime dependencies. Starts in ~5 ms.

## Pricing table

`calc-daily-cost.go` recomputes today's spend by scanning your local Claude Code transcripts and applying the `pricing` map at the top of the file. The default table covers Opus 4.7 / 4.8, Sonnet 4.6, Haiku 4.5, and Fable 5.

When Anthropic adjusts pricing, edit the per-token values in the `pricing` map using the current values from the [official pricing page](https://docs.anthropic.com/en/docs/about-claude/pricing), then rebuild:

```bash
cd ~/.claude/hooks && go build -o calc-daily-cost ./calc-daily-cost.go
```

The `today` figure will then track real billing.

## Caching

- Daily cost output: 60-second cache at `/tmp/cc-daily-cost-cache-<uid>.json`
- Daily cost incremental state: `/tmp/cc-daily-cost-state-<uid>.json` — per-file `(mtime, size, per-day breakdown, recent entries)`. Drop this to force a cold rescan
- PR lookup: 60-second cache at `/tmp/cc-pr-cache-<uid>-<hash>.json`, keyed by `<repo path>:<branch>`. Empty results (no PR, `gh` unauthenticated, network failure) are also cached as `{}` to avoid hammering `gh` once per statusline render
- If you want to force-refresh (e.g. after `git fetch`): `rm /tmp/cc-pr-cache-*` or `rm /tmp/cc-daily-cost-cache-*`

## Branch / PR behavior

| Branch state | Segment shown |
|---|---|
| `main` / `master` | `🌿 main` only (PR lookup skipped by design) |
| Feature branch, no PR | `🌿 feature/foo (+X -Y if dirty)` |
| Feature branch, PR open | `🌿 feature/foo (+X -Y if dirty) \| #1234 +500 -200` |
| PR with 0 diff vs base | `🌿 feature/foo \| #1234` (size segment omitted) |
| `gh` missing or unauthenticated | PR segment is silently skipped |

## License

MIT
