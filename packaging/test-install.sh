#!/bin/sh
# Installs a package in a clean container, starts ERS-Proxy with the packaged
# configuration, checks /health and removes the package again.
#
#   sh packaging/test-install.sh deb|rpm|archlinux|apk PACKAGE_FILE
set -eu

format=$1
pkg=$2

fail() {
    echo "FAIL: $*" >&2
    [ -f /tmp/ers-proxy.log ] && cat /tmp/ers-proxy.log >&2
    exit 1
}

echo "== install $pkg"
case "$format" in
    deb) apt-get install -y "$pkg" ;;
    rpm) if command -v dnf >/dev/null; then dnf install -y "$pkg"; else rpm -i "$pkg"; fi ;;
    archlinux) pacman -U --noconfirm "$pkg" ;;
    apk) apk add --allow-untrusted "$pkg" ;;
    *) fail "unknown format $format" ;;
esac

echo "== installed files"
ers-proxy version
id ers-proxy || fail "user ers-proxy missing"
[ -f /etc/ers-proxy/config.yaml ] || fail "config missing"
[ -s /etc/ers-proxy/secret.key ] || fail "secret.key missing"
[ "$(stat -c %a /etc/ers-proxy/secret.key)" = 640 ] || fail "secret.key mode is not 640"
[ "$(stat -c %U /var/lib/ers-proxy)" = ers-proxy ] || fail "/var/lib/ers-proxy not owned by ers-proxy"
[ -f /usr/lib/systemd/system/ers-proxy.service ] || fail "systemd unit missing"

echo "== admin user (as root, files must stay usable for the service)"
echo "smoke-test-password" | ers-proxy -c /etc/ers-proxy/config.yaml user create admin --role admin
[ "$(stat -c %U /var/lib/ers-proxy/ers-proxy.db)" = ers-proxy ] || fail "database owned by root after CLI use"

echo "== start server as ers-proxy"
su -s /bin/sh ers-proxy -c 'LANLEECHER_CONFIG=/etc/ers-proxy/config.yaml /usr/bin/ers-proxy server' \
    >/tmp/ers-proxy.log 2>&1 &
pid=$!
ok=
for _ in $(seq 1 40); do
    if out=$(wget -q -O - http://127.0.0.1:9050/health 2>/dev/null || curl -fsS http://127.0.0.1:9050/health 2>/dev/null); then
        [ "$out" = ok ] && ok=1 && break
    fi
    sleep 0.5
done
[ -n "$ok" ] || fail "server did not become healthy"
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:9050/login 2>/dev/null || echo 200)
[ "$code" = 200 ] || fail "/login returned $code"
kill "$pid"
wait "$pid" 2>/dev/null || true

echo "== remove"
case "$format" in
    deb) apt-get remove -y ers-proxy ;;
    rpm) if command -v dnf >/dev/null; then dnf remove -y ers-proxy; else rpm -e ers-proxy; fi ;;
    archlinux) pacman -R --noconfirm ers-proxy ;;
    apk) apk del ers-proxy ;;
esac
# Check the file, not `command -v`: the shell has cached the path since
# `ers-proxy version` above and would still report the removed binary.
[ ! -e /usr/bin/ers-proxy ] || fail "binary still installed"
[ -f /var/lib/ers-proxy/ers-proxy.db ] || fail "data was deleted on removal"
echo "OK: $pkg"
