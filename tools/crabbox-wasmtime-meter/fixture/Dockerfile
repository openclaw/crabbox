# Same reviewed Ubuntu 26.04 multi-architecture index as Crabbox's OS catalog.
FROM docker.io/library/ubuntu@sha256:2260313b31c8c011cd2eebe728008efac1b3982be73eb71348ea2648d2c0e09b
RUN apt-get update && apt-get install -y --no-install-recommends \
    openssh-server ca-certificates git rsync curl sudo python3 util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 60001 meter \
    && useradd --uid 60001 --gid 60001 --no-create-home --shell /usr/sbin/nologin meter \
    && passwd --lock meter \
    && install -d -m 0711 /run/meter-fixture \
    && printf 'fixture controller sentinel\n' > /root/controller-sentinel \
    && printf 'fixture key-access sentinel, not a key\n' > /run/fixture-key-sentinel \
    && chmod 0600 /root/controller-sentinel /run/fixture-key-sentinel
COPY crabbox-wasmtime-meter fixture-child /usr/local/bin/
COPY supervise.py /usr/local/bin/meter-supervise
RUN chmod 0755 /usr/local/bin/crabbox-wasmtime-meter /usr/local/bin/fixture-child \
    /usr/local/bin/meter-supervise
