# tokenator bench — A/B harness for agent configurations

`tokenator bench` runs the same task through multiple **arms** (harness +
configuration variants), N trials each, in clean isolated sessions, then
compares outcome, duration, tokens, and cost per arm.

```
tokenator bench run spec.json          # preflight → arms × trials → report
tokenator bench report [run-key]       # latest run by default; --trials for detail
tokenator bench list
tokenator bench run --resume 20260719-2055-smoke   # rerun error/timeout trials
tokenator bench ingest <run-key>       # backfill transcripts if inline ingest failed
```

Every trial consumes **real API/subscription usage** — the preflight prints
the arms × trials matrix and asks before running (`--yes` to skip).

## How a trial runs

1. Fresh copy of the `workspace` template directory (the agent's cwd).
2. Fresh harness home:
   - Claude Code: a per-trial `CLAUDE_CONFIG_DIR` — **no** user settings,
     plugins, hooks, memory, or MCP servers leak in. OAuth credentials and
     account identity are seeded from `~/.claude` so subscription billing
     works; transcripts land inside the trial dir.
   - OpenCode: a per-trial `XDG_DATA_HOME`; `auth.json` is seeded.
3. Headless run: `claude -p --output-format json --session-id <uuid>`
   (bench pre-assigns the session id, so the trial→session join is exact)
   or `opencode run --format json --auto` (session id parsed from events).
4. `checks` run in the workspace (exit 0 = pass): all pass → `ok`, any fail
   → `fail`. Both are completed observations; `timeout`/`error` are not,
   and `--resume` reruns only those.
5. The trial's transcripts are ingested into the tokenator DB immediately.

Reports join usage from the ingested requests via each trial's transcript
root, so subagent/sidechain sessions are counted with their trial. Trial
sessions are ordinary sessions afterward: `tokenator session <uuid>` and the
serve UI work on them.

## Spec reference

```json
{
  "name": "claude-mem-onoff",
  "prompt": "Fix the failing test so `go test ./...` passes. Do not modify the test file.",
  "prompt_file": "(alternative to prompt: path relative to this file)",
  "workspace": "/home/erewhon/bench/templates/widget-bug",
  "checks": [
    {"name": "tests", "cmd": "go test ./..."},
    {"name": "test-untouched", "cmd": "git diff --exit-code -- internal/widget/widget_test.go"}
  ],
  "trials": 3,
  "timeout_seconds": 900,
  "check_timeout_seconds": 120,
  "env": {"ANTHROPIC_BASE_URL": "https://llm.bcc.sh"},
  "regime": "subscription",
  "arms": [
    {"name": "clean", "harness": "claude_code", "model": "sonnet"},
    {"name": "mem", "harness": "claude_code", "model": "sonnet",
     "claude": {"plugin_dirs": ["/home/erewhon/.claude/plugins/cache/thedotmack/claude-mem/10.5.5"]}}
  ]
}
```

Arm fields: `harness` (`claude_code` | `opencode`), `model` (CC alias/name;
OC `provider/model`), `env`, `extra_args` (verbatim CLI passthrough), and:

- `claude`: `plugin_dirs` (each → `--plugin-dir`), `mcp_configs` (each →
  `--mcp-config`, plus `--strict-mcp-config`), `settings` (JSON or path →
  `--settings`), `setting_sources`, `permission_mode` (default
  `bypassPermissions` — trials run in throwaway copies), `effort`, `bare`.
- `opencode`: `pure` (no external plugins), `agent`, `variant`,
  `config_file` (exported as `OPENCODE_CONFIG`).

Since the config dir starts empty, a claude_code arm with no options **is**
the clean baseline; options add things back.

## Caveats

- **`bare` + subscription billing don't mix**: `--bare` never reads OAuth
  credentials (auth is strictly `ANTHROPIC_API_KEY`/apiKeyHelper). The
  fresh config dir already gives you a clean arm; reserve `bare` for
  API-key arms.
- **Plugins with external state**: claude-mem keeps its memory DB outside
  the config dir, so its state accumulates across trials and arms. That's
  usually what you want to measure (real accumulated memory), but it makes
  trials order-dependent; point its env at a copy for stricter isolation.
- **Routing through the gateway** (`ANTHROPIC_BASE_URL=https://llm.bcc.sh`
  in spec `env`) gives you wire-truth cache telemetry for every trial via
  `tokenator reqlog` + `cache --wire`.
- Trials run sequentially by design — duration comparisons stay honest.
- **Prompt-cache warmth crosses trials**: identical system/tools prefixes hit
  the org-level Anthropic cache, so the first trial of an arm pays a cold
  cache write (~4× cost) and later trials ride warm. Medians absorb this,
  and symmetric arms each pay one cold trial — but compare totals with care.
- OpenCode's harness-reported cost is unreliable for custom providers; the
  report prefers Claude Code's own total and flags the rest.
- Comparing across harnesses (CC vs OC arms) confounds system prompt, tool
  set, and agent loop all at once — it answers "which tool does better
  here", not "why".

## Example: model vs model

```json
{
  "name": "haiku-vs-sonnet",
  "prompt": "Add a --version flag printing 1.0.0, then make sure go test ./... passes.",
  "workspace": "/home/erewhon/bench/templates/cli-tool",
  "checks": [{"name": "tests", "cmd": "go test ./..."}],
  "trials": 5,
  "env": {"ANTHROPIC_BASE_URL": "https://llm.bcc.sh"},
  "arms": [
    {"name": "haiku",  "harness": "claude_code", "model": "haiku"},
    {"name": "sonnet", "harness": "claude_code", "model": "sonnet"}
  ]
}
```

## Reading the report

```
ARM     HARNESS      MODEL   TRIALS  PASS  MED DUR  P90 DUR  MED IN  MED OUT  MED CACHE RD  MED CACHE WR  MED COST
clean   claude_code  sonnet  3/3     3/3   94s      118s     1,204   8,911    310,442       102,003       $0.8110
mem     claude_code  sonnet  3/3     2/3   81s      101s     1,377   7,214    355,020       121,336       $0.9021
```

Tokens are per-trial medians over ingested requests (cache read/write are
where context-heavy plugins show up). Pass rate is the outcome guardrail:
cheaper tokens on a failing arm is a loss, not a win. `--trials` adds the
per-trial table with session ids for `tokenator session` drill-down.
