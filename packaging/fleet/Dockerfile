# syntax=docker/dockerfile:1
#
# The agent fleet's image: one per agent container, and karmax-brain.
# Build it with `fleetctl image build [--tag T]`, which passes UID, GID,
# USER_NAME, HOME_DIR and CLAUDE_VERSION from fleet.yaml (never by hand: the
# defaults below are not the fleet's, and an image whose home differs from
# fleet.yaml's leaves ~/.claude.json unwritable). For PC2:
# `fleetctl image ship pc2` — the same tag.
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
# The client for the build host (fleet.yaml build:); match its engine.
ARG DOCKER_VERSION=28.5.2
ARG UID=1000
ARG GID=1000
ARG HOME_DIR=/home/op
ARG USER_NAME=op

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git tini tmux jq ripgrep rsync procps less openssh-client gnupg unzip xz-utils zstd \
      python3 python3-venv python3-pip build-essential make locales \
 && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

# Chromium's runtime libraries, so an agent building a panel can look at it:
# `npx playwright install-deps chromium` lists these (its names are for
# Ubuntu 20.04; these are trixie's, t64 renames included), minus the X/GTK
# headed-mode extras. No browser is installed: `npx playwright install
# chromium` fetches one into the user's cache.
RUN apt-get update && apt-get install -y --no-install-recommends \
      fonts-liberation fonts-noto-color-emoji libasound2t64 libatk-bridge2.0-0t64 libatk1.0-0t64 \
      libatspi2.0-0t64 libcairo2 libcups2t64 libdbus-1-3 libdrm2 libfontconfig1 libfreetype6 libgbm1 \
      libglib2.0-0t64 libnspr4 libnss3 libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 libxdamage1 \
      libxext6 libxfixes3 libxkbcommon0 libxrandr2 \
 && rm -rf /var/lib/apt/lists/*

RUN set -eux; arch=$(dpkg --print-architecture); \
    case "$arch" in amd64) node=x64; bun=x64; dk=x86_64 ;; arm64) node=arm64; bun=aarch64; dk=aarch64 ;; *) echo "unsupported $arch"; exit 1 ;; esac; \
    curl -fsSL "https://download.docker.com/linux/static/stable/${dk}/docker-${DOCKER_VERSION}.tgz" | tar -xz -C /usr/local/bin --strip-components=1 docker/docker; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${node}.tar.xz" | tar -xJ -C /usr/local --strip-components=1; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" | tar -xz -C /usr/local; \
    curl -fsSL -o /tmp/bun.zip "https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-${bun}.zip"; \
    unzip -q /tmp/bun.zip -d /tmp && mv /tmp/bun-linux-*/bun /usr/local/bin/bun && rm -rf /tmp/bun*; \
    npm install -g "@anthropic-ai/claude-code@${CLAUDE_VERSION}"; \
    npm cache clean --force
# worktrunk (wt) makes every worktree an agent works in; the fleet's system
# config (home/worktrunk.toml) says where, locks them, and refuses merges.
# Its own skill comes from the same release's source. Pinned by checksum.
ARG WORKTRUNK_VERSION=0.81.0
ARG WORKTRUNK_SHA256_X86_64=0c253f216985ce9cd7b202a11606b5ce6aa934b1019fd1fbb9b7ce2b47b1c74a
ARG WORKTRUNK_SHA256_AARCH64=7a334d2e64c577afd0247e2411eff673360ab1f18897564f2d2e5d5374fbd74b
ARG WORKTRUNK_SOURCE_SHA256=a21258d4797149237b0c596d6a43d5ebdb28b573117d5b446b7376bb68bc404d
RUN set -eux; case "$(dpkg --print-architecture)" in \
      amd64) wk=x86_64; sum=$WORKTRUNK_SHA256_X86_64 ;; arm64) wk=aarch64; sum=$WORKTRUNK_SHA256_AARCH64 ;; *) exit 1 ;; esac; \
    rel="https://github.com/max-sixty/worktrunk/releases/download/v${WORKTRUNK_VERSION}"; \
    curl -fsSL -o /tmp/wt.tar.xz "$rel/worktrunk-${wk}-unknown-linux-musl.tar.xz"; \
    echo "$sum  /tmp/wt.tar.xz" | sha256sum -c -; \
    tar -xJf /tmp/wt.tar.xz -C /usr/local/bin --strip-components=1 "worktrunk-${wk}-unknown-linux-musl/wt" "worktrunk-${wk}-unknown-linux-musl/git-wt"; \
    curl -fsSL -o /tmp/wt-src.tar.gz "$rel/source.tar.gz"; \
    echo "$WORKTRUNK_SOURCE_SHA256  /tmp/wt-src.tar.gz" | sha256sum -c -; \
    mkdir -p /opt/worktrunk && tar -xzf /tmp/wt-src.tar.gz -C /opt/worktrunk --strip-components=3 "worktrunk-${WORKTRUNK_VERSION}/skills/worktrunk"; \
    rm -f /tmp/wt.tar.xz /tmp/wt-src.tar.gz; wt --version
