#!/bin/sh
# ERS-Proxy post-removal script. Data in /var/lib/ers-proxy and the
# configuration are kept on removal; "apt purge" deletes them.
set -e

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

if [ "$1" = "purge" ]; then
    rm -rf /var/lib/ers-proxy /etc/ers-proxy
    if id -u ers-proxy >/dev/null 2>&1; then
        userdel ers-proxy 2>/dev/null || true
    fi
fi
exit 0
