# Skiff

A tiny, self-hosted server panel for Laravel, JavaScript and Python apps, in the spirit of Laravel Forge.
Install it on a fresh Ubuntu VPS, connect GitHub, pick a repository, and Skiff
clones it, installs dependencies, and serves it with Nginx + PHP-FPM.

- **One static binary** (~13 MB), no runtime dependencies. Idles at ~16 MB RAM.
- **SQLite** for state, so there's no database server to run.
- **Server-rendered UI** embedded in the binary: no Node, no build step.
- Built for the smallest servers: PHP-FPM uses `ondemand` workers, jobs run one
  at a time, and provisioning adds swap on machines with < 2 GB RAM.

## Install on a server

Requirements: Ubuntu 22.04 or 24.04 (x86-64 or ARM), root access.

```bash
curl -fsSL https://raw.githubusercontent.com/abdielwilsn/skiff/main/scripts/install.sh | sudo bash
```

The installer downloads the latest release for your CPU from GitHub, verifies
its SHA-256 checksum, installs it to `/usr/local/bin/skiff`, creates a `skiff`
systemd service and prints a one-time setup link
(`https://SERVER_IP:8090/setup?token=…`). Open it to create the admin account.

Until the panel has its own domain, port 8090 uses a self-signed certificate,
so the setup token and your password are encrypted from the first visit. Your
browser warns once; check that the certificate's SHA-256 fingerprint matches
the one the installer printed (`sudo skiff tls-fingerprint` shows it again).
To avoid the warning, use an SSH tunnel and http://localhost:8090 instead.

Options: `| sudo bash -s -- --version v0.2.0` installs a specific release, and
`--port 9000` picks the panel port on a fresh install.

### Updating

Run the same command again. It installs the newest release, restarts the
service and keeps your data (`/var/lib/skiff`), settings and service file. If
you're already on the latest version it does nothing.

Then in the panel:

1. **Provision server**: installs Nginx, PHP 8.4, Composer, Git and creates the
   unprivileged `skiff` user that sites run as.
2. **Settings → Panel address & HTTPS**: give the panel a domain (e.g.
   `panel.example.com`) with a real certificate. Do this before setting up
   GitHub, since the OAuth callback URL depends on it.
3. **Settings → Login security**: turn on two-factor login.
4. **Settings → GitHub**: register a GitHub OAuth app (the page shows the exact
   callback URL), paste the Client ID and secret, then **Connect GitHub**.
   Repositories you own, collaborate on, or reach through an organization are
   listed. An organization that restricts OAuth apps hides its repositories
   until an owner approves yours: open
   `https://github.com/settings/connections/applications/<client id>` and press
   **Grant** (or **Request**) next to the organization.
5. **Repositories → Deploy**: pick a branch and a domain (or the server's IP).

## What a deploy does

Deploys are release-based, so a failed deploy never touches the live site:

```
/home/skiff/<domain>/
  current -> releases/20261122141530   what Nginx serves
  releases/<id>/                        one checkout per deploy (last 5 kept)
  shared/.env  shared/storage/  shared/database/database.sqlite
/var/lib/skiff/repos/<domain>/          git cache (root only)
```

1. Fetches the newest commit of the branch into the git cache and unpacks it
   into a new `releases/<id>/`. Git runs as root, with the GitHub token in its
   environment only, so no site, dependency or deploy script (all running as
   `skiff`) can read the token. The `skiff` user unpacks the files, so it owns
   every file in the release.
2. Links the shared `.env`, `storage/` and SQLite database into the release
   (`.env` is created from `.env.example` on the first deploy).
3. Runs the site's **deploy script** in the release as the `skiff` user (editable
   per site; the default runs `composer install`, clears cached config/routes,
   `key:generate` if needed, `npm ci && npm run build`, `migrate`, `optimize`).
4. Only if that succeeded: switches `current` to the new release with an atomic
   rename, updates Nginx, and reloads Nginx and PHP-FPM. If anything failed, the
   unfinished release is deleted and the live site is unchanged.
5. Keeps the newest 5 releases for rollback and drops `node_modules` from
   releases that aren't live.

