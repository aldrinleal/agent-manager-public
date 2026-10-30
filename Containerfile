# Static binary, so there is nothing to link against and no shell to exploit.
#
# Deliberately no openssh-client: sshx speaks SSH natively (x/crypto/ssh), so
# the image needs no ssh binary and no ~/.ssh. The only thing it needs from
# the filesystem is the key, which arrives from a k8s Secret as an env var.
FROM docker.io/library/alpine:3.21 AS certs

RUN apk add --no-cache ca-certificates tzdata

FROM scratch

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=certs /usr/share/zoneinfo /usr/share/zoneinfo

COPY bin/agentmgrd /agentmgrd
COPY bin/agentmgr  /agentmgr

ENV AGENTMGR_PORT=8000 \
    AGENTMGR_INVENTORY=/etc/agentmgr/hosts.yaml \
    AGENTMGR_DB_PATH=/data/agentmgr.db \
    AGENTMGR_KNOWN_HOSTS=/data/known_hosts \
    GIN_MODE=release

EXPOSE 8000

ENTRYPOINT ["/agentmgrd"]
