FROM alpine:3.22
RUN apk add --no-cache git \
    && git config --global user.email it@example.invalid && git config --global user.name it \
    && git init -q -b main /tmp/src && echo hello >/tmp/src/README \
    && git -C /tmp/src add README && git -C /tmp/src commit -qm init \
    && git clone -q --bare /tmp/src /srv/main.git && rm -rf /tmp/src
COPY --chmod=0755 fake-claude.sh /usr/local/bin/fake-claude
