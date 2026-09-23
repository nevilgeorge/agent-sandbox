# agent-sandbox

A Linux container for running CLI coding agents — Claude Code, Codex, or anything
else you install — against host folders you explicitly mount. The agent can read
and write those folders and nothing else on your machine, so it is safe to hand it
free rein inside (`claude --dangerously-skip-permissions`) without giving it free
rein over your home directory.

## What's in the image

Debian bookworm (`node:22-bookworm-slim`) with:

- **Python 3.11** (`python3`, `pip`, `venv`)
- **git**
- **Node 22** + npm
- **Claude Code** (`@anthropic-ai/claude-code`) and **Codex** (`@openai/codex`)
- curl, wget, jq, ripgrep, less, vim-tiny, build-essential, openssh-client,
  procps, dnsutils

Everything runs as the non-root user `agent` (uid 1000).

### Why this base image

`node:22-bookworm-slim` *is* a Linux distribution — it's `debian:bookworm-slim`
with Node unpacked into `/usr/local` by the Node maintainers. It was chosen because
both agent CLIs ship through npm, so Node has to be there regardless.

The base contributes little to image size: of ~1.8GB, the agent CLIs account for
~560MB and `build-essential` for most of the ~420MB apt layer. Alpine would save
~150MB and cost you Python's prebuilt `manylinux` wheels (musl forces `pip` to
compile from source). Ubuntu 24.04 would get you Python 3.12 and git 2.43 instead
of 3.11 and 2.39, at the price of installing Node by hand — a reasonable swap if
you ever want it, but not one the base was doing much work for either way.

If a project needs a newer Python than 3.11, build a venv inside the container
rather than rebuilding the image.

## Setup

**1. Start the container runtime.** If you use OrbStack, open `OrbStack.app` once —
that starts the daemon and installs the `docker` CLI onto your `PATH`. (The wrapper
will find OrbStack's bundled CLI even before you do this, but the daemon still has to
be running.)

**2. Build the image:**

```sh
./sandbox build
```

**3. Add API keys.** The container stores no credentials and persists nothing
between runs, so authentication comes entirely from the environment. Put the keys
in a `.env` beside the `sandbox` script:

```sh
cp .env.example .env
chmod 600 .env
$EDITOR .env        # fill in ANTHROPIC_API_KEY and/or OPENAI_API_KEY
```

`.env` is gitignored and excluded from the Docker build context, so it is never
committed and never baked into an image layer. Every `KEY=VALUE` in it is passed
into the container, so anything else the agents need — `GITHUB_TOKEN`,
`ANTHROPIC_MODEL` — goes in the same file with no change to the script.

The file is parsed rather than sourced: one `KEY=VALUE` per line, `#` comments and
a leading `export` are fine, but there is no `$VAR` interpolation and no multi-line
values. Variables already exported in your shell take precedence over the file, so
`ANTHROPIC_API_KEY=other ./sandbox run ...` and CI secret injection still work
unchanged — as does skipping `.env` entirely and exporting everything yourself.
`SANDBOX_ENV_FILE=/path/to/other.env` reads a different file.

`./sandbox run` refuses to start if neither key is found, and warns if only one is.

This is what makes the container portable: there is no login step, no named
volume to migrate, and no machine-specific state. The same image plus the same
`.env` behaves identically on any host.

`hello-world/` is a small Python project included in this repo purely as a
target to try the examples below against.

## Usage

```sh
./sandbox run                        # mount the current directory at /workspace
./sandbox run ~/code/myproject       # mount that directory instead
./sandbox run . -- claude --dangerously-skip-permissions
./sandbox run . -- python3 script.py # one-off command, no shell
```

Mount extra folders with `-v`, same syntax as docker:

```sh
./sandbox run ~/code/myproject \
    -v ~/datasets:/mnt/data:ro \
    -v ~/scratch:/mnt/scratch
```

## Headless / non-interactive

