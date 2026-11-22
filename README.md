# DoneGuard

**Your coding agent says it's done. DoneGuard checks.**

DoneGuard is a small CLI for developers who run **Claude Code** or **Codex CLI** and
want "done" to mean "the tests pass". It plugs into the agents' **Stop** and
**SubagentStop** hooks: when the agent tries to end its turn, DoneGuard runs your
verify commands (tests, lint, type-check) and checks that no protected paths
(`.env`, keys, secrets) were touched (as seen by git; gitignored files aren't). If anything fails, the stop is **blocked** and
the agent gets a short, actionable reason (the failing command, its exit code and
the relevant lines of output), so it keeps working until the checks pass, up to a
retry cap.

```
DoneGuard blocked stop: `go test ./...` failed (exit 1).
--- output ---
--- FAIL: TestAdd (0.00s)
    add_test.go:7: expected 5, got 4
FAIL
---
Fix the failing tests/checks, re-run `go test ./...` until it passes, then you may stop.
```

It ships as a single static Go binary with no runtime dependencies. DoneGuard itself
makes no network calls and is local only (your verify commands and Codex may). Tested against Claude Code 2.1.x and Codex CLI 0.170.

## Motivation

The **Runta FrontierHarness Eval** (November 2026, 27,280 runs, with Baseten) found
that **64.5% of inference spend went to runs that did not pass**
([source](https://x.com/SadCreatorTalks/status/2123074519787302997)).

A "done" that hasn't been verified costs you twice: once for the tokens, and again
when you find the breakage later. DoneGuard turns "the agent says it's done" into
"the tests say it's done". It also logs every stop attempt, so you can measure your
own false-done rate with `doneguard report`.

## Install

Requires **Go 1.23+**.

```sh
go install github.com/mkarvan/doneguard/cmd/doneguard@latest
```

This puts `doneguard` in `$(go env GOBIN)` (default `$(go env GOPATH)/bin`). To pin a
release, use a tag instead of `@latest`, e.g. `@v0.1.0`.

From source:

```sh
git clone https://github.com/mkarvan/doneguard
cd doneguard
CGO_ENABLED=0 go build -o doneguard ./cmd/doneguard
sudo mv doneguard /usr/local/bin/    # optional; hooks use the absolute path anyway
```

## Quickstart

Common setup, in your project (a git repository):

```sh
cd ~/code/my-project
doneguard init                 # writes .doneguard.yml (auto-detects your test command)
                               # and adds .doneguard/ to .gitignore
git add .doneguard.yml .gitignore && git commit -m "Add DoneGuard"
```

Commit the config. Inside a git repository the **committed** `.doneguard.yml` is the
policy, so the agent can't weaken it by editing the file
(see [Policy integrity](#how-it-works)).

### Claude Code

```sh
doneguard install --agent claude   # merges Stop + SubagentStop into .claude/settings.json
```

**Restart Claude Code** so it loads the new hooks (check with `/hooks`). Then use it
as usual; when Claude tries to finish with failing checks, the stop is blocked and
it is told what to fix.

### Codex CLI

```sh
doneguard install --agent codex    # merges Stop + SubagentStop into .codex/hooks.json
```

Codex only loads project-local hooks in a **trusted** project, and only runs hooks
you have approved. Interactively, approve the DoneGuard hook in `/hooks` (or when
Codex prompts you). For `codex exec`, `--trust` saves project trust and
`--dangerously-bypass-hook-trust` skips hook review for that one run; automation
that vets hook sources usually passes both. See [Codex hook trust](#codex-hook-trust).

If hooks can't be trusted or aren't available, use the wrapper instead:

```sh
doneguard run codex -- exec "make the failing tests pass"
```

See [Codex wrapper (fallback)](#codex-wrapper-fallback).

### Both, and the report

`doneguard install` with no `--agent` registers both. Afterwards:

```sh
doneguard report    # false-done stats from .doneguard/sessions.jsonl
```

## How it works

```
agent tries to stop
        │
        ▼
 doneguard hook  ◄── hook JSON on stdin (session_id, cwd, stop_hook_active, …)
        │
        ├─ no .doneguard.yml above cwd? → allow silently (never bricks other projects)
        ├─ .doneguard.yml invalid?      → block: "fix the config" (counts as a retry)
        │
        ├─ 1. protected paths: git status (tracked + untracked, honouring .gitignore)
        │      + commits since the last allowed stop, matched against `protected`
        ├─ 2. verify commands: run in order via the shell, with timeouts
        │
        ├─ all pass                → allow (exit 0, no output)
        ├─ fail, retries < max     → {"decision":"block","reason":"…"}  (exit 0)
        └─ fail, retries ≥ max     → allow, logged as UNVERIFIED
                                     (with a systemMessage warning for the user)
        │
        ▼
 .doneguard/sessions.jsonl  ◄── one JSON line per stop attempt (+ token usage)
```

**Retry logic.** Claude Code sets `stop_hook_active: true` while the agent is
continuing because of a Stop hook. Allowing whenever that flag is true would give the
agent just one retry, so DoneGuard ignores it for that purpose and keeps its own
per-session counter under `.doneguard/state/`:

- Block while checks fail and `retries < max_retries`.
- When `retries >= max_retries`, allow the stop and mark the session **UNVERIFIED**.
- Never block more than **7 times in a row**. Claude Code caps consecutive Stop-hook
  blocks at 8 (`CLAUDE_CODE_STOP_HOOK_BLOCK_CAP`), so `max_retries` is clamped to 7.
- A stop with `stop_hook_active: false` (a new user turn) starts a new chain.
- Subagents (`SubagentStop`) get their own counter per `agent_id`, so parallel
  subagents don't eat into the main session's budget.
- The counter is interprocess-safe. A block slot is reserved under a per-key file
  lock *before* the checks run and released when they pass, so racing hook processes
  (for example, a project and a global install firing together) can never exceed the
  cap. The lock is not held while verify commands run. Each reservation stays tied to
  its hook invocation until that invocation settles: a concurrent passing stop or a
  fresh turn resets the counter only down to the slots still in flight, and a hook
  blocks only if it still holds its reservation. A reservation from a crashed hook
  expires after the sum of the verify timeouts plus five minutes.
- A retry-state file that exists but can't be read or decoded is not treated as a new
  chain. Mid-chain, a failing stop is allowed as **UNVERIFIED**. The next fresh turn
  (`stop_hook_active: false`) or verified allow replaces the file.
- A `stop_hook_active` string other than `false`/`0`/`no` counts as `true`, so
  malformed input can't reset the counter.
- **If the retry state cannot be written** (lock timeout, unwritable `.doneguard/`, or
  a `.doneguard/` symlink that leads outside the project), DoneGuard never emits a
  block it couldn't count. A failing stop is allowed as **UNVERIFIED** with a
  warning. This is a deliberate fail-open: blocking without counting could loop
  forever.
- After an UNVERIFIED allow the counter stays at the cap until the next fresh turn,
  and the protected-path baseline (`BaseHead`) is **not** advanced. Only a verified
  allow moves the baseline forward, so violations that were committed and then
  allowed as UNVERIFIED stay visible on later stops.
- Hook input that isn't a JSON object, or that has no `hook_event_name` or
  `session_id`, is ignored (exit 0, no output) and never touches the retry counter.
  Input larger than 32 MiB is ignored rather than parsed from a truncated prefix.

**Protocol.** By default DoneGuard prints a single JSON object on stdout and exits 0,
which both Claude Code and Codex accept. It never sends `continue: false`, because on
Stop that would end the turn instead of sending the agent back to work. The documented
fallback, exit code 2 with the reason on stderr, is available via
`doneguard hook --exit2` or `DONEGUARD_HOOK_MODE=exit2`.

**Policy integrity.** Inside a git repository the **committed** config is the
policy. The nearest directory, from cwd up to the repo root, whose `.doneguard.yml`
(or `.yaml`) is in `HEAD` owns the project. DoneGuard blocks, and runs **no**
verify commands, when any of these is true:

- that config was modified (staged or not), deleted, or renamed;
- an uncommitted `.doneguard.yml`/`.yaml` appears between cwd and the owner
  directory, where it would shadow the committed one;
- the config is gone but `.doneguard/` runtime data shows it was in use (for
  example, a committed deletion). This also applies to a nested project whose own
  committed config was deleted while a parent directory still has one;
- the config is a symlink, in the work tree or in `HEAD`. A symlink's target can
  change while the link itself stays clean, so it is never followed.

Verify commands and limits are parsed from the **committed bytes** (`HEAD`) of the
config that passed these checks. They are never read again from the work tree, so
replacing the file after the check has no effect.

DoneGuard never falls back to a parent project's config when the project's own
config was removed. When the policy is violated, `max_retries` and `output_limit`
come from the `HEAD` copy, so an agent can't switch to audit mode by editing the
file. A brand-new, uncommitted config (right after `doneguard init`) is used as-is,
so your first session isn't blocked by it.

**Indeterminate checks.** If git confirms that the directory isn't a git repository,
the protected-path check is skipped, as before. A missing `git` binary is not
treated as "not a repository". DoneGuard can't tell then whether a committed policy
exists, so it blocks as indeterminate wherever a config or `.doneguard/` runtime
data is visible. Any other git failure blocks as an
*indeterminate* check (`block_kind: "indeterminate"`), and the reason names the git
error. That includes a corrupt index, a broken `HEAD`, or a recorded baseline commit
that no longer exists, so a git error is never read as "no violations". Verify
commands are not run in that case either. At the retry cap it becomes an
UNVERIFIED allow like any other failure.

**Re-check after verify.** Protected paths are checked again after the verify
commands finish. A test or generator that touches `.env` during verification
blocks the stop (`.env (modified, during verify)`). The second check compares
against `HEAD` from before verify ran, even on a session's first stop when no baseline
was stored yet. A verify command that commits a protected file is caught that way
(`.env (committed, during verify)`), and that pre-verify `HEAD` is kept as the baseline
for the next stop. If the repository disappears during verification, the check is
indeterminate.

## Commands

| Command | What it does |
|---|---|
| `doneguard init [--force] [--dir D] [--no-gitignore]` | Writes `.doneguard.yml` with auto-detected verify commands and default protected globs, and appends `.doneguard/` to `.gitignore`. Refuses to overwrite an existing config without `--force`. |
| `doneguard install [--agent claude\|codex\|all] [--local] [--global] [--exe PATH] [--timeout S]` | Merges Stop + SubagentStop hooks into `.claude/settings.json` (or `settings.local.json` with `--local`) and `.codex/hooks.json`. Idempotent; never clobbers other hooks or settings; keeps key order. |
| `doneguard uninstall [--agent …] [--local] [--global]` | Removes only DoneGuard's entries. Leaves other hooks and settings alone and never deletes the file. |
| `doneguard hook [--source claude\|codex] [--exit2] [--verbose]` | Hook entry point (this is what the configs call). |
| `doneguard report [--json] [--file PATH]` | False-done stats from `.doneguard/sessions.jsonl` (found by walking up from cwd). |
| `doneguard run codex -- <codex args>` | Fallback wrapper (see below). |
| `doneguard version` | Prints the version. |

Auto-detection (`init`):

| Found | Verify command |
|---|---|
| `Makefile` with a `test:` rule | `make test` (used alone: it's the project's canonical entry point) |
| `package.json` with a real `test` script | `npm test` / `pnpm test` / `yarn test` / `bun run test` (from lockfile or `packageManager`) |
| `pytest.ini`, `pyproject.toml`/`tox.ini` mentioning pytest, `setup.cfg [tool:pytest]`, `conftest.py`, or `tests/test_*.py` | `python -m pytest` (`uv run pytest` / `poetry run pytest` with those lockfiles) |
| `go.mod` | `go test ./...` |
| `Cargo.toml` | `cargo test` |

If several ecosystems are present (and there's no Makefile `test` rule), each one adds
its own command. If nothing is detected, `init` writes `verify: []` with a note
telling you to add one.

## Config reference (`.doneguard.yml`)

```yaml
verify:                       # run in order; the first failure blocks
  - command: go test ./...
    timeout: 120s             # Go duration or integer seconds; default 120s
  - npm run lint              # plain strings work too
protected:                    # created/modified/deleted ⇒ block
  - ".env"
  - ".env.*"
  - "!.env.example"           # negation: later rules win
  - "**/*secret*"
  - "**/*credential*"
  - "**/id_rsa"
  - "**/*.pem"
  - "secrets/"                # everything inside a directory
max_retries: 3                # blocks per turn before allowing as UNVERIFIED (0 = audit only; max 7)
verify_cwd: .                 # where verify commands run (default: the config's directory)
output_limit: 4000            # max chars of command output in a block reason (min 200)
```

**Protected pattern rules** (paths are relative to the config's directory):

- A pattern with no `/` (`.env`, `*.pem`) matches the file name at any depth.
- A pattern containing `/` matches the whole path. `**` matches any number of
  directories (`**/id_rsa`, `config/**`, `a/**/b`). A leading `/` anchors it to the
  project root.
- A trailing `/` (`secrets/`), or a pattern with no wildcards (`secrets`,
  `config/prod`), also protects everything inside that directory.
- `*`, `?`, `[a-z]` and `{a,b}` work as usual. `!pattern` un-protects (last match wins).
- `.doneguard/` and `.git/` are always ignored.

**Output trimming.** When a command's output is longer than `output_limit`, DoneGuard
keeps the tail (test runners print their summary last) and pulls forward the earlier
lines that look like failures: `--- FAIL`, `Error`, `panic:`, `assert`,
`expected`, `Traceback`, and so on. ANSI colours and progress-bar redraws are stripped.

**Validation.** Unknown keys, bad durations, bad glob syntax, etc. are errors that
include line numbers. When the config is invalid, the hook **blocks** with the parse
error so the agent (or you) can fix it, rather than silently letting everything through.

The YAML parser is built in so the binary stays dependency-free. It supports the
subset you'd use here: block maps and lists, quoted strings, flow lists, `|`/`>`
block scalars and comments. Anchors, tags, flow mappings, block-scalar indentation
indicators and multi-document files are rejected with an error; nothing is silently
mis-parsed.

## Hook configuration written by `install`

```json
{
  "hooks": {
    "Stop": [
      { "hooks": [ { "type": "command",
                     "command": "/usr/local/bin/doneguard hook --source claude",
                     "timeout": 300,
                     "statusMessage": "DoneGuard verification" } ] }
    ],
    "SubagentStop": [ … same … ]
  }
}
```

- The command uses the absolute, symlink-resolved path from `os.Executable()`, so
  DoneGuard doesn't need to be on `PATH`. Re-run `install` after moving the binary;
  it updates the entry in place.
- The timeout is `max(300, sum of verify timeouts + 60)` seconds unless you pass
  `--timeout`.
- DoneGuard only claims entries whose command is exactly its managed form. That
  means a single program named `doneguard` (or `doneguard.exe`) at any path,
  followed by `hook` and optionally `--source claude|codex`, `--exit2` or
  `--verbose`. This covers what `install` writes and the legacy forms
  `doneguard hook` and `/path/to/doneguard hook`. Compound shell commands
  (`doneguard hook && …`), wrapper scripts (`my-doneguard hook`) and entries that
  only share the `statusMessage` label are never modified or removed.
- A binary installed under another name (`install --exe /path/dg`) is recognised
  by its exact path. Re-installing doesn't duplicate it, and
  `uninstall --exe /path/dg` (or running `uninstall` from that binary) removes it.
- The managed hook is always placed in a group with no `matcher`. A legacy
  DoneGuard entry found in a scoped group is removed from that group, and the
  group's other hooks are kept.
- Project-local writes (`.claude/settings*.json`, `.codex/hooks.json`,
  `.doneguard.yml`, `.gitignore`, and everything under `.doneguard/`) refuse to
  follow symlinks that lead outside the project. `--global` targets still follow
  symlinks so dotfile managers keep working.
- `--global` writes to `~/.claude/settings.json` and `~/.codex/hooks.json`. It's
  opt-in only. Because the hook allows silently in any directory without a
  `.doneguard.yml`, a global install only affects projects that have one.

### Codex hook trust

Two separate things must happen before Codex (tested with 0.170) runs the hook:

1. **The project is trusted**, so Codex loads the project-local `.codex/hooks.json`.
   Interactively, Codex asks on first use. Non-interactively, `codex exec --trust`
   saves that trust for the project.
2. **The hook is approved.** Interactively, approve it in `/hooks` (or when
   prompted). `--dangerously-bypass-hook-trust` skips this review for that one
   invocation only; nothing is saved.

Automation that already vets hook sources (e.g. CI) usually passes both to
`codex exec`. If your Codex build
gates hooks behind a feature flag, enable it (`codex features list` should show
`hooks` as enabled). When Codex receives `{"decision":"block","reason":…}` from a
Stop hook, it continues the turn and uses the reason as the next prompt.

### Codex wrapper (fallback)

If hooks can't be trusted or aren't available, wrap Codex instead:

```sh
doneguard run codex -- exec --model gpt-6 "make the failing tests pass"
```

1. Refuses to start if the config differs from the committed one.
2. Runs `codex <args>` with stdout and stderr passed straight through.
3. If Codex exits non-zero, the turn didn't complete. The wrapper returns that exit
   code without verifying or resuming.
4. Otherwise it runs the same verification as the hook.
5. If verification fails and retries remain, it resumes **the same session**:
   `codex exec <original options> -C <abs dir> resume <session-id> "<failure reason>"`.
   The original root options (`-m`, `-c`, `-p`, `-s`, `-a`, `--add-dir`, `--json`,
   `--dangerously-*`, …) are repeated before `resume`. `-C` is preserved as an
   absolute path. Images (`-i`) are not re-sent.
6. Exits with Codex's exit code once verified. If the cap is hit, it prints
   **UNVERIFIED** and exits **3**.

**Session identity.** The wrapper only resumes a session it can identify for
certain. It accepts three sources:

- the structured `{"type":"thread.started","thread_id":…}` event (the first line of
  `--json` output);
- the `session id: <uuid>` line in the stderr header of `codex exec`;
- an explicit `resume <uuid>` in your arguments.

It **never** uses `resume --last`, which could pick up a different, concurrent
session. In each of the following cases the wrapper verifies once and, on failure,
exits **UNVERIFIED** (3) instead of resuming:

- no id was found;
- two different ids were reported;
- a resumed run reports a different id;
- the session is `--ephemeral`;
- you ran an interactive session without an explicit id;
- you ran `resume --last …`. Codex then picks the most recent session and treats a
  UUID-shaped positional as the prompt, so it is never taken as a session id;
- your arguments include an option DoneGuard doesn't recognise. This currently
  includes `--trust`: passed through the wrapper, it makes DoneGuard verify once and
  never auto-resume. Trust the project first with a plain `codex exec --trust …`
  run, or use the hooks.

Non-task invocations (`--version`, `--help`, `login`, `mcp`, …) are passed through
untouched. Inside the wrapper, `DONEGUARD_WRAPPER=1` is set, and a native DoneGuard
hook stays silent so retries are owned by one layer. Set `DONEGUARD_CODEX_BIN` to
use a different `codex` binary.

## Logging and `doneguard report`

Each stop attempt adds one line to `.doneguard/sessions.jsonl` next to
`.doneguard.yml`. The file is private (`0600`). Writes use `O_APPEND` plus a bounded
interprocess lock, so concurrent subagent hooks never interleave. If the lock can't be
taken within 2 s, the entry is skipped so the verdict isn't delayed. Transcripts
are only parsed if they are regular files no larger than 256 MiB. Fields: `timestamp`, `session_id`, `agent_id`,
`hook_event_name`, `source` (`claude`|`codex`|`wrapper`), `decision`, `reason`,
`block_kind`, `verify_ok`, `verify_skipped`, `failing_command`, `failing_exit_code`,
`protected_violations`, `retry_count`, `max_retries`, `unverified`,
`stop_hook_active`, `input_tokens`, `output_tokens`, `total_tokens`,
`last_assistant_message` (truncated), `duration_ms`, `notes`.

Token usage is read from `transcript_path` (or `agent_transcript_path` for
subagents) when the format is recognised. For Claude Code JSONL, `message.usage` is
de-duplicated by message id, and input includes cache creation and cache reads. For
Codex rollouts, the last `token_count.total_token_usage` is used. The values are
cumulative for that transcript and are `null` if unavailable. Parsing never fails
the hook.

Output format (placeholders, not real results; `<n>` is a count, `<p>` a percentage):

```
$ doneguard report
DoneGuard report — <path>/.doneguard/sessions.jsonl
  Period:               <first timestamp> → <last timestamp>
  Stop attempts:        <n> across <n> session(s) [claude <n>, codex <n>]
  Blocked:              <n> (verify failed <n>, protected path <n>, config error <n>, check incomplete <n>)
  Allowed:              <n> (verified <n>, UNVERIFIED <n>, no verify configured <n>)
  False-done:           <n> of <n> stop attempts (<p>%) blocked because checks failed
  Unverified sessions:  <n>
  Tokens:               <n> total; <n> (<p>%) spent on false-done attempts
```

- **False-done** (`false_done`): a stop attempt that DoneGuard
  **blocked** because verification failed. Either a verify command failed or a
  protected path (including the DoneGuard config itself) was touched. In other
  words, the agent said it was done and it wasn't.
  - Config errors (`blocked_config`) and indeterminate checks
    (`blocked_indeterminate`) are not false-dones.
  - Neither are allows at the retry cap. The blocked attempts earlier in that
    session were already counted.
- **Verified / UNVERIFIED allows** (`allowed_unverified`, `unverified_sessions`):
  UNVERIFIED means the stop was allowed while checks were still failing. That
  happens because the retry cap was reached, `max_retries: 0` audit mode is on, or
  retry state couldn't be saved.
- **Tokens on false-done attempts**: for each attempt, the growth in the
  transcript's cumulative usage since the previous attempt in the same session,
  summed over the false-done attempts.

`doneguard report --json` prints the same data in machine-readable form.

## Safety behaviour

- **No config, no effect.** If no `.doneguard.yml` exists in cwd or any parent, the
  hook prints nothing and allows the stop, so a stray binary on `PATH` or a global
  install can't brick unrelated projects. Use `--verbose` to see why on stderr.
- **Malformed hook input** never blocks the agent; it's noted on stderr and allowed.
  It is never counted as a stop.
- **`DONEGUARD_DISABLE=1`** turns the hook into a no-op (an escape hatch).
- **No git repo**: the protected-path check is skipped (with a note in the log) and
  verify commands still run. Other git errors block as indeterminate (see above).
- **Verify must finish.** If a verify command exits but leaves background processes
  holding its output open, the check is *incomplete*. It fails and the leftovers are
  killed.
- **Empty `verify`**: protected paths are still checked. If they're clean the stop is
  allowed and logged as `verify_skipped`.
- Verify commands run with stdin closed and their output captured, so they can't
  corrupt the hook's JSON on stdout. They run in their own process group and the
  whole tree is killed on timeout. `DONEGUARD=1` is set in their environment.
- DoneGuard itself makes no network calls and has no telemetry. Your verify commands,
  and a wrapped Codex, may.

## Trust model

DoneGuard exists to run your project's verify commands, and those commands run
with the hook's full privileges and environment. That means **installing the
hook, or running `doneguard run codex`, in a project means you trust that project's
verify commands.** That covers `.doneguard.yml` itself and everything those
commands execute: `go test`, `make test` and `npm test` all run code from the
working tree, which the agent can change.

What DoneGuard does guarantee:

- It never runs verify commands from a policy that differs from the committed
  one. The one exception is first run: a brand-new, uncommitted `.doneguard.yml`
  (no committed copy yet) is used as-is until you commit it.
- It never lets a removed or shadowing config silently replace the committed
  one.

It does **not** sandbox verify commands, sign policies, or stop an agent from
changing the code those commands run (for example, editing a `Makefile` or weakening
a test). Protect such files with `protected:` globs if that matters to you. A global
install applies this trust to every repository that contains a `.doneguard.yml`, so
prefer project installs for checkouts you don't control. The agent can also
disable DoneGuard outright (`DONEGUARD_DISABLE=1`, `doneguard uninstall`, deleting
`.doneguard/state`). DoneGuard is a guard against an agent that *thinks* it's done
and isn't; it isn't a security boundary against an adversarial one.

## Limitations

- **Block cap.** Claude Code stops honouring Stop-hook blocks after 8 in a row, so
  DoneGuard caps itself at 7 and then allows the stop as UNVERIFIED. A stubborn
  failure ends the turn eventually; check `doneguard report`.
- **Hook trust.** Codex won't run the hook until you trust it. Hooks that never run
  can't protect you; use `doneguard run codex` where trust isn't possible.
- **You must define verify.** DoneGuard is only as good as your verify commands.
  With `verify: []` it only checks protected paths.
- **Protected paths are git-based.** Changes are detected with `git status`, which
  respects `.gitignore`. A gitignored `.env` that the agent edits is **not** detected.
  Pre-existing uncommitted changes to a protected file also count as violations, so
  start sessions from a clean state for protected files. Commits made after the last
  *verified* stop are included; commits made before DoneGuard's first stop in a
  session are not. The check compares endpoints, so a change that was committed and
  later reverted is not seen.
- **Windows.** Hook command quoting assumes a POSIX-style shell, and verify-command
  descendants are not tracked or killed on Windows.
- **Hook timeout.** If verification runs longer than the hook timeout, the agent
  platform kills the hook and usually treats that as a non-blocking error, so the stop
  goes through unverified and nothing is logged. Keep verify fast or raise `--timeout`.
- **Not a substitute for code review.** Passing tests ≠ correct code. An agent can
  still write weak tests. Consider protecting test fixtures (e.g. `"**/testdata/**"`)
  if gaming is a concern.
- **Local only.** No dashboards, no sync. The log is a local JSONL file.

## Why Go?

A Stop hook runs on every turn of every session, so it has to start instantly and
work on any machine without a runtime. Go builds one static binary
(`CGO_ENABLED=0`) for Linux, macOS and Windows. Its standard library covers
everything DoneGuard needs (process groups, JSON, file locking), and the code has
zero third-party dependencies.

## Development

```sh
gofmt -l .                 # should print nothing
go vet ./...
CGO_ENABLED=0 go test ./...
```

`internal/` holds the packages: `config` (YAML subset + validation), `detect`
(init), `glob` (protected patterns), `gitutil`, `verify` (runner + output trimming),
`hook` (decision engine + policy integrity), `install` (order-preserving JSON merge),
`sessionlog`, `state` (retry counters), `filelock`, `safepath` (symlink confinement),
`transcript` (token usage), `report`, `wrapper` (Codex fallback) and `cli`. The `e2e/`
tests build the real binary, create a sample git repo with a failing Go test and a
protected `.env`, and pipe realistic Claude Code and Codex payloads into
`doneguard hook`. They're fully offline: Codex is replaced by a shell script.

CI (`.github/workflows/ci.yml`) runs `go vet ./...` and `CGO_ENABLED=0 go test ./...`
on every push and pull request.

## Releases

There is no release tooling; a release is a git tag. Tag a version
(`git tag v0.1.0 && git push origin v0.1.0`) and users install it with
`go install github.com/mkarvan/doneguard/cmd/doneguard@v0.1.0` (or `@latest`). Keep
`Version` in `internal/cli/cli.go` in step with the tag, or set it at build time with
`-ldflags "-X github.com/mkarvan/doneguard/internal/cli.Version=v0.1.0"`.

## License

MIT. Copyright 2026 DoneGuard contributors. See [LICENSE](LICENSE).
