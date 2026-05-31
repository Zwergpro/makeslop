# syntax=docker/dockerfile:1
FROM debian:trixie-slim

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
    less git sudo procps fzf zsh jq nano tmux gh unzip wget curl ca-certificates gnupg2 ripgrep locales \
    && curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && echo "en_US.UTF-8 UTF-8" > /etc/locale.gen \
    && locale-gen \
    && update-locale LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8 \
    && rm -rf /tmp/* /var/tmp/*

ARG GO_VERSION=1.26.3
RUN ARCH=$(dpkg --print-architecture) \
    && curl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz -o /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz \
    && rm -rf /usr/local/go/test /usr/local/go/api /usr/local/go/doc \
              /usr/local/go/pkg/*/cmd /usr/local/go/.git* \
    && go version

RUN groupadd --gid 1000 user && \
    useradd --uid 1000 --gid user --create-home --shell /bin/bash user && \
    echo "user:password" | chpasswd && \
    usermod -aG sudo user && \
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
    VISUAL=vim

ARG ZSH_IN_DOCKER_VERSION=1.2.1
RUN sh -c "$(wget -O- https://github.com/deluan/zsh-in-docker/releases/download/v${ZSH_IN_DOCKER_VERSION}/zsh-in-docker.sh)" -- \
  -p git \
  -p fzf \
  -a 'eval "$(fzf --zsh)"' \
  -a "export PROMPT_COMMAND='history -a' && export HISTFILE=/commandhistory/.bash_history" \
  -x

RUN echo 'export PATH="$HOME/.npm-global/bin:/go/bin:$HOME/.local/bin:$PATH"' >> ~/.zshrc

RUN curl -fsSL https://claude.ai/install.sh | bash