**Rollback**: a site's **Deployments** page lists the kept releases; **Roll back** switches
`current` back instantly and rebuilds the config cache from the current `.env`.
Database migrations run since that release are not undone.

Sites deployed before release-based deploys move over on their next deploy:
their `.env`, `storage/` and SQLite file are moved into `shared/` (and linked
back, so the old code keeps working until the switch), and the old checkout is
removed on the deploy after that.

## Deploying new commits

**Deploy now** on a site always fetches the newest commit of its branch
(**Promote to production** is the exception: it builds the commit staging runs)
(`git fetch` + `git reset --hard`), then runs the deploy script.

**Push to deploy**: in a site's **Settings → Git**, **Turn on** adds a webhook
to the GitHub repository (through your connected GitHub account, which needs
admin access to the repo). From then on every push to the site's branch queues
a deploy; the job title shows the commit and who pushed it.

- Deliveries go to `https://<panel domain>/hooks/github/<site id>`, so the panel
  needs its HTTPS domain first (Settings → Panel address & HTTPS).
- Each site has its own random secret; deliveries without a valid
  `X-Hub-Signature-256` are rejected, and pushes to other branches are ignored.
- Pushes that arrive while a deploy is already queued are folded into it, since
  every deploy fetches the latest commit.
- Turning auto-deploy off, or deleting the site, removes the webhook.

## Staging and other environments

A site's **Environments** page adds a staging (or preview, dev…) environment:
the same repository and setup (project type, PHP/Node/Python version, deploy
script, workers and scheduler), deployed from **its own branch** to **its own
domain**. Each environment is a full site of its own, with its own releases,
`.env`, logs, Commands, HTTPS and push-to-deploy webhook. Switch between them
from the menu at the top of the site's sidebar.

- **Variables**: by default the new environment starts with a copy of
  production's `.env`. `APP_ENV` becomes the environment's name, `APP_URL` its
  address, and URLs, allowed hosts and cookie domains that name production's
  domain get the new one. Database, cache and mail settings are copied as they
  are. Untick the box to start from `.env.example` instead.
- **Database**: a site whose MySQL or PostgreSQL database Skiff manages gets a
  separate database for each environment, so staging's migrations and test
  data never touch production's. A site using Redis shares the server's Redis
  with a key prefix of the environment's own. SQLite files are per environment anyway. If
  production's `.env` points at a database server Skiff doesn't manage, change
  the copy on the environment's **Env variables** page.
- **Promote to production** deploys the exact commit an environment runs to
  production, through production's normal deploy: backup first, deploy script,
  migrations, health check, rollback if it fails. Production keeps following
  its own branch, so merge the change there too; its next deploy or push
  builds that branch's tip again.
- A production site can't be deleted while it has other environments; delete
  them first.

## Queue workers & scheduler (Laravel)

The **Queue workers & scheduler** card on a Laravel site's **Overview**:

- **Worker processes** (0–10) run `php artisan queue:work <options>` as systemd
  services (`skiff-<domain>-worker@N`) as the `skiff` user in the live release,
  with the site's PHP. They restart if they crash and are restarted after every
  deploy, rollback and `.env` change so they always run the current code.
- **Options** are passed to `queue:work`, e.g. `redis --queue=high,default
  --tries=3 --timeout=90`.
- **Scheduler** runs `php artisan schedule:run` every minute via a systemd
  timer (`skiff-<domain>-schedule.timer`).

The site's **Logs** page shows their output (from the journal) and the tail of
`storage/logs/laravel.log`.

## JavaScript projects

Pick a framework on **New site** and Skiff sets the project type:

| Type | Frameworks | How it's served |
|---|---|---|
| **Node.js app** | Next.js, Nuxt, SvelteKit (adapter-node), Remix / React Router, Astro (server), Express, Fastify, NestJS… | Your **start command** runs as a systemd service (`skiff-<domain>-app`) with `PORT`, `HOST=127.0.0.1`, `NODE_ENV=production` and the site's `.env`; Nginx proxies the domain to it (WebSockets included). |
| **Static site** | Vite (React, Vue, Svelte, Solid), Astro, Create React App, Next.js static export, plain HTML | Nginx serves the build **output folder** (e.g. `dist`) from the live release, falling back to `index.html` for client-side routes. |

