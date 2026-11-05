# npm-sync

`npm-sync` keeps one or more **backup** [Nginx Proxy Manager](https://nginxproxymanager.com) (NPM) instances identical to a **master** instance, so a backup can take over traffic if the master goes down. It works like [adguardhome-sync](https://github.com/bakito/adguardhome-sync):

- **One-way, strict follow.** The master is the single source of truth. Anything added, changed or deleted on the master is applied to every backup; manual changes on a backup are overwritten on the next sync (except hosts you exclude on purpose).
- **Periodic.** Runs on a cron schedule (default every 12 hours), once on startup, and on demand from the web UI or API.
- **The master does all certificate work.** Certificates are copied from the master as files and re-copied whenever they change there. Backups never contact Let's Encrypt and need no DNS provider tokens.

It is a single static Go binary, distributed as the Docker image `ghcr.io/toyonix/npm-sync`. [SPEC.md](SPEC.md) describes in detail how it works.

> **Credits.** This project is heavily inspired by two others:
>
> - [npm-sync](https://github.com/jeffersonraimon/npm-sync) by Jefferson J. Raimon, a Python tool that mirrors proxy hosts between NPM instances. This project started as a fork of it and was then rewritten in Go.
> - [adguardhome-sync](https://github.com/bakito/adguardhome-sync) by bakito. npm-sync follows its model for Nginx Proxy Manager: strict one-way sync, a cron schedule, a web UI and API, and configuration by file or environment.

## What is synced

| | |
|---|---|
| Let's Encrypt certificates (HTTP and DNS challenge) | copied, refreshed on renewal |
| Custom (uploaded) certificates | copied, on NPM up to v2.16.0 ([details](#custom-certificates)) |
| Proxy hosts, redirection hosts, 404 hosts, streams | created, updated and deleted to match the master |
| Access lists | passwords supplied in the config ([details](#access-lists)) |

Not synced, because NPM's API does not expose them: DNS provider tokens (not needed) and access list passwords (supplied in npm-sync's config instead).

## Requirements

- NPM **v2.9.9** or later on every instance (tested with **v2.16.0**). Pin the master and all backups to the **same image tag** and upgrade them together. Forks such as NPMplus are not supported.
- An NPM user for npm-sync on each instance, **without 2FA**. A dedicated user is recommended.
- Run the master and backups with the **same `TZ`**. NPM stores certificate expiry in its local time, and npm-sync compares the master's expiry with the copy's to detect renewals.
- Private keys travel through npm-sync when certificates are copied (in memory only, never written to disk; custom certificates' files are held for the duration of one run). Keep npm-sync and the NPM admin ports on a trusted network, or use `https://` URLs for the admin APIs.

## Quick start

1. Copy [`config.example.yaml`](config.example.yaml) to `config.yaml` and set the URLs and credentials.
2. Check what the first run would do, without changing anything:
   ```bash
   docker run --rm -v ./config.yaml:/config/config.yaml:ro \
     -e DRY_RUN=true -e CRON= ghcr.io/toyonix/npm-sync:latest
   ```
3. Run it with Docker Compose ([`docker-compose.example.yml`](docker-compose.example.yml)):
   ```yaml
   services:
     npm-sync:
       image: ghcr.io/toyonix/npm-sync:latest
       restart: unless-stopped
       ports:
         - "8080:8080"   # web UI and API
       volumes:
         - ./config.yaml:/config/config.yaml:ro
   ```
4. Open `http://<host>:8080` for the dashboard.

Image tags: `latest` (and `main`) are built from every push to the `main` branch. Releases are also published as `X.Y.Z` and `X.Y`, if you prefer to pin a version.

## How a run works

1. Read the master's certificates, access lists, proxy hosts, redirection hosts, 404 hosts and streams.
2. For each backup, in turn:
   1. Copy every certificate that has no copy yet, and re-copy those that changed on the master (renewed, or re-uploaded). Copies are custom certificates named `<master name> [npm-sync:<master id>]`; that tag is how npm-sync finds them again, so don't rename them. Certificates without the tag are never touched.
   2. Create and update access lists, matched by name.
   3. Sync hosts: **delete** hosts that are not on the master, then **update** hosts that differ, then **create** missing ones — each step across all host types, so a domain can move from one type to another on the master. Proxy, redirection and 404 hosts are matched by their set of domain names; streams by incoming port together with their TCP/UDP flags. Certificate and access list IDs (including those of custom locations) are rewritten to the backup's copies. Disabled hosts stay disabled. Unchanged hosts are never written, so nginx is not reloaded without need.
   4. If certificates were renewed and nothing else made NPM reload nginx, re-save one host that uses a renewed certificate so nginx serves the new files.
   5. Delete certificate copies and access lists that are no longer on the master, unless a host on the backup still uses them. (NPM would silently remove an access list from the hosts that use it.)

A failure on one object is logged and the run continues (set `continueOnError: false` to stop that backup's run instead). A failure on one backup never stops the others.

### Safety checks

Before anything is written to a backup:

- **Empty master.** If the master returns no objects of a switched-on type (proxy hosts, redirection hosts, 404 hosts, streams, access lists or certificates) while the backup has some, the backup's run is aborted. If you really removed the last host of a type on the master, run once with `allowEmptyMaster: true` (or `ALLOW_EMPTY_MASTER=true`). This protects against a master that was reset or misconfigured. Override with `allowEmptyMaster: true`.
- **Delete limit.** If a run would delete more than `maxDeletes` objects (default `10`) on one backup, the backup's run is aborted. A host whose domains changed counts as one delete. Raise the limit for one run, or run with `dryRun: true` first to check the plan.
- **Same instance.** npm-sync refuses to start if a backup URL points to the master (same host name, or a host name that resolves to the same address, on the same port).
- **Dry run.** `dryRun: true` computes and logs every action and writes nothing.

### Excluding domains

`excludeDomains` lists patterns of domains that npm-sync never touches, for hosts that should exist only on one side:

```yaml
excludeDomains:
  - "internal.example.com"   # exact domain
  - "*.lab.example.com"      # any subdomain of lab.example.com (not lab.example.com itself)
  - "staging-*"              # any domain starting with staging-
```

A host is excluded when any of its domains matches any pattern (case-insensitive; `*` also matches dots). This applies to proxy, redirection and 404 hosts; streams have no domains and are not affected. Excluded master hosts are not synced, and excluded backup hosts are never updated or deleted. A backup can add its own patterns under `backups[].excludeDomains`.

If two streams on the master use the same port and protocols, neither is synced and an error is reported.

### Custom certificates

Certificates you uploaded by hand on the master are copied too. NPM up to and including v2.16.0 returns their files, private key included, in its certificate list, and npm-sync copies them from there. Changes are detected by comparing the certificate files, so a re-upload is picked up even if the expiry date stays the same.

This relies on NPM behaviour that is going away: NPM [considers returning the key a bug](https://github.com/NginxProxyManager/nginx-proxy-manager/issues/5128) and has [removed it](https://github.com/NginxProxyManager/nginx-proxy-manager/pull/5891) for the release after v2.16.0. Once the master runs such a release:

- a copy made earlier keeps being used until it expires, but changes on the master are no longer followed (shown in the UI);
- a custom certificate without a copy cannot be copied: hosts that use it are synced **without TLS** (Force SSL, HTTP/2 and HSTS off) and shown as `degraded`.

Let's Encrypt certificates are not affected: they are copied through NPM's certificate download, which is a regular feature.

### Access lists

NPM never returns access list passwords, so npm-sync needs the password of every user of every access list on the master in its own config:

```yaml
accessListPasswords:
  "Admin panels":       # access list name, as on the master
    alice: "s3cret"
    bob: "hunter2"
```

or from the environment (each value can also be a `_FILE`):

```
ACCESSLIST1_NAME="Admin panels"
ACCESSLIST1_USER1_USERNAME=alice
ACCESSLIST1_USER1_PASSWORD_FILE=/run/secrets/alice
ACCESSLIST1_USER2_USERNAME=bob
ACCESSLIST1_USER2_PASSWORD=hunter2
```

It **fails closed**: if a user has no configured password, or the password is clearly wrong, that access list is not synced and every host that uses it is skipped and reported as an error. A host is never synced with weaker protection than on the master.

NPM shows a *hint* for each password (its first character and its length). npm-sync uses it to catch typos in the configured passwords and to detect password changes, so unchanged passwords are not rewritten. A change that keeps the same first character and length cannot be detected: push it with **Sync now (send passwords)** in the UI, `POST /api/v1/sync?forcePasswords=true`, or `--force-passwords` in one-shot mode.

Access lists are matched by name. If two lists on the master share a name, neither is synced.

### Retries

Requests are retried up to 3 times (after 1 s, then 2 s) on connection errors, timeouts and HTTP 502/503/504. A create is not sent again blindly: npm-sync first checks whether the earlier attempt succeeded. A 401 triggers one re-login.

## Configuration

Settings come from the YAML file (default `/config/config.yaml`, or `--config` / `NPM_SYNC_CONFIG`) and from environment variables, which override the file. See [`config.example.yaml`](config.example.yaml) for every setting and its default.

Any environment variable can instead be given as `<NAME>_FILE` pointing to a file, for Docker secrets (setting both is an error):

```
MASTER_URL, MASTER_USERNAME, MASTER_PASSWORD
BACKUP1_URL, BACKUP1_USERNAME, BACKUP1_PASSWORD      # BACKUP2_..., BACKUP3_... (no gaps)
BACKUP1_FEATURES_PROXY_HOSTS=false                    # per-backup feature override
BACKUP1_EXCLUDE_DOMAINS="backup-only.example.com"
EXCLUDE_DOMAINS="internal.example.com,*.lab.example.com"
CRON, RUN_ON_START, DRY_RUN, CONTINUE_ON_ERROR, ALLOW_EMPTY_MASTER, MAX_DELETES, HTTP_TIMEOUT
FEATURES_CERTIFICATES, FEATURES_PROXY_HOSTS, FEATURES_ACCESS_LISTS, ...
WEB_PORT, WEB_USERNAME, WEB_PASSWORD, WEB_METRICS
LOG_LEVEL, LOG_FORMAT
```

Example with secrets:

```yaml
services:
  npm-sync:
    image: ghcr.io/toyonix/npm-sync:latest
    environment:
      MASTER_URL: http://npm-master.lan:81
      MASTER_USERNAME: npm-sync@example.com
      MASTER_PASSWORD_FILE: /run/secrets/npm_password
      BACKUP1_URL: http://npm-backup.lan:81
      BACKUP1_USERNAME: npm-sync@example.com
      BACKUP1_PASSWORD_FILE: /run/secrets/npm_password
    secrets: [npm_password]
secrets:
  npm_password:
    file: ./npm_password.txt
```

`npm-sync --print-config` prints the effective configuration with passwords masked.

### One-shot mode

With `cron: ""`, `web.port: 0` and `runOnStart: true` (the default), npm-sync runs once and exits with code `0` on success and `1` on failure, for use from an external scheduler:

```bash
docker run --rm -v ./config.yaml:/config/config.yaml:ro -e CRON= -e WEB_PORT=0 ghcr.io/toyonix/npm-sync:latest
```

## Web UI and API

npm-sync serves a small web UI on port `8080` (`web.port`; `0` turns it off), styled like NPM's own interface, with light and dark mode:

- **Dashboard**: master and backups with reachability, NPM version and last result; last and next run; **Sync now** and **Sync now (send passwords)**.
- **Objects**: every synced object per backup with its last action (created, updated, deleted, unchanged, skipped, excluded, degraded, error) and why; certificates also show their expiry and when they were last copied.
- **Logs**: the last 1000 log lines, filterable by level.

The same data is available over HTTP, for automation:

| Method | Path | |
|---|---|---|
| `GET` | `/api/v1/status` | Last run result per backup and per object, reachability, next run (JSON). |
| `POST` | `/api/v1/sync` | Start a run: `202`, or `409` if one is running. `?forcePasswords=true` sends every access list password. |
| `GET` | `/api/v1/logs?level=info` | Recent log lines (JSON). |
| `POST` | `/api/v1/logs/clear` | Clear the log buffer. |
| `GET` | `/healthz` | `200` while the process is alive. |
| `GET` | `/readyz` | `200` if the last run succeeded for every backup, otherwise `503` (also before the first run). |
| `GET` | `/metrics` | Prometheus metrics, if `web.metrics: true`. |

For example, to sync right after changing something on the master:

```bash
curl -X POST http://npm-sync:8080/api/v1/sync
```

`/readyz` is the one to monitor (Uptime Kuma, a Docker healthcheck, ...): it turns `503` when a backup could not be brought in line — a backup that is down, a host skipped because of a missing access list password, an expired certificate — so you know before you need the backup. Hosts synced without TLS (`degraded`) don't fail it.

Set `web.username` and `web.password` to protect the UI and `/api/*` with basic auth (`/healthz`, `/readyz` and `/metrics` stay open). The UI and API can trigger writes to every backup, so enable it if the port is reachable by anyone but you.

Metrics: `npm_sync_run_total{result}`, `npm_sync_run_duration_seconds`, `npm_sync_last_run_timestamp_seconds`, `npm_sync_objects{backup,type,state}`, `npm_sync_certificate_expiry_timestamp_seconds{backup,certificate}`.

## Logs

Every action is logged with the backup, object type, name and action (`created`, `updated`, `deleted`, `unchanged`, `skipped`, `excluded`, `degraded`, `error`). The console format is coloured on a terminal; set `log.format: json` for JSON. Passwords, tokens, private keys and certificate contents are never logged.

## Development

```bash
go test ./...                    # unit tests, including a fake NPM API
test/integration/run.sh          # two real NPM instances in docker compose
docker build -t npm-sync .
```

## Migrating from jeffersonraimon/npm-sync

If you used the original Python [npm-sync](https://github.com/jeffersonraimon/npm-sync), the configuration format and location changed:

| Before (`/app/config.yml`) | Now (`/config/config.yaml`) |
|---|---|
| `source` | `master` |
| `destinations` | `backups` |
| `host` | `url` |
| `interval_hours: 12` | `cron: "0 */12 * * *"` |
| `mirror` | always on (strict follow); use `excludeDomains` for hosts to keep |
| `dry_run`, `log_level` | `dryRun`, `log.level` |

Update the volume mount (for example `./config.yaml:/config/config.yaml:ro`) and the image, which moved from `ghcr.io/jeffersonraimon/npm-sync` to `ghcr.io/toyonix/npm-sync`. `NPM_SYNC_CONFIG` keeps its name.

## License

MIT — see [LICENSE](LICENSE), which keeps the copyright of the original npm-sync. The web UI embeds [Tabler](https://tabler.io)'s CSS (MIT, see [`internal/web/static/THIRD_PARTY_LICENSES.txt`](internal/web/static/THIRD_PARTY_LICENSES.txt)).