`./sandbox run DIR -- CMD` runs a command instead of opening a shell. It allocates
a TTY only when one is present, so it works unchanged from scripts, pipes, cron and
CI. Both agents authenticate from the keys in `.env` (or your environment); Codex
additionally gets logged in at container start, see Notes.

```sh
# Claude Code
./sandbox run hello-world -- claude -p --dangerously-skip-permissions "run the tests and fix failures"

# machine-readable output (result, cost, turn count, session id)
./sandbox run hello-world -- claude -p --dangerously-skip-permissions \
    --output-format json "summarize this project" | jq -r .result

# prompt on stdin
echo "what does main() do?" | ./sandbox run hello-world -- claude -p --dangerously-skip-permissions

# Codex
./sandbox run hello-world -- codex exec --dangerously-bypass-approvals-and-sandbox "add a test for main()"
./sandbox run hello-world -- codex exec --json --dangerously-bypass-approvals-and-sandbox "..."
```

The `--dangerously-*` flags are what make these headless: without them each agent
stops to ask for approval and hangs with no one to answer. They are appropriate
here precisely because the container *is* the sandbox — Codex's own help describes
that flag as "intended solely for running in environments that are externally
sandboxed". The agent can still only reach the directories you passed with `-v`.

Exit status propagates, so `&&` chaining and CI gating work normally. Useful extras:
`--max-turns N` caps agent loops, and `--output-format stream-json` emits events as
they happen for long runs.

Other commands:

| Command | Purpose |
|---|---|
| `./sandbox shell` | Open a second shell in the running sandbox |
| `./sandbox rebuild` | Rebuild from scratch (picks up new agent versions) |
| `./sandbox --help` | Full usage |

Resource caps, if you want them:

```sh
SANDBOX_CPUS=4 SANDBOX_MEMORY=8g ./sandbox run ~/code/myproject
```

`SANDBOX_PIDS` caps the container's process count the same way (default 2048).

## Notes

- **Agent versions are baked in at build time.** `./sandbox rebuild` pulls the latest.
  To pin either CLI: `./sandbox build --build-arg CLAUDE_VERSION=2.1.280
  --build-arg CODEX_VERSION=0.156.1` (the versions currently in the image).
- **Networking is unrestricted.** The isolation here is filesystem and process, not
  network — the agent can reach anything your machine can.
- **git identity** is copied from your host `git config --global` at run time. Commits
  made in the container are unsigned; your macOS signing keys are not mounted.
- **pip** on bookworm is PEP 668-managed. Use a venv (`python3 -m venv .venv`) or
  `pip install --break-system-packages` for throwaway installs.
- **The container is stateless.** Each `./sandbox run` is a fresh `--rm` container
  with no volumes attached. Anything written outside `/workspace` and your extra
  `-v` mounts is discarded on exit -- including agent config, caches and session
  history. That is the tradeoff for portability: nothing to migrate, but also no
  conversation history across runs, and model caches are re-fetched each time.
- **API keys bill per token**, unlike a subscription OAuth login. Watch usage if
  you script long headless runs.
- **Codex is logged in at container start.** Unlike Claude Code, which reads
  `ANTHROPIC_API_KEY` straight from the environment, Codex authenticates from
  `~/.codex/auth.json`; its websocket transport sends no `Authorization` header
  without it and 401s with "Missing bearer or basic authentication in header"
  even when `OPENAI_API_KEY` is set and valid. `docker/entrypoint.sh` runs
  `codex login --with-api-key` on every start to seed it. If codex ever 401s,
  `./sandbox run . -- codex doctor` shows the auth mode and handshake result.
- **`.env` never leaves the host directory.** It is listed in `.gitignore`, and
  `.dockerignore` excludes everything but `docker/entrypoint.sh` from the build
  context, so no key reaches an image layer. The keys are passed to the container
  at run time with `docker run -e`, which means they are visible in
  `docker inspect` for the life of the container.
