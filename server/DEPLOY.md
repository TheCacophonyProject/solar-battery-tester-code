# Deploying the battery run web app

Sets up `webapp.py` on a server behind nginx, with:

- **gunicorn** running the app as a `systemd` service (auto-starts, restarts on failure)
- **nginx** as the public-facing reverse proxy
- **HTTP Basic Auth** (username/password) in front of the whole app, including the API the testers post to
- **HTTPS via Let's Encrypt**, with HTTP forced to redirect to HTTPS
- **an archive of every run**, stored on the server under the battery's ID and browsable in the page

Assumes a Debian/Ubuntu server. Adjust package manager commands if you're on something else.

## Architecture

```text
Tester Pi (bt-nnnn) --HTTPS POST /runs --\
                                          >-- nginx (:443, basic auth, TLS) --HTTP--> gunicorn (127.0.0.1:8000) --> webapp.py
Browser / Postman   --HTTPS-------------/                                                                             |
                                                                                              /var/lib/battery-summary/runs
```

gunicorn only ever listens on `127.0.0.1`, so the app is unreachable except through nginx -- there's no way to bypass the auth or the TLS termination.

Each tester finishes a battery, zips the run locally, and posts it to `/runs`. The server stores the zip, applies the same pass/fail checks as `plot_results.py`, and hands the verdict back to the tester (which logs it). Nothing is written to a USB drive at either end.

A run zip uploaded through the page's own form is filed in exactly the same way, so a run you have a zip of but the server doesn't -- one off a tester that was offline, say -- can be added by hand and is then browsable like any other.

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
cd /opt/solar-battery-tester-code/server
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
sudo cp /opt/solar-battery-tester-code/server/deploy/battery-summary.service \
       /etc/systemd/system/battery-summary.service
sudo systemctl daemon-reload
sudo systemctl enable --now battery-summary
sudo systemctl status battery-summary
```

Logs: `sudo journalctl -u battery-summary -f`

Worker count (`-w 3` in the unit file) is a reasonable default for a small server; each worker generates one plot at a time (matplotlib rendering is CPU-bound), so scale it with `nproc` if you expect concurrent uploads. A summary image is drawn once, while the run is being stored, and kept next to its zip, so browsing the archive costs nothing afterwards. (A run stored before that was the case, or whose render failed, is drawn the first time it's viewed instead.)

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
sudo cp /opt/solar-battery-tester-code/server/deploy/nginx-battery-summary.conf \
       /etc/nginx/sites-available/battery-summary
sudo nano /etc/nginx/sites-available/battery-summary   # set server_name to your real domain
sudo ln -s /etc/nginx/sites-available/battery-summary /etc/nginx/sites-enabled/
sudo nginx -t
sudo systemctl reload nginx
```

At this point `http://battery.example.com/` should prompt for the username/password you set in step 6, then show the run browser over plain HTTP. That's expected -- HTTPS comes next.

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

In a browser, `https://battery.example.com/` should prompt for the username/password, then show the run browser: pick a battery ID, pick one of its tests, and its summary image and a link to the raw zip appear below.

### Using the API (e.g. from Postman or a tester)

Every route is behind Basic Auth, so requests need credentials in addition to the file:

- Postman: **Authorization** tab -> type **Basic Auth** -> your username/password (Postman adds the `Authorization` header for you).
- curl equivalents:

  ```bash
  # Store a run and get its verdict back (what the testers do)
  curl -u yourusername -F "zipfile=@run.zip" -F "battery_id=126" https://battery.example.com/runs

  # Check a zip without storing it (the only route that stores nothing)
  curl -u yourusername -F "zipfile=@run.zip" https://battery.example.com/check

  # What is stored
  curl -u yourusername https://battery.example.com/api/batteries
  curl -u yourusername https://battery.example.com/api/batteries/126/runs
  curl -u yourusername https://battery.example.com/api/runs/126/Battery_126_Tester_bt-6329_Time_2026-09-08_09-27-56

  # Pull a run's zip back off the server
  curl -u yourusername -O -J \
    https://battery.example.com/runs/126/Battery_126_Tester_bt-6329_Time_2026-09-08_09-27-56/zip
  ```