The default deploy script installs dependencies with the package manager whose
lockfile is in the repo (npm, pnpm or yarn) and runs the `build` script if
there is one. Node apps and static sites get the same release-based deploys,
rollback, HTTPS, push-to-deploy, `.env` editor and Commands as Laravel sites.

After switching a Node app to a new release, Skiff waits for it to answer on
its port. **If it doesn't start, the previous release is put back
automatically** and the deploy is marked failed with the app's last log lines.
Saving the `.env` restarts the app.

## Python projects

Pick **Django**, **FastAPI**, **Flask** or **Other Python server** on
**New site**, and a Python version (3.11–3.14).

- Python comes from [uv](https://docs.astral.sh/uv/): the first Python deploy
  downloads the uv binary from its GitHub release (checksum-verified) into
  `/opt/skiff/uv`, and uv installs the chosen Python into `/opt/skiff/python`.
  Both are root-owned; no apt packages or PPAs are involved.
- Every release gets its own `.venv`, so a rollback runs with exactly the
  packages it was built with. The default deploy script installs from
  `uv.lock` (`uv sync --frozen`), `requirements.txt` or `pyproject.toml`, adds
  the app server the preset uses (gunicorn or uvicorn), and for Django runs
  `collectstatic` (when `STATIC_ROOT` is set) and `migrate`.
- The **start command** runs as a systemd service like a Node app, with the
  release's `.venv` first on `PATH`, `PORT`, `HOST=127.0.0.1` and the site's
  `.env`. For Django, `$WSGI_MODULE` is set to the package that holds
  `wsgi.py`, so the default `gunicorn "$WSGI_MODULE" …` works without editing.
  If a new release doesn't start, the previous one is put back automatically.
- Nginx serves `/static/` from the release's `staticfiles/` folder (Django's
  usual `STATIC_ROOT`) and passes anything else, including files it doesn't
  have, to the app (so WhiteNoise keeps working).
- Django's default SQLite database (`db.sqlite3` next to `manage.py`) lives in
  `shared/` so it survives deploys, and it's included in backups.
- Add your domain to Django's `ALLOWED_HOSTS` (or read it from `.env`).
  Settings in `.env` reach the app, the deploy script and the Commands page as
  environment variables.

## Running commands

