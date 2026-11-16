# Skiff

A tiny, self-hosted server panel for Laravel apps, in the spirit of Laravel Forge.
Install it on a fresh Ubuntu VPS, connect GitHub, pick a repository, and Skiff
clones it, installs dependencies, and serves it with Nginx + PHP-FPM.

- **One static binary** (~13 MB), no runtime dependencies. Idles at ~16 MB RAM.
- **SQLite** for state, so there's no database server to run.
- **Server-rendered UI** embedded in the binary: no Node, no build step.
- Built for the smallest servers: PHP-FPM uses `ondemand` workers, jobs run one
  at a time, and provisioning adds swap on machines with < 2 GB RAM.

## Install on a server

Requirements: Ubuntu 22.04 or 24.04, root access.

```bash
# On your machine (needs Go 1.26+, or use `make docker-release` with Docker)
make release
scp dist/skiff-linux-amd64 scripts/install.sh root@YOUR_SERVER:/root/

# On the server
sudo SKIFF_PORT=8090 /root/install.sh /root/skiff-linux-amd64
```

The installer creates a `skiff` systemd service and prints a one-time setup
link (`http://SERVER_IP:8090/setup?token=…`). Open it to create the admin
account.

Then in the panel:

1. **Provision server**: installs Nginx, PHP 8.4, Composer, Git and creates the
   unprivileged `skiff` user that sites run as.
2. **Settings → Panel address & HTTPS**: give the panel a domain (e.g.
   `panel.example.com`) so you stop logging in over plain HTTP. Do this before
   setting up GitHub, since the OAuth callback URL depends on it.
3. **Settings → GitHub**: register a GitHub OAuth app (the page shows the exact
   callback URL), paste the Client ID and secret, then **Connect GitHub**.
4. **Repositories → Deploy**: pick a branch and a domain (or the server's IP).

## What a deploy does

1. Checks out the latest commit of the branch into `/home/skiff/<domain>`
   (shallow clone; the GitHub token is passed to git through the environment,
   never written to disk, and is *not* visible to your deploy script).
2. On the first deploy, creates `.env` from `.env.example` with production
   values and an SQLite database file if the app uses one.
3. Runs the site's **deploy script** as the `skiff` user (editable per site;
   the default runs `composer install`, `key:generate` if needed, `migrate`,
   `optimize`).
4. Writes the Nginx site config (first deploy only; your edits are kept),
   then reloads Nginx and PHP-FPM.

Every job's output streams live in the UI and is kept under
`/var/lib/skiff/logs`.

## PHP versions

Each site picks its PHP version (8.2, 8.3, 8.4 or 8.5; new sites default to
8.4, which Laravel 13 needs). A version is installed the first time a site uses
it, from Ubuntu's repositories or `ppa:ondrej/php`, with its own `ondemand`
PHP-FPM pool, so unused versions cost almost nothing.

Changing a site's version on its page takes effect on the next deploy: Skiff
installs the version if needed, runs the deploy script with that `php` (and
`composer`) first on `PATH`, and repoints the site's Nginx config at the
matching PHP-FPM socket, keeping any other edits (including certbot's).

## Node.js and frontend builds

Each site also picks a Node.js LTS version (22, 24 or 26; default 24, or
None). If the repository has a `package.json`, the first deploy installs that
version from the official nodejs.org builds (checksum-verified, into
`/opt/skiff/node/<major>`; no apt repositories or install scripts), and puts
`node`/`npm` first on the deploy script's `PATH`.

The default deploy script builds assets after `composer install`:

```bash
if [ -f package.json ]; then
    if [ -f package-lock.json ]; then npm ci; else npm install; fi
    npm run build
fi
```

so Vite's `public/build/manifest.json` exists before the site goes live. Sites
whose deploy script is still an untouched older default are upgraded to the new
one automatically when Skiff starts; edited scripts are left alone (add the
block above yourself).

## MySQL databases

MySQL isn't part of provisioning. **Databases → Install MySQL** adds it only
when you need it:

- MySQL 8 from Ubuntu's packages, listening on `127.0.0.1` only (the X
  Protocol port 33060 is turned off).
- On servers with < 2 GB RAM, Skiff writes `/etc/mysql/mysql.conf.d/zz-skiff.cnf`
  to shrink it (no `performance_schema`, 64–128 MB InnoDB buffer pool, 50
  connections).
- Skiff administers MySQL as root over the unix socket (Ubuntu's default
  `auth_socket`), so no root password exists or is stored.

**New site → "Create a MySQL database"** (ticked by default once MySQL is
installed) creates a database and a same-named user with a random password, and
writes `DB_CONNECTION=mysql`, `DB_HOST`, `DB_DATABASE`, `DB_USERNAME`,
`DB_PASSWORD` into `.env` on the first deploy, before the deploy script runs
migrations.

