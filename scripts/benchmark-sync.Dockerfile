FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache openssh rsync bash coreutils perl procps git tar gzip curl util-linux findutils \
    && passwd -d root && mkdir -p /root/.ssh /run/sshd && chmod 700 /root/.ssh
CMD ["sh", "-c", "ssh-keygen -A && exec /usr/sbin/sshd -D -e -o PasswordAuthentication=no -o PermitRootLogin=prohibit-password"]