Each site has a **Commands** page (in the site's sidebar) for one-off
commands, like Laravel Cloud's: a text box plus one-click presets for
`migrate`, `migrate:status`, `db:seed`, `queue:restart`, `optimize:clear`,
`storage:link` and more. Commands:

- run in the site's directory as the unprivileged `skiff` user, with the site's
  PHP and Node.js first on `PATH` (the same environment as the deploy script);
- can span several lines (they run as a bash script and stop at the first
  failure);
- run non-interactively: prompts get their default answer, so pass `--force`
  where Artisan asks for it in production;
- stream their output live, like deploys, and are kept in a per-site history
  with **Use again**.

Destructive commands (`migrate:fresh`, `db:wipe`, `rm -rf` …) ask for
confirmation first.

## PHP versions

Each site picks its PHP version (8.2, 8.3, 8.4 or 8.5; new sites default to
8.4, which Laravel 13 needs). A version is installed the first time a site uses
it, from Ubuntu's repositories or [packages.sury.org](https://packages.sury.org/php/) (servers set up with the frozen `ppa:ondrej/php` are moved to it), with its own `ondemand`
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

## Databases: MySQL, PostgreSQL and Redis

None of them is part of provisioning. The **Databases** page installs each one
only when you need it, listening on `127.0.0.1` only:

- **MySQL 8** from Ubuntu's packages (the X Protocol port 33060 is off). On
  servers with < 2 GB RAM, `/etc/mysql/mysql.conf.d/zz-skiff.cnf` shrinks it
  (no `performance_schema`, 64–128 MB InnoDB buffer pool, 50 connections).
  Skiff administers it as root over the unix socket (`auth_socket`).
- **PostgreSQL** from Ubuntu's packages (16 on Ubuntu 24.04). On small servers
  `conf.d/zz-skiff.conf` lowers `shared_buffers` (64–128 MB) and
  `max_connections` (50). Skiff administers it as the `postgres` system user
  (peer authentication). Each database is owned by its own login role and
  other roles can't connect to it. PHP's `pgsql` extension is installed with
  every PHP version.
- **Redis** from Ubuntu's packages, with a password Skiff generates
  (`/etc/redis/skiff.conf`, shown on the Databases page).

No superuser password exists or is stored for MySQL or PostgreSQL.

**New site → Database** creates a MySQL or PostgreSQL database and a
same-named user with a random password, and writes its settings into `.env` on
the first deploy, before the deploy script runs migrations:

- Laravel: `DB_CONNECTION` (`mysql` or `pgsql`), `DB_HOST`, `DB_PORT`,
  `DB_DATABASE`, `DB_USERNAME`, `DB_PASSWORD`.
- Django, FastAPI, Flask, Node apps: `DATABASE_URL`
  (`postgres://user:password@127.0.0.1:5432/name` or `mysql://…:3306/…`), which
  dj-database-url, Prisma, Drizzle, SQLAlchemy and most libraries read. Your
  app still needs its driver (e.g. `psycopg` for Django, `pg` for Node).

**Redis** for a site, either ticked when creating it or with **Use Redis** on
its Overview:

- Laravel: `REDIS_HOST`, `REDIS_PORT`, `REDIS_PASSWORD`, and a `REDIS_PREFIX`
  of the site's own (e.g. `app_example_com_`), so sites and their staging
  environments sharing the server's Redis never see each other's keys.
  Optionally `CACHE_STORE`, `SESSION_DRIVER` and `QUEUE_CONNECTION` switch to
  `redis` (on by default). Skiff installs the `phpredis` extension.
- Other apps: `REDIS_URL=redis://default:password@127.0.0.1:6379/0`.
- The site's app or workers restart so they pick it up.

On the **Databases** page you can also create standalone databases, attach one
to an already-deployed site (its `.env` is updated; Laravel migrations run,
other apps restart), reveal passwords, and delete databases (type the name to
confirm).

- Each user only has access to its own database.
- Passwords reach the database over stdin and `.env` through an environment
  variable, so they never appear in job logs or process lists. They're stored
  in Skiff's root-only SQLite file so the panel can show them to you.
- Deleting a site keeps its database (and data) and just detaches it.
- To browse a database from your computer, use an SSH tunnel to port 3306 or
  5432.

## Backups

**Backups** dumps every database on the server **every night** (03:00 server
time by default) and **before every deploy** of a Laravel or Python site, so a
bad migration can be undone:

- MySQL databases with `mysqldump --single-transaction` (a consistent snapshot
  without locking the app), PostgreSQL databases with `pg_dump` (plain SQL
  without ownership, restored as the database's own user), and the SQLite databases of deployed sites
  (`database/database.sqlite`, Django's `db.sqlite3`) with SQLite's `.backup`.
- Files are gzipped into `/var/backups/skiff/<database>/`, readable by root only.
  The newest 7 nightly/manual backups are kept per database (configurable), plus
  the last 3 taken before deploys and restores.
- **Off-site storage**: add any S3-compatible bucket (AWS S3, Cloudflare R2,
  Backblaze B2, DigitalOcean Spaces, Hetzner, MinIO…) and every backup is also
  uploaded to `<folder>/<server>/<database>/`. Uploads use curl's built-in AWS
  signing, with the keys passed on stdin so they never appear in logs or
  process lists. Saving the settings tests the connection.
- **Restore** puts a database back to a backup. Skiff first takes a safety
  backup of the current data, downloads the file from the bucket if the local
  copy is gone, then replaces the database (MySQL and PostgreSQL: dropped and recreated;
  SQLite: an atomic file swap) and restarts the site's app or workers.
- **Download** any backup from the panel.
- Redis isn't backed up: it holds caches, sessions and queued jobs, which are
  meant to be short-lived.

If a backup before a deploy fails, the deploy stops (nothing changes); turn off
"Back up before each deploy" to deploy anyway.

## Server health

**Server** shows CPU, memory, disk and swap use, load, uptime, a 24-hour chart
of each (sampled once a minute, kept in memory), and whether Nginx, PHP-FPM,
MySQL, PostgreSQL, Redis and every site process (app servers, queue workers, schedulers) are
running. The dashboard shows the same numbers in a strip and warns when the
disk is over 85% full, memory is over 90% used, or a service has failed.

## HTTPS for sites

In a site's **Settings → Domain & HTTPS**, **Enable HTTPS** gets a free Let's Encrypt certificate:

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
3. From then on, visits to `SERVER_IP:8090` get a 307 redirect to
   `https://<domain>`, and session cookies are marked `Secure`. Login rate
   limiting uses the real client IP from the proxy's `X-Real-IP`, trusted only
   on requests that also carry the proxy's secret `X-Skiff-Proxy` header. The
   proxy config is root-only, so sites on the server can't forge it.

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

## Releasing

Releases are built by GitHub Actions (`.github/workflows/release.yml`) when a
version tag is pushed:

```bash
git tag v0.2.0 && git push origin v0.2.0
```

The workflow runs the tests, builds `skiff-linux-amd64` and `skiff-linux-arm64`
with the tag as their version, writes `SHA256SUMS`, and publishes a GitHub
release with generated notes. Servers pick it up the next time someone runs the
install command.

To install a build from your own machine instead (no release needed):

```bash
make docker-release
scp dist/skiff-linux-amd64 scripts/install.sh root@SERVER:/root/
ssh root@SERVER 'bash /root/install.sh /root/skiff-linux-amd64'
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
internal/laravel     Provisioning, Nginx templates, deploy pipeline, PHP/Node/Python runtimes,
                     HTTPS (certbot), panel reverse proxy, MySQL, PostgreSQL, Redis,
                     backups, server health
internal/github      OAuth flow, repo listing, token-in-env git auth
internal/jobs        Sequential background job runner with file logs
internal/store       SQLite schema and queries
internal/web         HTTP handlers, templates, static assets (embedded)
scripts/install.sh   systemd installer
```

## Security notes

- The panel runs as root (it manages packages and Nginx); sites run as `skiff`.
- Code running as `skiff` can't reach root's secrets through Skiff. Git, which
  holds the GitHub token, runs as root. Each app's `.env` is loaded by
  `skiff env-exec` as the site user, not by systemd as root. Site files
  (SQLite databases, `laravel.log`) are read and written as `skiff` during
  backups, restores and on the Logs page, so a symlink planted there can't
  make root read or write anything else.
- All sites share the `skiff` user, so one site can still read another's files
  and `.env`. Keep sites you don't trust on separate servers.
- First-run setup requires a token only readable on the server.
- The panel's port speaks HTTPS from the first start (self-signed until it has
  a domain). Plain HTTP is only answered for local clients (SSH tunnels,
  Skiff's own proxy); anything else is redirected to HTTPS. If you run your own
  reverse proxy, pass `--url https://panel.example.com` and Skiff leaves the
  panel's address to you.
- **Two-factor login** (Settings → Login security): a 6-digit code from an
  authenticator app after the password, plus 10 single-use recovery codes.
  Turning it on logs out every other session. Lost the phone and the codes?
  `sudo skiff disable-2fa` on the server turns it off.
- Sessions are HttpOnly + SameSite cookies that end after 12 hours without use
  (and 30 days after login); Settings can log out every other session. Every
  form carries a CSRF token, and logins (both steps) are rate-limited per IP.
- The GitHub token, OAuth secret and off-site storage keys are stored in
  `/var/lib/skiff/skiff.db` (mode 0700 directory, root only).
- Backups in `/var/backups/skiff` are root-only (directories 0700, files 0600).

## Locked out?

```bash
sudo skiff reset-admin   # prints a new /setup?token=… link; no restart needed
sudo skiff disable-2fa   # lost your authenticator and recovery codes
```
