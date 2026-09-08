# syntax=docker/dockerfile:1.7

# Koyeb publishes this release for linux/amd64 only. Keep the digest tied to
# sandbox-container v0.0.22; Koyeb injects and starts sandbox-executor as PID 1.
FROM --platform=linux/amd64 docker.io/koyeb/sandbox@sha256:64d478144162b02c919ba58de54a1942274ebca71fe43c046a18616015f4591e

ARG TAILSCALE_VERSION=1.102.3
ARG TAILSCALE_SHA256=36ddd9b51be57ffc2990cf76323cfa13643bfbb1b8a969f6183fa164741cdef5
ARG CODE_SERVER_VERSION=4.136.2
ARG CODE_SERVER_SHA256=83cf05cf4013da071bffade5fe207865c80966231e4f3cf5946aff3e15a966ec
ARG NODE_VERSION=24.20.0
ARG NODE_AMD64_SHA256=855d581f8a4eb1a8117e3426de25fe02770592febcfb31369aee1ffbfee9e8ec
ARG GOOGLE_CHROME_VERSION=152.0.7977.82-1
ARG GOOGLE_CHROME_SHA256=4d25e4a028c78a7ae910683551c2f234792cc5595e7e3e34939f599342ada446

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN set -eux; \
    test "$(dpkg --print-architecture)" = amd64; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      dbus-x11 \
      fonts-dejavu-core \
      fonts-liberation \
      iproute2 \
      jq \
      libatomic1 \
      netcat-openbsd \
      openssh-client \
      openssh-server \
      openssl \
      procps \
      rsync \
      scrot \
      tigervnc-tools \
      tmux \
      util-linux \
      x11-utils \
      x11vnc \
      x11-xserver-utils \
      xauth \
      xclip \
      xdotool \
      xsel \
      xterm \
      xvfb \
      xfce4-panel \
      xfce4-session \
      xfce4-settings \
      xfce4-terminal \
      xfconf \
      xfdesktop4 \
      xfwm4; \
    rm -f /etc/ssh/ssh_host_*; \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    curl -fsSL "https://pkgs.tailscale.com/stable/tailscale_${TAILSCALE_VERSION}_amd64.tgz" -o /tmp/tailscale.tgz; \
    printf '%s  %s\n' "${TAILSCALE_SHA256}" /tmp/tailscale.tgz | sha256sum -c -; \
    mkdir -p /tmp/tailscale; \
    tar -xzf /tmp/tailscale.tgz -C /tmp/tailscale --strip-components=1 --no-same-owner; \
    install -D -m 0755 /tmp/tailscale/tailscale /usr/local/libexec/crabbox-koyeb-sandbox/tailscale; \
    install -m 0755 /tmp/tailscale/tailscaled /usr/local/sbin/tailscaled; \
    rm -rf /tmp/tailscale /tmp/tailscale.tgz

RUN set -eux; \
    curl -fsSL \
      "https://github.com/coder/code-server/releases/download/v${CODE_SERVER_VERSION}/code-server-${CODE_SERVER_VERSION}-linux-amd64.tar.gz" \
      -o /tmp/code-server.tgz; \
    printf '%s  %s\n' "${CODE_SERVER_SHA256}" /tmp/code-server.tgz | sha256sum -c -; \
    mkdir -p /tmp/code-server; \
    tar -xzf /tmp/code-server.tgz -C /tmp/code-server --strip-components=1 --no-same-owner; \
    rm -f /tmp/code-server.tgz; \
    install -d -m 0755 /usr/local/lib/code-server; \
    cp -a /tmp/code-server/. /usr/local/lib/code-server/; \
    chmod 0755 /usr/local/lib/code-server; \
    ln -s /usr/local/lib/code-server/bin/code-server /usr/local/bin/code-server; \
    XDG_CONFIG_HOME=/tmp/code-server-config /usr/local/bin/code-server --version; \
    rm -rf /tmp/code-server /tmp/code-server-config

