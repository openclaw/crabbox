FROM alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f
RUN apk add --no-cache --upgrade ca-certificates-bundle=20261020-r0
RUN apk add --no-cache openssh rsync bash coreutils perl procps git tar gzip curl util-linux findutils \
    && passwd -d root && mkdir -p /root/.ssh /run/sshd && chmod 700 /root/.ssh
CMD ["sh", "-c", "ssh-keygen -A && exec /usr/sbin/sshd -D -e -o PasswordAuthentication=no -o PermitRootLogin=prohibit-password"]
