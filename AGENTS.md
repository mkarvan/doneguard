# AGENTS.md

Instructions for AI coding agents (Claude Code, Codex CLI, and others).

DoneGuard is a single static Go binary that plugs into the **Stop** and **SubagentStop**
hooks of Claude Code and Codex CLI. When the agent tries to end its turn, DoneGuard
runs the project's verify commands (tests, lint, ...) and checks that no protected
paths (`.env`, keys, secrets) were touched. If anything fails, the stop is blocked and
the agent is told what to fix, up to a retry cap. It makes no network calls.

If the user asks you to "install DoneGuard", "add github.com/mkarvan/doneguard to this
repo" or similar, follow the next section exactly. Every step is non-interactive and
safe to run more than once.

## Installing DoneGuard into a user's project

### 1. Get the `doneguard` binary

**Hard rule: if you cannot get a working `doneguard` binary, stop here.** Do not run
`init` or `install`, do not create `.doneguard.yml`, and do not edit the project. Tell
the user which step failed and show its output.

Use the first option that applies, and check every exit status.

**a. Already installed.** If `doneguard version` exits 0 and prints a version, use it
and go to step 2.

**b. Go 1.23 or newer is available** (`go version`; this module's `go.mod` says
`go 1.23`, so older toolchains will not work):

```sh
go install github.com/mkarvan/doneguard/cmd/doneguard@latest &&
  export PATH="$(go env GOPATH)/bin:$PATH" &&
  { [ -z "$(go env GOBIN)" ] || export PATH="$(go env GOBIN):$PATH"; } &&
  doneguard version
```

If this exits nonzero, try option c; if that fails too, stop.

**c. Otherwise** (no Go, or Go older than 1.23), install the prebuilt binary (Linux/macOS,
amd64/arm64; no sudo; HTTPS only; verifies the SHA-256 against the release's
`checksums.txt`). Download the script to a file first, then run it. **Don't pipe it
into `sh`**: with a pipe a failed download or a failed install is easy to miss.

```sh
dg_tmp=$(mktemp -d) &&
  curl -fsSL --proto '=https' --proto-redir '=https' -o "$dg_tmp/install.sh" \
    https://raw.githubusercontent.com/mkarvan/doneguard/main/scripts/install.sh &&
  echo "downloaded $dg_tmp/install.sh"
```

If that exits nonzero, stop. Optionally read `$dg_tmp/install.sh`. Then:

```sh
sh "$dg_tmp/install.sh" &&
  export PATH="$HOME/.local/bin:$PATH" &&
  doneguard version
```

If this exits nonzero, stop.

The script installs to `~/.local/bin/doneguard` (it creates the directory if needed)
and exits nonzero if the download, the checksum or the installed binary's
`doneguard version` fails. Optional environment variables: `DONEGUARD_VERSION=v0.1.0`
pins a release, `DONEGUARD_BIN_DIR=/some/dir` changes the install directory (then put
that directory on `PATH` instead of `~/.local/bin`). `sh "$dg_tmp/install.sh" --dry-run`
prints what it would download without changing anything.

(Humans who have read the script may run it as
`curl -fsSL https://raw.githubusercontent.com/mkarvan/doneguard/main/scripts/install.sh | sh`.
As an agent, use the checked two-step download above.)

**Required check:** `doneguard version` must exit 0 and print a version before you go
on. If it says "command not found", the install directory is not on `PATH`; fix `PATH`
for this shell and re-check. If it still fails, stop (see the hard rule above). If the
user wants `PATH` permanent, tell them to add the `export PATH=...` line to their shell
profile; don't edit their dotfiles unless they ask. If your tool runs each command in
a fresh shell, repeat the `export PATH=...` (or use the absolute path to `doneguard`)
in the later steps.

### 2. Go to the user's project

`cd` to the root of the user's project. It should be a git repository
(`git rev-parse --show-toplevel`). DoneGuard's protected-path checks and policy
integrity checks need git.

If it isn't a git repository, run `git init` only if that is clearly what the user
wants (for example, a new project they asked you to set up). Otherwise tell the user it
isn't a git repository and that protected-path checks will be skipped, and continue.

### 3. Create the config

```sh
doneguard init --yes
```

