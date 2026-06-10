# syntax=docker/dockerfile:1
FROM debian:trixie-slim@sha256:b6e2a152f22a40ff69d92cb397223c906017e1391a73c952b588e51af8883bf8

ARG HTTP_PROXY=""
ARG HTTPS_PROXY=""

ENV GOBIN=/go/bin \
    GOPATH=/go \
    PATH=/usr/local/go/bin:/go/bin:$PATH \
    TZ="UTC" \
    LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8

RUN mkdir -p /go/pkg/mod

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends --no-install-suggests \
    less git procps fzf zsh jq nano tmux gh unzip wget curl ca-certificates gnupg2 ripgrep locales xz-utils \
    && echo "en_US.UTF-8 UTF-8" > /etc/locale.gen \
    && locale-gen \
    && update-locale LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8 \
    && rm -rf /tmp/* /var/tmp/*

ARG GO_VERSION=1.26.4
RUN ARCH=$(dpkg --print-architecture) \
    && case "${ARCH}" in \
         amd64) GO_SHA256="1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f" ;; \
         arm64) GO_SHA256="ef758ae7c6cf9267c9c0ef080b8965f453d89ab2d25d9eb22de4405925238768" ;; \
         *)     echo "Unsupported arch: ${ARCH}" && exit 1 ;; \
       esac \
    && curl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz -o /tmp/go.tgz \
    && echo "${GO_SHA256}  /tmp/go.tgz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz \
    && rm -rf /usr/local/go/test /usr/local/go/api /usr/local/go/doc \
        /usr/local/go/pkg/*/cmd /usr/local/go/.git* \
    && go version

ARG NODE_VERSION=24.16.0
RUN ARCH=$(dpkg --print-architecture) \
    && case "${ARCH}" in \
         amd64) NODE_ARCH="x64"  ; NODE_SHA256="2faf6a387e9b62b888e21c54f01249fb27537ffecf1842f29f4c919d0a59a0ff" ;; \
         arm64) NODE_ARCH="arm64"; NODE_SHA256="589f5b6dd4fcfee4dfda73013903c966abaa8abd93dbc9d436544e472b4f0e74" ;; \
         *)     echo "Unsupported arch: ${ARCH}" && exit 1 ;; \
       esac \
    && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz" -o /tmp/node.txz \
    && echo "${NODE_SHA256}  /tmp/node.txz" | sha256sum -c - \
    && tar -C /usr/local --strip-components=1 -xJf /tmp/node.txz \
    && rm /tmp/node.txz \
    && node --version \
    && npm --version

RUN groupadd --gid 1000 user && \
    useradd --uid 1000 --gid user --create-home --shell /bin/bash user && \
    mkdir /commandhistory && touch /commandhistory/.bash_history && \
    chown -R user /commandhistory && \
    mkdir -p /workspace /home/user/.claude && \
    chown -R user:user /workspace /home/user/.claude /go && \
    chmod g+s /workspace /home/user /go

WORKDIR /workspace
USER user

RUN --mount=type=cache,target=/home/user/.npm,uid=1000,gid=1000 \
    mkdir -p ~/.npm-global && npm config set prefix ~/.npm-global && \
    npm install -g @openai/codex

ENV SHELL=/bin/zsh \
    EDITOR=vim \
    VISUAL=vim \
    POWERLEVEL9K_DISABLE_GITSTATUS=true

ARG ZSH_IN_DOCKER_VERSION=1.2.1
ARG ZSH_IN_DOCKER_SHA256=0cf15b6f9c63a8103eb33bad87dc4b43fed0e0ad5884dff90f943ee1e868796e
RUN wget -O /tmp/zsh-in-docker.sh "https://github.com/deluan/zsh-in-docker/releases/download/v${ZSH_IN_DOCKER_VERSION}/zsh-in-docker.sh" \
    && echo "${ZSH_IN_DOCKER_SHA256}  /tmp/zsh-in-docker.sh" | sha256sum -c - \
    && sh /tmp/zsh-in-docker.sh -- \
    -p git \
    -p fzf \
    -a 'eval "$(fzf --zsh)"' \
    -a "export PROMPT_COMMAND='history -a' && export HISTFILE=/commandhistory/.bash_history" \
    -x \
    && rm /tmp/zsh-in-docker.sh

RUN echo 'export PATH="$HOME/.npm-global/bin:/go/bin:$HOME/.local/bin:$PATH"' >> ~/.zshrc

RUN curl -fsSL https://claude.ai/install.sh | bash
