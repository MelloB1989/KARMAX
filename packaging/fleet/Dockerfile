# syntax=docker/dockerfile:1
#
# The agent fleet's image: one per agent container, and karmax-brain.
# Build from the repository root, as yourself, so files the agents write in
# the shared code directory are yours:
#
#   docker build -f packaging/fleet/Dockerfile -t karmax-fleet:2.1.294 \
#     --build-arg UID=$(id -u) --build-arg GID=$(id -g) --build-arg HOME_DIR=$HOME \
#     --build-arg CLAUDE_VERSION=2.1.294 .
#
# For PC2, build there or `docker save | ssh pc2 docker load` — the same tag.
# CLAUDE_VERSION is pinned (the auto-updater is off in the container); bump it
# deliberately, after `claude agents --json` is checked against the new one.

FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY fleet/go.mod fleet/go.sum ./fleet/
RUN cd fleet && go mod download
COPY . .
# The karmax CLI talks to the daemon on the host over its API; a pure-Go
# build is all a container needs.
RUN CGO_ENABLED=0 go build -tags modernc -o /out/karmax ./cmd/karmax \
 && cd fleet && CGO_ENABLED=0 go build -o /out/fleetctl ./cmd/fleetctl

FROM debian:trixie-slim
ARG CLAUDE_VERSION=2.1.294
ARG NODE_VERSION=22.12.0
ARG GO_VERSION=1.26.3
ARG BUN_VERSION=1.2.2
ARG UID=1000
ARG GID=1000
ARG HOME_DIR=/home/op
ARG USER_NAME=op

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git tini tmux jq ripgrep procps less openssh-client gnupg unzip xz-utils zstd \
      python3 python3-venv python3-pip build-essential make locales \
 && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

RUN set -eux; arch=$(dpkg --print-architecture); \
    case "$arch" in amd64) node=x64; bun=x64 ;; arm64) node=arm64; bun=aarch64 ;; *) echo "unsupported $arch"; exit 1 ;; esac; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${node}.tar.xz" | tar -xJ -C /usr/local --strip-components=1; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" | tar -xz -C /usr/local; \
    curl -fsSL -o /tmp/bun.zip "https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-${bun}.zip"; \
    unzip -q /tmp/bun.zip -d /tmp && mv /tmp/bun-linux-*/bun /usr/local/bin/bun && rm -rf /tmp/bun*; \
    npm install -g "@anthropic-ai/claude-code@${CLAUDE_VERSION}"; \
    npm cache clean --force

# You, inside: same uid, gid and HOME as on the host, so absolute paths under
# HOME (the shared code, Claude Code's per-project keys) match everywhere.
RUN set -eux; \
    getent group "$GID" >/dev/null || groupadd -g "$GID" "$USER_NAME"; \
    if getent passwd "$UID" >/dev/null; then userdel -r "$(getent passwd "$UID" | cut -d: -f1)" 2>/dev/null || true; fi; \
    useradd -u "$UID" -g "$GID" -d "$HOME_DIR" -m -s /bin/bash "$USER_NAME"; \
    mkdir -p "$HOME_DIR/.claude/sessions" "$HOME_DIR/.claude/fleet" /work /run/fleet; \
    chown -R "$UID:$GID" "$HOME_DIR" /work /run/fleet

COPY packaging/fleet/agent-run /opt/fleet/agent-run
COPY packaging/fleet/home/status.sh packaging/fleet/home/event.sh packaging/fleet/home/settings.json /opt/fleet/
COPY packaging/fleet/home/gitconfig /etc/gitconfig
COPY --from=build /out/karmax /out/fleetctl /usr/local/bin/
RUN chmod 755 /opt/fleet/agent-run /opt/fleet/*.sh \
 && cp /opt/fleet/settings.json "$HOME_DIR/.claude/settings.json" && chown "$UID:$GID" "$HOME_DIR/.claude/settings.json"

ENV HOME=${HOME_DIR} \
    DISABLE_AUTOUPDATER=1 \
    TERM=xterm-256color \
    LANG=C.UTF-8 \
    XDG_RUNTIME_DIR=/run/fleet \
    PATH=/usr/local/go/bin:${HOME_DIR}/go/bin:${HOME_DIR}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
USER ${UID}:${GID}
WORKDIR /work
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/opt/fleet/agent-run"]
