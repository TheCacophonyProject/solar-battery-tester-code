# Deploying the battery run summary web app

Sets up `webapp.py` on a server behind nginx, with:

- **gunicorn** running the app as a `systemd` service (auto-starts, restarts on failure)
- **nginx** as the public-facing reverse proxy
- **HTTP Basic Auth** (username/password) in front of the whole app, including the `/check` API
- **HTTPS via Let's Encrypt**, with HTTP forced to redirect to HTTPS

Assumes a Debian/Ubuntu server. Adjust package manager commands if you're on something else.

## Architecture

```
Browser / Postman --HTTPS--> nginx (:443, basic auth, TLS) --HTTP--> gunicorn (127.0.0.1:8000) --> webapp.py
```

gunicorn only ever listens on `127.0.0.1`, so the app is unreachable except through nginx -- there's no way to bypass the auth or the TLS termination.

## 0. Prerequisites

- A server with a public IP, reachable on ports 80 and 443 (check your cloud provider's firewall/security group, not just the server's own).
- A domain name (or subdomain) with an **A record pointing at the server's IP**. Let's Encrypt needs this to work before you request a certificate -- confirm with `dig +short battery.example.com` from your own machine.
- Root/sudo access on the server.

Throughout, replace `battery.example.com` with your real domain.

## 1. Get the code onto the server

```bash
sudo mkdir -p /opt/solar-battery-tester-code
sudo chown "$USER" /opt/solar-battery-tester-code
git clone <your-repo-url> /opt/solar-battery-tester-code
# or: scp -r solar-battery-tester-code/ you@server:/opt/
```

## 2. Python environment

```bash
cd /opt/solar-battery-tester-code/scripts
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

Quick sanity check it runs:

```bash
.venv/bin/gunicorn -w 1 -b 127.0.0.1:8000 webapp:app
# in another shell: curl -I http://127.0.0.1:8000/
# Ctrl-C once you see a 200
```

## 3. Dedicated service user

Don't run the app as root or as your login user. A system account with no shell and no home directory is enough:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin battery-summary
sudo chown -R battery-summary:battery-summary /opt/solar-battery-tester-code
```

## 4. systemd service

Copy the unit file and point it at your paths (it already assumes `/opt/solar-battery-tester-code` and the `battery-summary` user from the steps above -- edit `deploy/battery-summary.service` first if you used different ones):

```bash
sudo cp /opt/solar-battery-tester-code/scripts/deploy/battery-summary.service \
       /etc/systemd/system/battery-summary.service
sudo systemctl daemon-reload
sudo systemctl enable --now battery-summary
sudo systemctl status battery-summary
```

Logs: `sudo journalctl -u battery-summary -f`

Worker count (`-w 3` in the unit file) is a reasonable default for a small server; each worker generates one plot at a time (matplotlib rendering is CPU-bound), so scale it with `nproc` if you expect concurrent uploads.

## 5. Install nginx, certbot, and htpasswd tooling

```bash
sudo apt update
sudo apt install nginx certbot python3-certbot-nginx apache2-utils
```

## 6. Create the Basic Auth credentials

```bash
sudo htpasswd -c /etc/nginx/.htpasswd yourusername
# prompts for a password. For additional users, drop the -c (it overwrites the file).
```

## 7. Configure the nginx site

```bash
sudo cp /opt/solar-battery-tester-code/scripts/deploy/nginx-battery-summary.conf \
       /etc/nginx/sites-available/battery-summary
sudo nano /etc/nginx/sites-available/battery-summary   # set server_name to your real domain
sudo ln -s /etc/nginx/sites-available/battery-summary /etc/nginx/sites-enabled/
sudo nginx -t
sudo systemctl reload nginx
```

At this point `http://battery.example.com/` should prompt for the username/password you set in step 6, then show the upload page over plain HTTP. That's expected -- HTTPS comes next.

## 8. Get a Let's Encrypt certificate and force HTTPS

```bash
sudo certbot --nginx -d battery.example.com --redirect
```

`--redirect` makes certbot rewrite the port-80 server block to `return 301 https://...` instead of serving content, so HTTP is fully forced to HTTPS. Certbot edits `/etc/nginx/sites-available/battery-summary` in place, adding a second `server { listen 443 ssl; ... }` block with the certificate paths and keeping your `auth_basic` / `proxy_pass` directives from step 7. After it finishes, the file has roughly this shape:

```nginx
server {
    listen 80;
    server_name battery.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    server_name battery.example.com;

    ssl_certificate /etc/letsencrypt/live/battery.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/battery.example.com/privkey.pem;
    include /etc/letsencrypt/options-ssl-nginx.conf;
    ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem;

    client_max_body_size 100M;
    auth_basic "Battery run summary";
    auth_basic_user_file /etc/nginx/.htpasswd;

    location / {
        proxy_pass http://127.0.0.1:8000;
        ...
    }
}
```

Certbot installs a systemd timer that renews the cert automatically before it expires (~every 90 days). Confirm it's active and test the renewal path without actually renewing:

```bash
systemctl list-timers | grep certbot
sudo certbot renew --dry-run
```

## 9. Firewall

Only 80 and 443 need to be open publicly; gunicorn's 127.0.0.1:8000 is already unreachable from outside.

```bash
sudo ufw allow 'Nginx Full'   # opens 80 + 443
sudo ufw enable               # if not already enabled
sudo ufw status
```

## 10. Verify

```bash
curl -I http://battery.example.com/          # expect 301 -> https
curl -I https://battery.example.com/         # expect 401 (no credentials yet)
curl -I -u yourusername https://battery.example.com/   # expect 200
```

In a browser, `https://battery.example.com/` should prompt for the username/password, then show the upload form.

### Using the `/check` API (e.g. from Postman)

Every route is behind Basic Auth now, including `/check`, so requests need credentials in addition to the file:

- Postman: **Authorization** tab -> type **Basic Auth** -> your username/password (Postman adds the `Authorization` header for you).
- curl equivalent:
  ```bash
  curl -u yourusername -F "zipfile=@run.zip" https://battery.example.com/check
  ```

## Updating the app later

```bash
cd /opt/solar-battery-tester-code
sudo -u battery-summary git pull      # or re-copy files
sudo systemctl restart battery-summary
```

nginx config changes: edit the file in `/etc/nginx/sites-available/`, then `sudo nginx -t && sudo systemctl reload nginx`.