This writes `.doneguard.yml` with auto-detected verify commands (`make test`,
`npm test`, `python -m pytest`, `go test ./...`, `cargo test`, ...) and default
protected globs, and adds `.doneguard/` to `.gitignore`. If a config file (or a
symlink to one) already exists it is left unchanged and the command exits 0. If
something unusable sits at `.doneguard.yml` / `.doneguard.yaml` (a directory, a
dangling symlink), it exits nonzero: stop and tell the user rather than continuing
without a policy.

Read the output. If it says no test command was detected, tell the user and offer to
add one under `verify:` in `.doneguard.yml`. Ask before committing the config;
when it is committed, the committed copy becomes the policy the agent can't weaken.

### 4. Register the hooks

Pick the agent the user runs:

```sh
doneguard install --agent claude   # Claude Code: .claude/settings.json
doneguard install --agent codex    # Codex CLI:   .codex/hooks.json
doneguard install                  # both, or if you're not sure
```

`install` is idempotent: re-running it updates the DoneGuard entry in place and never
touches other hooks or settings. It registers the absolute path of the binary you ran,
so run it with the `doneguard` you just installed.

### 5. Verify and tell the user what's next

```sh
doneguard version
doneguard report    # optional; empty until the first stop
```

Then check the hook files. Each one you installed must have a `"type": "command"` hook
under **both** `hooks.Stop` and `hooks.SubagentStop` whose `command` is
`<absolute path>/doneguard hook --source claude` (in `.claude/settings.json`) or
`... --source codex` (in `.codex/hooks.json`). A `grep` for "doneguard" is not enough;
check the structure. With `jq`:

```sh
# prints "true" and exits 0 only if both Stop and SubagentStop have the hook
dg_hooked() { jq -e --arg src "$1" '. as $r | all("Stop", "SubagentStop"; . as $e |
  any($r.hooks[$e][]?.hooks[]?; .type == "command" and
    (.command | test("doneguard\\x27? hook --source " + $src + "$"))))' "$2"; }
dg_hooked claude .claude/settings.json   # if installed for Claude Code
dg_hooked codex  .codex/hooks.json       # if installed for Codex
```

Without `jq`, read the JSON file and check the same thing by hand. If either event is
missing, re-run `doneguard install` for that agent and check again; don't hand-edit the
file. Show the user the two hook entries.

Then tell the user:

- **Claude Code:** restart Claude Code so it loads the new hooks (`/hooks` shows them).
- **Codex CLI:** Codex only runs project hooks in a **trusted** project, and only hooks
  that were **approved**. These are separate steps, not interchangeable:
  - interactively: trust the project when asked, and approve the DoneGuard hook in
    `/hooks`;
  - `codex exec --trust` saves trust for the project (it does not approve the hook);
  - `codex exec --dangerously-bypass-hook-trust` skips hook review for that one run.
    Use it only for automation where the hook source has already been vetted.

  See "Codex hook trust" in README.md. If hooks can't be used, the fallback is
  `doneguard run codex -- exec "<prompt>"`.

### 6. Uninstall

```sh
doneguard uninstall                  # both agents
doneguard uninstall --agent claude   # or just one
doneguard uninstall --agent codex
```

This removes only DoneGuard's hook entries and leaves other hooks, settings and the
files themselves alone. `.doneguard.yml` and `.doneguard/` are not removed; delete
them only if the user asks.

### 7. What NOT to do

- Don't hand-edit `.claude/settings.json` or `.codex/hooks.json` to add or remove
  DoneGuard hooks. Use `doneguard install` / `doneguard uninstall`.
- Don't commit `.doneguard/` (session logs, retry state). `init` adds it to
  `.gitignore`. `.doneguard.yml` **should** be committed.
- Don't run `doneguard init --force` unless the user explicitly asked to overwrite
  their config.
- Don't put secrets (tokens, passwords, API keys) in the hook command or in
  `.doneguard.yml`.
- Don't run `doneguard install --global` unless the user asked for a global install.
- Don't weaken `.doneguard.yml` (remove verify commands, lower `max_retries`) to get
  past a blocked stop. Fix the failing check instead.

## When DoneGuard blocks your stop

The block reason names the failing command, its exit code and the relevant output, or
the protected path you touched. Fix the cause, re-run the command until it passes, then
stop again. Editing or deleting a committed `.doneguard.yml` is itself blocked.

## Working on this repository

```sh
gofmt -l .                     # must print nothing
go vet ./...
CGO_ENABLED=0 go test ./...
```

No third-party dependencies. `scripts/install.sh` must stay POSIX `sh`
(`dash -n scripts/install.sh`).
