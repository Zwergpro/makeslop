FROM golang:1.26-trixie

ENV HTTP_PROXY=http://192.168.1.201:7777 \
    HTTPS_PROXY=http://192.168.1.201:7777 \
    GOBIN=/usr/local/bin \
    GOPATH=/go \
    TZ="UTC" \
    LANG=en_US.utf-8 \
    LC_ALL=en_US.utf-8

RUN echo "LC_ALL=en_US.UTF-8" >> /etc/environment && \
    echo "en_US.UTF-8 UTF-8" >> /etc/locale.gen && \
    echo "LANG=en_US.UTF-8" > /etc/locale.conf

RUN mkdir -p /go/pkg/mod && chmod -R 0777 /go/pkg/mod

RUN apt-get update && apt-get install -y --no-install-recommends --no-install-suggests \
    less git sudo procps fzf zsh jq nano tmux gh unzip wget curl ca-certificates gnupg2 ripgrep \
  && curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
  && apt-get install -y --no-install-recommends nodejs \
  && npm install -g @openai/codex \
  && npm cache clean --force \
  && apt-get clean && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

RUN groupadd --gid 1000 user && \
    useradd --uid 1000 --gid user --create-home --shell /bin/bash user && \
    echo "user:password" | chpasswd && \
    usermod -aG sudo user && \
    mkdir /commandhistory && touch /commandhistory/.bash_history && \
    chown -R user /commandhistory && \
    mkdir -p /workspace /home/user/.claude && \
    chown -R user:user /workspace /home/user/.claude /go /usr/local/bin /tmp && \
    chmod g+s /workspace /home/user /go /usr/local/bin /tmp

ARG REVDIFF_VERSION=1.1.1
RUN wget "https://github.com/umputun/revdiff/releases/download/v${REVDIFF_VERSION}/revdiff_${REVDIFF_VERSION}_linux_amd64.deb" && \
    dpkg -i "revdiff_${REVDIFF_VERSION}_linux_amd64.deb"

WORKDIR /workspace
USER user

RUN go install github.com/umputun/ralphex/cmd/ralphex@latest  && \
    curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.11.4

ENV SHELL=/bin/zsh \
    EDITOR=vim \
    VISUAL=vim \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

ARG ZSH_IN_DOCKER_VERSION=1.2.1
RUN sh -c "$(wget -O- https://github.com/deluan/zsh-in-docker/releases/download/v${ZSH_IN_DOCKER_VERSION}/zsh-in-docker.sh)" -- \
  -p git \
  -p fzf \
  -a "source /usr/share/doc/fzf/examples/key-bindings.zsh" \
  -a "source /usr/share/doc/fzf/examples/completion.zsh" \
  -a "export PROMPT_COMMAND='history -a' && export HISTFILE=/commandhistory/.bash_history" \
  -x

RUN echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc

RUN curl -x "${HTTP_PROXY}" -fsSL https://claude.ai/install.sh | bash