# yarn and pnpm through corepack, which fetches the version a package.json pins.
RUN corepack enable

# You, inside: same uid, gid and HOME as on the host, so absolute paths under
# HOME (the shared code, Claude Code's per-project keys) match everywhere.
# /work/code is a sandbox agent's code volume: Docker gives a new volume its
# mount point's owner, so it is the agent's, not root's.
RUN set -eux; \
    getent group "$GID" >/dev/null || groupadd -g "$GID" "$USER_NAME"; \
    if getent passwd "$UID" >/dev/null; then userdel -r "$(getent passwd "$UID" | cut -d: -f1)" 2>/dev/null || true; fi; \
    useradd -u "$UID" -g "$GID" -d "$HOME_DIR" -m -s /bin/bash "$USER_NAME"; \
    mkdir -p "$HOME_DIR/.claude/sessions" "$HOME_DIR/.claude/fleet" /work/code /run/fleet; \
    chown -R "$UID:$GID" "$HOME_DIR" /work /run/fleet

COPY packaging/fleet/agent-run /opt/fleet/agent-run
COPY packaging/fleet/kbuild /usr/local/bin/kbuild
# git and gh as the repository owner's account (fleet.yaml github.owners); the
# gh wrapper comes before /usr/bin/gh on PATH.
COPY packaging/fleet/gh packaging/fleet/git-credential-fleet packaging/fleet/fleet-gh-token /usr/local/bin/
COPY packaging/fleet/home/status.sh packaging/fleet/home/event.sh packaging/fleet/home/worktree.sh packaging/fleet/home/settings.json /opt/fleet/
COPY packaging/fleet/home/gitconfig /etc/gitconfig
COPY packaging/fleet/home/worktrunk.toml /etc/xdg/worktrunk/config.toml
COPY packaging/fleet/tmux.conf /etc/tmux.conf
COPY packaging/fleet/skills /opt/fleet/skills
COPY --from=build /out/karmax /out/fleetctl /usr/local/bin/
# worktrunk's own skill beside the fleet's: agent-run copies them all in.
RUN cp -R /opt/worktrunk /opt/fleet/skills/worktrunk
RUN chmod 755 /opt/fleet/agent-run /opt/fleet/*.sh /usr/local/bin/kbuild \
      /usr/local/bin/gh /usr/local/bin/git-credential-fleet /usr/local/bin/fleet-gh-token \
 && cp /opt/fleet/settings.json "$HOME_DIR/.claude/settings.json" && chown "$UID:$GID" "$HOME_DIR/.claude/settings.json"

ENV HOME=${HOME_DIR} \
    DISABLE_AUTOUPDATER=1 \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
    TERM=xterm-256color \
    LANG=C.UTF-8 \
    XDG_RUNTIME_DIR=/run/fleet \
    PATH=/usr/local/go/bin:${HOME_DIR}/go/bin:${HOME_DIR}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
USER ${UID}:${GID}
WORKDIR /work
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/opt/fleet/agent-run"]
