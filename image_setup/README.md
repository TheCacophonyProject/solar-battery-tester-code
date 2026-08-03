# Solar Battery Tester RPi setup

These are instructions to get the salt tester working on a Raspberry Pi 3B+ with Raspberry Pi OS 13 (trixie) installed.

## Hostname generator

Each Pi gets a random hostname in the format `bt-<nnnn>` (e.g. `bt-1234`) on
its first boot, so that flashing the same image onto many SD cards doesn't
give every device the same identity.

This is done with a first-boot systemd oneshot service: it generates the
hostname, sets it, writes `/etc/salt/minion_id` so the minion picks up the
same id, regenerates unique SSH host keys, then disables itself so it never
runs again.

The image ships with no SSH host keys (they're cleared before capturing the
image, see below), so this script is also responsible for generating them —
otherwise `ssh.service` won't start at all, and if we instead left the keys
in the image, every Pi flashed from it would share the exact same host key
(a "REMOTE HOST IDENTIFICATION HAS CHANGED" trap waiting to happen, and a
real security problem since one leaked key would compromise every device).

`/usr/local/sbin/firstboot-hostname.sh`:

```bash
#!/bin/bash
set -euo pipefail

PREFIX="bt"
SUFFIX=$(od -An -N2 -tu2 /dev/urandom | tr -d ' ')
NEW_HOSTNAME=$(printf "%s-%04d" "$PREFIX" "$((SUFFIX % 10000))")

hostnamectl set-hostname "$NEW_HOSTNAME"

# Keep /etc/hosts consistent
sed -i "s/^127\.0\.1\.1.*/127.0.1.1\t${NEW_HOSTNAME}/" /etc/hosts
grep -q "127.0.1.1" /etc/hosts || echo -e "127.0.1.1\t${NEW_HOSTNAME}" >> /etc/hosts

# Salt minion id explicit rather than relying on hostname resolution
echo "$NEW_HOSTNAME" > /etc/salt/minion_id

# Generate this device's own unique SSH host keys (image ships with none)
ssh-keygen -A
systemctl restart ssh

systemctl disable firstboot-hostname.service
```

`chmod +x /usr/local/sbin/firstboot-hostname.sh`

`/etc/systemd/system/firstboot-hostname.service`:

```ini
[Unit]
Description=First boot: assign random hostname and SSH host keys
Before=salt-minion.service ssh.service
ConditionPathExists=!/var/lib/firstboot-hostname.done

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/firstboot-hostname.sh
ExecStartPost=/usr/bin/touch /var/lib/firstboot-hostname.done
RemainAfterExit=true

[Install]
WantedBy=multi-user.target
```

`Before=ssh.service` matters here: `ssh.service` normally starts early in
boot, well before `multi-user.target`. Without this ordering, `sshd` would
try to start before our script has generated any host keys and fail outright
— which is exactly the "removing `ssh_host_*` broke SSH" symptom. With the
ordering in place, `ssh.service` waits for this oneshot to finish first.

Enable it with `sudo systemctl enable firstboot-hostname.service`.

**Before capturing the image**, clear the machine-specific state so every
flashed card is unique:

```bash
sudo rm -f /var/lib/firstboot-hostname.done
sudo systemctl enable firstboot-hostname.service
sudo systemctl stop salt-minion
sudo rm -f /etc/salt/pki/minion/minion.{pem,pub} /etc/salt/pki/minion/minion_master.pub
sudo rm -f /etc/salt/minion_id
sudo rm -f /etc/ssh/ssh_host_*
sudo truncate -s 0 /etc/machine-id
sudo rm -f /var/lib/dbus/machine-id
```

The `systemctl enable` step is essential here: `firstboot-hostname.sh` disables
its own unit as its last action once it has run, so after a normal boot the
service is disabled, not just "already satisfied". Clearing the `.done`
marker alone isn't enough to make it run again — the unit has to be
re-enabled too, otherwise systemd never queues it at boot regardless of the
marker file.

## Installing salt (optional)

We want to install salt so the test devices can be updated/checked remotely.

We want salt to wait until we have the random hostname setup so we don't get
conflicts in the names — this is handled by the fleet's usual convention of
gating `salt-minion.service` on `/etc/salt/minion_id` existing (see
`saltops/pi/salt-minion/override.conf`), rather than a hostname race.

> Note: these Pis run Debian 13 (trixie), which is newer than Salt's official
> support matrix. There's no trixie-specific repo from Broadcom yet, so we
> use the standard `salt.sources` file (which targets bookworm/12) — it works
> because Salt's packages are largely codename-agnostic. `dpkg --print-architecture`
> should report `arm64`.

To install salt-minion:

```bash
sudo mkdir -m 755 -p /etc/apt/keyrings

curl -fsSL https://packages.broadcom.com/artifactory/api/security/keypair/SaltProjectKey/public \
  | sudo tee /etc/apt/keyrings/salt-archive-keyring.pgp

curl -fsSL https://github.com/saltstack/salt-install-guide/releases/latest/download/salt.sources \
  | sudo tee /etc/apt/sources.list.d/salt.sources

sudo apt update
sudo apt install salt-minion
```

Point it at the Cacophony salt master
(`/etc/salt/minion.d/cacophony.conf`):

```yaml
master: salt.cacophony.org.nz
publish_port: 4507
master_port: 4508
```

Create the directory and file, then restart the minion to pick it up:

```bash
sudo mkdir -p /etc/salt/minion.d
sudo tee /etc/salt/minion.d/cacophony.conf > /dev/null <<'EOF'
master: salt.cacophony.org.nz
publish_port: 4505
master_port: 4506
EOF
```

Gate the minion on the hostname/minion_id being set
(`/etc/systemd/system/salt-minion.service.d/override.conf`):

```ini
[Unit]
ConditionPathExists=/etc/salt/minion_id
```

Then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable salt-minion
```
(Leave it stopped for now — `firstboot-hostname.service` starts the chain on
first real boot, since `salt-minion` won't pass its `ConditionPathExists`
until `minion_id` exists.)