On the **Databases** page you can also create standalone databases, attach one
to an already-deployed site (its `.env` is updated and migrations run), reveal
passwords, and delete databases (type the name to confirm).

- Each user only has access to its own database.
- Passwords reach MySQL over stdin and `.env` through an environment variable,
  so they never appear in job logs or process lists. They're stored in Skiff's
  root-only SQLite file so the panel can show them to you.
- Deleting a site keeps its database (and data) and just detaches it.
- To browse a database from your computer, use an SSH tunnel to port 3306.

## HTTPS for sites

On a site's page, **Enable HTTPS** gets a free Let's Encrypt certificate:

1. Checks that the domain resolves (and whether it points at this server).
2. Installs certbot + its Nginx plugin the first time (kept out of
   provisioning so tiny servers don't pay for it until needed).
3. Runs `certbot --nginx --redirect`, which adds TLS and an HTTP→HTTPS redirect
   to the site's existing Nginx config, keeping any edits you made.
4. Switches `APP_URL` to `https://` (only if it was the default
   `http://<domain>`) and refreshes Laravel's config cache.

Certificates renew automatically through certbot's systemd timer. The site
page shows the expiry date. **Remove HTTPS** deletes the certificate and resets
the site's Nginx config to Skiff's plain-HTTP template. Deleting a site also
deletes its certificate.

Requirements: a real domain (not an IP, `localhost` or `.test`) whose A record
points at the server, and port 80 open to the internet.

## HTTPS for the panel

**Settings → Panel address & HTTPS** serves the panel itself at a domain:

1. Writes an Nginx reverse proxy (`/etc/nginx/sites-available/skiff-panel`)
   from that domain to the panel on `127.0.0.1:<port>`.
2. Gets a Let's Encrypt certificate for it with certbot, the same way sites do.
   If that fails, the proxy is rolled back so nothing is left exposed.
3. From then on, plain-HTTP visits to `http://SERVER_IP:8090` get a 307
   redirect to `https://<domain>`, and session cookies are marked `Secure`.
   Login rate limiting uses the real client IP from the proxy's `X-Real-IP`
   (trusted only from localhost).

Requests from localhost are never redirected, so an SSH tunnel always works:
`ssh -L 8090:localhost:8090 root@server`, then open http://localhost:8090.
If the domain ever stops resolving, run `sudo skiff reset-panel-domain` on the
server and the panel answers on its IP again within seconds.

Remember to update your GitHub OAuth app's callback URL to
`https://<domain>/github/callback` after switching.

## Testing HTTPS without a real domain

To try it without hitting production rate limits, run the panel against Let's
Encrypt's staging CA (certificates won't be browser-trusted):

```bash
skiff serve --acme-server https://acme-staging-v02.api.letsencrypt.org/directory
```

## Develop locally

```bash
make dev   # http://127.0.0.1:8090, dry-run: server commands are printed, not executed
```

To exercise real provisioning and deploys without a VPS, run the Linux binary
in an Ubuntu container:

```bash
make docker-release
docker run -d --name skiff-test -p 8090:8090 -p 8081:80 \
  -v "$PWD/dist/skiff-linux-arm64:/usr/local/bin/skiff:ro" \
  ubuntu:24.04 /usr/local/bin/skiff serve
docker logs skiff-test   # shows the setup link
```

(Use `skiff-linux-amd64` on Intel machines. Deploy with domain `localhost`, then open
http://localhost:8081.)

## Layout

```
cmd/skiff            CLI entry point (serve, reset-admin, version)
internal/executor    Executor interface: Local runs bash on this box; DryRun prints.
                     A future SSH/agent executor lets one panel manage many servers.
internal/laravel     Provisioning script, Nginx template, deploy pipeline, HTTPS (certbot),
                     panel reverse proxy, MySQL
internal/github      OAuth flow, repo listing, token-in-env git auth
internal/jobs        Sequential background job runner with file logs
internal/store       SQLite schema and queries
internal/web         HTTP handlers, templates, static assets (embedded)
scripts/install.sh   systemd installer
```

## Security notes

- The panel runs as root (it manages packages and Nginx); sites run as `skiff`.
- First-run setup requires a token only readable on the server.
- Sessions are HttpOnly + SameSite cookies, every form carries a CSRF token,
  logins are rate-limited per IP.
- **Give the panel an HTTPS domain** (see above) before using it over the
  internet. Until then, prefer an SSH tunnel. If you run your own reverse
  proxy instead, pass `--url https://panel.example.com` and Skiff leaves the
  panel's address to you.
- The GitHub token and OAuth secret are stored in `/var/lib/skiff/skiff.db`
  (mode 0700 directory, root only).

## Locked out?

```bash
sudo skiff reset-admin   # prints a new /setup?token=… link; no restart needed
```