RUN set -eux; \
    curl -fsSL "https://dl.google.com/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_${GOOGLE_CHROME_VERSION}_amd64.deb" -o /tmp/google-chrome.deb; \
    printf '%s  %s\n' "${GOOGLE_CHROME_SHA256}" /tmp/google-chrome.deb | sha256sum -c -; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends /tmp/google-chrome.deb; \
    rm -f /tmp/google-chrome.deb \
      /etc/apt/sources.list.d/google-chrome.list \
      /etc/apt/sources.list.d/google-chrome.sources; \
    rm -rf /var/lib/apt/lists/*; \
    install -d -m 0755 /etc/opt/chrome/policies/managed; \
    printf '%s\n' '{"DefaultBrowserSettingEnabled":false,"MetricsReportingEnabled":false,"PromotionalTabsEnabled":false}' \
      >/etc/opt/chrome/policies/managed/crabbox.json

RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential \
      git \
      python3 \
      python3-pip \
      python3-venv \
      unzip \
      zip; \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz" \
      -o /tmp/node.tar.gz; \
    printf '%s  %s\n' "${NODE_AMD64_SHA256}" /tmp/node.tar.gz | sha256sum -c -; \
    tar -xzf /tmp/node.tar.gz -C /usr/local --strip-components=1 --no-same-owner; \
    rm -f /tmp/node.tar.gz; \
    test "$(/usr/local/bin/node --version)" = "v${NODE_VERSION}"

# sshd rejects accounts whose shadow entry starts with ! or * before it can
# evaluate a public key. Use an invalid hash; runtime password auth stays off.
RUN set -eux; \
    if ! getent group crabbox >/dev/null; then groupadd --system crabbox; fi; \
    if ! id crabbox >/dev/null 2>&1; then \
      useradd --create-home --gid crabbox --shell /bin/bash crabbox; \
    fi; \
    usermod --password 'x' crabbox; \
    install -d -m 0755 -o crabbox -g crabbox /workspace /workspace/crabbox; \
    install -d -m 0700 -o root -g root /var/lib/crabbox-koyeb; \
    install -d -m 0750 -o root -g crabbox /var/lib/crabbox

COPY --chmod=0755 bootstrap.sh /usr/local/libexec/crabbox-koyeb-sandbox/bootstrap.sh
COPY --chmod=0755 desktop-session.sh /usr/local/libexec/crabbox-koyeb-sandbox/desktop-session.sh
COPY --chmod=0755 healthcheck.sh /usr/local/libexec/crabbox-koyeb-sandbox/healthcheck.sh
COPY --chmod=0755 teardown.sh /usr/local/libexec/crabbox-koyeb-sandbox/teardown.sh
COPY --chmod=0755 crabbox-browser /usr/local/bin/crabbox-browser
COPY --chmod=0755 tailscale-client /usr/local/bin/tailscale
COPY --chmod=0755 crabbox-worker-browser /usr/local/bin/crabbox-worker-browser
COPY --chmod=0755 crabbox-worker-terminal /usr/local/bin/crabbox-worker-terminal

RUN ln -s /usr/local/libexec/crabbox-koyeb-sandbox/healthcheck.sh /usr/local/bin/crabbox-ready \
    && ln -s /usr/local/libexec/crabbox-koyeb-sandbox/bootstrap.sh /usr/local/bin/crabbox-koyeb-bootstrap \
    && ln -s /usr/local/libexec/crabbox-koyeb-sandbox/teardown.sh /usr/local/bin/crabbox-koyeb-teardown

WORKDIR /workspace/crabbox

# Do not add ENTRYPOINT, CMD, EXPOSE 22, or EXPOSE 5900. The Koyeb Sandbox
# platform owns PID 1 and its authenticated management ports. Bootstrap is
# invoked as a short executor /run call; only Tailscale Serve publishes SSH.