`battery_id` is optional, and so is the name of the zip. The server files a run by what its `results.json` says -- `batteryID`, `testerName` and `timestamp` (see `client/README.md`) -- so a zip can arrive called anything at all, from any machine, and is still filed under the battery and the tester that actually ran it. The form fields and the zip's own name are only fallbacks, for runs with no `results.json`; runs already stored under the older `Battery_<id>___Time_<time>` names keep them.

## The run archive

Stored runs live under `BATTERY_RUNS_DIR` (`/var/lib/battery-summary/runs`, created by `StateDirectory=` in the unit file), one directory per battery:

```text
runs/126/Battery_126_Tester_bt-6329_Time_2026-09-08_09-27-56.zip     the run exactly as the tester sent it
runs/126/Battery_126_Tester_bt-6329_Time_2026-09-08_09-27-56.json    verdict, checks, tester, upload time
runs/126/Battery_126_Tester_bt-6329_Time_2026-09-08_09-27-56.png     summary image, drawn as the run is stored
```

A run zip is a couple of MB, so a few thousand runs fit in a few GB; the images are the bigger half and can be deleted at any time (the next view re-renders them). Posting a run that's already stored replaces it, which is what lets a tester safely re-send one it wasn't sure got through.

This directory is the only state the app has -- back it up (or `rsync` it somewhere) and nothing else on the server needs saving.

## When an upload doesn't show up

The bottom of the browse page says what the server can see -- `3 batteries, 12 runs stored in /var/lib/battery-summary/runs` -- or, if it can't read that directory, why not. That line answers most of it: if the count is wrong, the runs aren't where the app is looking; if the page says it can't load them, the app or nginx is unhappy rather than the archive being empty.

```bash
# What the app itself reports (the page uses exactly this)
curl -u yourusername https://battery.example.com/api/batteries

# What is actually on disk
sudo ls /var/lib/battery-summary/runs/126/

# Did the service pick up new code? gunicorn holds the old module until restarted
sudo systemctl restart battery-summary
sudo journalctl -u battery-summary -n 50
```

A run directory holds three files once a run is stored: the `.zip`, the `.json` and the `.png`. A `.json` the service user can't read, or one that isn't valid JSON, is logged and skipped rather than emptying the whole listing -- so a run showing a `?` verdict means its metadata is unreadable, not that it's missing.

## Pointing a tester at the server

Give the testers their own Basic Auth account, so it can be changed without disturbing anyone's browser logins:

```bash
sudo htpasswd /etc/nginx/.htpasswd tester    # no -c: don't overwrite the file
```

Then on each tester Pi, fill in `/etc/solar-battery-tester/config.toml` (created with empty values by the package's postinstall, root-only since it holds the password). `/etc/solar-battery-tester/config.toml.example` sits next to it as an annotated reference, and is in the repo as `client/_release/config.toml.example`:

```toml
[server]
url = "https://battery.example.com"
username = "tester"
password = "..."
```

```bash
sudo systemctl restart solar-battery-tester
sudo journalctl -u solar-battery-tester -f     # look for "Server verdict for Battery_..."
```

Runs are written to `/var/lib/solar-battery-tester/data` first and posted afterwards, so a tester with no network keeps testing: its zips stay in that directory and go up at the start of the next run, once the server is reachable again. Zips the server has taken move to `data/uploaded/`; anything it refuses outright (a corrupt zip, say) moves to `data/rejected/` so it isn't offered forever.

## Updating the app later

```bash
cd /opt/solar-battery-tester-code
sudo -u battery-summary git pull      # or re-copy files
sudo systemctl restart battery-summary
```

The restart matters: gunicorn keeps the old code loaded until it gets one. If you copy files across by hand rather than pulling, remember the page is `server/templates/index.html` and `server/static/`, not just `webapp.py`.

nginx config changes: edit the file in `/etc/nginx/sites-available/`, then `sudo nginx -t && sudo systemctl reload nginx`.
