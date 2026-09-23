# syntax=docker/dockerfile:1

# Linux sandbox for running CLI coding agents (Claude Code, Codex, ...)
# against bind-mounted host folders.
FROM node:22-bookworm-slim

# Pin these to reproduce a specific build, e.g. --build-arg CLAUDE_VERSION=2.1.280
ARG CLAUDE_VERSION=latest
ARG CODEX_VERSION=latest

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
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
        vim-tiny \
    && rm -rf /var/lib/apt/lists/*

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

COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 755 /usr/local/bin/entrypoint.sh

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
RUN npm config set prefix /home/agent/.npm-global \
    && printf 'export PATH=/home/agent/.npm-global/bin:$PATH\n' >> /home/agent/.bashrc

# Installed as `agent`, not root: Claude Code self-updates in place and needs
# write access to its own install directory.
RUN npm install -g \
        "@anthropic-ai/claude-code@${CLAUDE_VERSION}" \
        "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force

# The agent CLIs are pinned at build time and the container is stateless, so
# telemetry, error reporting and self-update checks are just noise here.
ENV CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    DISABLE_AUTOUPDATER=1

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["bash", "-l"]
