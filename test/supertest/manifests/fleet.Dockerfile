# The fleet a supertest run manages: the smallest thing that is honestly a server. Real sshd,
# real python for Ansible modules, a real non-root account, and nothing else. If SwitchTender
# can run a playbook against three of these inside a fresh Kind cluster, the demonstration is a
# deployment, not a mock.
FROM alpine:3.22
RUN apk add --no-cache openssh-server python3 \
    && ssh-keygen -A \
    && adduser -D ops \
    # Alpine locks a new account (! in the shadow field), and sshd refuses locked accounts even
    # for public-key auth. * means no password and no lock, which is exactly the posture: keys
    # only.
    && sed -i 's/^ops:!/ops:*/' /etc/shadow \
    && mkdir -p /etc/ssh/auth \
    # Replace the stock directive rather than appending an override: sshd honors the FIRST
    # AuthorizedKeysFile it reads, and alpine ships one uncommented, so an appended line is dead
    # text and every key is silently refused.
    && sed -i 's|^AuthorizedKeysFile.*|AuthorizedKeysFile /etc/ssh/authorized_keys|' \
        /etc/ssh/sshd_config \
    && printf 'PasswordAuthentication no\n' >> /etc/ssh/sshd_config
# The key arrives as a Kubernetes secret mounted at /etc/ssh/auth, and a secret mount is a
# world-writable tmpfs directory, which sshd's StrictModes path check rightly refuses. The boot
# script copies the key to a root-owned path with sane permissions instead of turning that check
# off: a test fleet that relaxes sshd's hygiene to pass is demonstrating the wrong thing.
RUN printf '#!/bin/sh\nset -e\ncp /etc/ssh/auth/authorized_keys /etc/ssh/authorized_keys\nchmod 644 /etc/ssh/authorized_keys\nexec /usr/sbin/sshd -D -e\n' \
        > /usr/local/bin/boot \
    && chmod +x /usr/local/bin/boot
EXPOSE 22
CMD ["/usr/local/bin/boot"]
