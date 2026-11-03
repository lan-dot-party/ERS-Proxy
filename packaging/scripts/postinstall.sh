#!/bin/sh
# ERS-Proxy post-installation script (deb, rpm, archlinux, apk).
# Creates the system user, the data directory and secret.key. The service is
# not started automatically: the admin configures it first.
set -e

if ! id -u ers-proxy >/dev/null 2>&1; then
    if command -v systemd-sysusers >/dev/null 2>&1 && [ -f /usr/lib/sysusers.d/ers-proxy.conf ]; then
        systemd-sysusers /usr/lib/sysusers.d/ers-proxy.conf
    elif command -v useradd >/dev/null 2>&1; then
        useradd --system --home-dir /var/lib/ers-proxy --no-create-home \
            --shell /usr/sbin/nologin --user-group ers-proxy 2>/dev/null ||
            useradd --system --home-dir /var/lib/ers-proxy --no-create-home --shell /sbin/nologin ers-proxy
    else
        addgroup -S ers-proxy 2>/dev/null || true
        adduser -S -D -H -h /var/lib/ers-proxy -s /sbin/nologin -G ers-proxy ers-proxy
    fi
fi

mkdir -p /var/lib/ers-proxy /etc/ers-proxy
chown ers-proxy:ers-proxy /var/lib/ers-proxy
chmod 0700 /var/lib/ers-proxy
chown root:ers-proxy /etc/ers-proxy
chmod 0750 /etc/ers-proxy
chown root:ers-proxy /etc/ers-proxy/config.yaml
chmod 0640 /etc/ers-proxy/config.yaml

# The key encrypts switch passwords in the database. It is created
# once and never replaced by updates.
if [ ! -s /etc/ers-proxy/secret.key ]; then
    umask 027
    od -An -tx1 -N32 /dev/urandom | tr -d ' \n' > /etc/ers-proxy/secret.key
    echo >> /etc/ers-proxy/secret.key
    chown root:ers-proxy /etc/ers-proxy/secret.key
    chmod 0640 /etc/ers-proxy/secret.key
fi

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
    # Updates restart a running service; migrations run on start.
    systemctl try-restart ers-proxy.service || true
fi

if [ ! -f /var/lib/ers-proxy/ers-proxy.db ]; then
    cat <<'EOF'

ERS-Proxy is installed. Next steps:
  1. Check the configuration: sudo nano /etc/ers-proxy/config.yaml   (listen, public_url)
  2. Create the first admin:  sudo ers-proxy user create admin --role admin
  3. Start the service:       sudo systemctl enable --now ers-proxy
  4. Log in and add the first switch under Settings → Devices.
     Behind a reverse proxy: see /usr/share/doc/ers-proxy/README.md (public_url!)

EOF
fi
exit 0
