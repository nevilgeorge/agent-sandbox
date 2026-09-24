# syntax=docker/dockerfile:1

# uv comes from its own image rather than curl|sh: COPY --from resolves the
# multi-arch index for us, and the pinned tag is the integrity check.
ARG UV_VERSION=0.12.18
FROM ghcr.io/astral-sh/uv:${UV_VERSION} AS uv

# Linux sandbox for running CLI coding agents (Claude Code, Codex, ...)
# against bind-mounted host folders.
FROM node:22-bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# Debian's docker-clean hook deletes downloaded .debs after each install, which
# would defeat the cache mount below, so drop it and tell apt to keep them.
RUN rm -f /etc/apt/apt.conf.d/docker-clean \
    && printf 'Binary::apt::APT::Keep-Downloaded-Packages "true";\n' \
        > /etc/apt/apt.conf.d/keep-cache

# Cache mounts keep the .debs and the package lists outside the image, so an
# ordinary rebuild (a package added here, a CLI version bump below) unpacks
# from local disk instead of re-downloading -- measured at ~2.4x faster on the
# apt step. Note `./sandbox rebuild` passes --no-cache, which DOES empty these
# mounts on Docker 29.x; only the plain `./sandbox build` path benefits.
# There is deliberately no `rm -rf /var/lib/apt/lists/*`: with /var/lib/apt
# mounted that would wipe the cache we just populated, and the lists never
# reach a layer either way.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update -o Acquire::Retries=3 \
    && apt-get install -y --no-install-recommends \
        python3 \
        python3-pip \
        python3-venv \
        git \
        ca-certificates \
        curl \
        wget \
        less \
        jq \
        ripgrep \
        build-essential \
        openssh-client \
        procps \
        dnsutils \
        vim-tiny

COPY --from=uv /uv /uvx /usr/local/bin/
RUN uv --version
# uv's cache lives in $HOME but venvs are created under the /workspace bind
# mount, a different filesystem -- without this uv warns about failed hardlinks
# on every run.
ENV UV_LINK_MODE=copy

# The base image ships a `node` user at uid/gid 1000. Rename it to `agent`
# rather than creating a new one: keeping uid 1000 keeps bind-mounted file
# ownership sane, and running non-root is required because Claude Code
# refuses --dangerously-skip-permissions when it is running as root.
RUN usermod -l agent node \
    && groupmod -n agent node \
    && usermod -d /home/agent -m agent

# Agent-owned scratch dirs for per-run CLI state. The container is stateless:
# these are recreated empty on every run and discarded when it exits.
RUN mkdir -p /workspace /home/agent/.claude /home/agent/.codex /home/agent/.npm-global \
    && chown -R agent:agent /workspace /home/agent

# Debian's /etc/profile overwrites PATH outright, so a login shell (our CMD)
# would otherwise lose the npm global bin dir and not find claude or codex.
# /etc/profile.d/*.sh is sourced after that assignment, so this survives.
RUN printf 'export PATH=/home/agent/.npm-global/bin:$PATH\n' \
        > /etc/profile.d/10-npm-global.sh \
    && chmod 644 /etc/profile.d/10-npm-global.sh

# npm installs run as `agent` so Claude Code can self-update in place.
USER agent
ENV PATH=/home/agent/.npm-global/bin:$PATH
# Keep all of Claude Code's state under one directory rather than scattering
# .claude.json into $HOME. Nothing here persists -- auth comes from the
# environment -- but it keeps the container's filesystem tidy and predictable.
ENV CLAUDE_CONFIG_DIR=/home/agent/.claude

# The agent CLIs are pinned at build time and the container is stateless, so
# telemetry, error reporting and self-update checks are just noise here. Set
# before the install so the build-time version checks below inherit them.
ENV CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    DISABLE_AUTOUPDATER=1

# Declared here rather than at the top of the file: an ARG in scope is recorded
# on every RUN beneath it, which is why these two used to show up on the 467MB
# apt layer in `docker history`. Bump them to move the agent CLIs.
ARG CLAUDE_VERSION=2.1.282
ARG CODEX_VERSION=0.156.1

# Installed as `agent`, not root: Claude Code self-updates in place and needs
# write access to its own install directory. The cache mount must be owned by
# uid 1000 or npm falls back to an unwritable ~/.npm and re-downloads every
# build. `npm cache clean` is gone on purpose -- the cache is a mount now, it
# never reaches a layer, and cleaning it would discard what we are caching.
# The --version calls are a build-time smoke test: both CLIs resolve their
# native binary through optionalDependencies, and a silent failure there would
# otherwise only surface at the first agent run.
RUN --mount=type=cache,target=/home/agent/.npm,uid=1000,gid=1000,sharing=locked \
    npm config set prefix /home/agent/.npm-global \
    && printf 'export PATH=/home/agent/.npm-global/bin:$PATH\n' >> /home/agent/.bashrc \
    && npm install -g \
        "@anthropic-ai/claude-code@${CLAUDE_VERSION}" \
        "@openai/codex@${CODEX_VERSION}" \
    && claude --version \
    && codex --version

# Last layer that depends on the build context, so everything above stays
# cached when the entrypoint changes. The context is one file (.dockerignore
# excludes the rest), and with this ordering an entrypoint edit rebuilds 4kB
# instead of the ~1GB of apt and npm layers above. `bash -n` is a syntax check:
# the script runs under `set -euo pipefail`, so a typo would otherwise surface
# only at container start.
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN bash -n /usr/local/bin/entrypoint.sh

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["bash", "-l"]
