#!/bin/bash
set -e

CONF_DIR=/etc/solar-battery-tester
CONF=$CONF_DIR/config.toml

# Runs live here until the server has them; the tester creates this itself, but
# making it now means a fresh install has somewhere to write from the first run.
mkdir -p /var/lib/solar-battery-tester/data
mkdir -p "$CONF_DIR"

# The config is created once and never overwritten, so an upgrade drops in a
# fresh config.toml.example but leaves a tester's own settings alone.
if [[ ! -f $CONF ]]; then
    if [[ -f $CONF_DIR/server.env ]]; then
        # Carry over the settings from the environment file this replaced, so a
        # tester that was already uploading keeps doing so after the upgrade.
        # shellcheck disable=SC1091
        . "$CONF_DIR/server.env"
        esc() { printf '%s' "${1-}" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
        {
            echo "# Carried over from server.env by the package upgrade."
            echo "# See config.toml.example for what else can go in here."
            echo "[server]"
            printf 'url = "%s"\n' "$(esc "$BATTERY_SERVER_URL")"
            printf 'username = "%s"\n' "$(esc "$BATTERY_SERVER_USERNAME")"
            printf 'password = "%s"\n' "$(esc "$BATTERY_SERVER_PASSWORD")"
        } > "$CONF"
        mv "$CONF_DIR/server.env" "$CONF_DIR/server.env.replaced-by-config.toml"
        echo "Moved the old server.env settings into $CONF."
    elif [[ -f $CONF_DIR/config.toml.example ]]; then
        # The example's comments, with its values emptied: a tester that hasn't
        # been pointed at a server yet must not start posting runs at the
        # example URL. It keeps its runs locally until someone fills this in.
        sed 's/^\(url\|username\|password\) = .*/\1 = ""/' \
            "$CONF_DIR/config.toml.example" > "$CONF"
    else
        printf '[server]\nurl = ""\nusername = ""\npassword = ""\n' > "$CONF"
    fi
fi

# It holds a password, so it's readable only by root (which the tester runs as).
chmod 600 "$CONF"

systemctl restart solar-battery-tester

echo "Post-installation script finished."
