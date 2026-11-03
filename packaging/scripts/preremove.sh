#!/bin/sh
# ERS-Proxy pre-removal script. On an update the service keeps running and
# postinstall restarts it; only a real removal stops it.
#   deb: $1 = remove | upgrade | deconfigure
#   rpm: $1 = 0 (remove) | 1 (upgrade)
#   archlinux/apk: called on removal only
set -e

case "$1" in
    upgrade | 1) exit 0 ;;
esac

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl disable --now ers-proxy.service >/dev/null 2>&1 || true
fi
exit 0
