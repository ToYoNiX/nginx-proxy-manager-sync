# npm-sync — How it works

This document describes how npm-sync behaves and why, for contributors and coding agents. Code comments refer to its sections, for example `(spec 4.2.1)`. When behaviour changes, update the matching section in the same change.

## 1. Goal

`npm-sync` keeps one or more **backup** Nginx Proxy Manager (NPM) instances identical to a **master** NPM instance, so a backup can take over traffic if the master goes down.

The model follows [adguardhome-sync](https://github.com/bakito/adguardhome-sync):

- **One-way, strict follow.** The master is the single source of truth. Anything added, changed or deleted on the master is applied to every backup. Manual changes on a backup are overwritten on the next sync. The only exception is hosts whose domains are excluded on purpose (3.2).
- **Periodic.** Syncs run on a cron schedule (default every 12 hours), once on startup, and on demand from the web UI or API.
- **Master does all certificate work.** Backups never contact Let's Encrypt. Certificates (Let's Encrypt and custom) are copied from the master as files, and re-copied whenever they change on the master. No DNS provider tokens are needed.

npm-sync is written in **Go**, starting from the Python [npm-sync](https://github.com/jeffersonraimon/npm-sync) by Jefferson J. Raimon. It is distributed as a single static binary and a Docker image at `ghcr.io/toyonix/npm-sync`. License: MIT.

### Not synced (limitations)

These cannot be read through NPM's API, so npm-sync does not sync them:

- **DNS provider tokens** (the credentials used for DNS-challenge certificates). NPM never returns them. Backups don't need them, because they never issue certificates.
- **Access list passwords.** NPM masks them. They are supplied in npm-sync's config instead (4.4).

Manually uploaded (custom) certificates are synced only while NPM returns their private key through the API, which it does up to and including v2.16.0 (4.2.3).

### Non-goals

- Two-way sync, or merging changes made on a backup.
- Automatic failover (DNS switching, VIPs). npm-sync only keeps backups ready; switching traffic is out of scope.
- Syncing NPM users, permissions, audit log or global settings.
- Issuing certificates on backups.
- Forks of NPM such as NPMplus. Only upstream NPM (`jc21/nginx-proxy-manager`) is supported; forks differ in authentication and API schema.

## 2. Terminology

| Term | Meaning |
|---|---|
| Master | The NPM instance that is edited by hand and receives traffic. |
| Backup | An NPM instance kept identical to the master by npm-sync. There can be several. |
| Managed object | An object on a backup that npm-sync created and therefore owns. |
| Host | A proxy host, redirection host, dead (404) host or stream. |
| SANs | Subject Alternative Names: the domains a certificate is valid for. In NPM, a certificate's `domain_names`. |
| Run | One sync pass: read the master, then bring every backup in line with it. |

## 3. What gets synced

Each feature can be switched off globally or per backup (section 6). All are on by default.

| Feature | NPM API | Matched by |
|---|---|---|
| `certificates` | `/api/nginx/certificates` | tag in the copy's name (4.2.2) |
| `accessLists` | `/api/nginx/access-lists` | name (4.4) |
| `proxyHosts` | `/api/nginx/proxy-hosts` | domain set (4.3) |
| `redirectionHosts` | `/api/nginx/redirection-hosts` | domain set (4.5) |
| `deadHosts` (404 hosts) | `/api/nginx/dead-hosts` | domain set (4.5) |
| `streams` | `/api/nginx/streams` | incoming port and TCP/UDP flags (4.5) |

Disabled hosts on the master are synced as disabled hosts (4.3, 4.5).

### 3.1 Switched-off features

A switched-off feature is left alone on the backup: its objects are not created, updated or deleted. Strict follow (4.3) applies only to switched-on features.

Hosts depend on certificates and access lists, so switching those off also affects hosts:

- **`certificates` off.** No copies are created, refreshed or deleted. Hosts still point to an existing usable copy (4.2.2). Hosts whose certificate has no usable copy are synced without TLS and marked degraded, as in 4.2.3.
- **`accessLists` off.** Hosts that use an access list on the master are **skipped**, never synced without it (fail closed, 4.4).

### 3.2 Excluded domains

`excludeDomains` lists domain patterns that npm-sync never touches. It is useful when a backup needs a few hosts of its own, or when some master hosts should not be failed over.

```yaml
excludeDomains:
  - "internal.example.com"   # exact domain
  - "*.lab.example.com"      # any subdomain of lab.example.com
  - "staging-*"              # any domain starting with staging-
```

- **Pattern syntax.** Patterns are shell-style wildcards, matched case-insensitively against each domain: `*` matches any run of characters, including dots, and `?` matches one character. `*.example.com` does not match `example.com` itself; list both if needed.
- **What counts as excluded.** A host is excluded when **any** of its domains matches **any** pattern. This applies to proxy hosts, redirection hosts and dead hosts. Streams have no domains of their own and are not affected.
- **On the master.** Excluded hosts are not synced to the backup.
- **On the backup.** Excluded hosts are left alone: never updated or deleted, whether or not the master has them.
- **Collisions.** If an excluded backup host holds a domain that a synced master host needs, creating the master host fails with NPM's `<domain> is already in use`. npm-sync reports this as an error that names the excluded backup host and its type.
- **Certificates and access lists are not filtered.** Every master certificate is still copied, so a wildcard certificate keeps working for the hosts that are synced. Cleanup (4.1) never deletes an object that a host on the backup references, including excluded hosts.
- **Global and per-backup.** A top-level `excludeDomains` applies to every backup. A backup can add its own list under `backups[].excludeDomains`, and the two lists are combined.
- **Reporting.** Excluded hosts appear in the UI and the status API with the action `excluded`. They do not affect readiness.

## 4. Sync behaviour

### 4.1 Run order

For each run:

1. **Read the master**: version (4.9), then, after logging in, certificates, every host type, and access lists (with `?expand=items,clients`) if any backup has `accessLists` on. If the master cannot be read, the run is aborted for every backup.
2. For **each backup**, in sequence:
   1. Read the backup's version, log in, and read its certificates and every switched-on type.
   2. Plan certificates, access lists and hosts, and run the **safety checks** (4.6). A failed check aborts this backup's run before anything is written.
   3. Sync certificates: create and refresh copies (4.2).
   4. Sync access lists: create and update (4.4).
   5. Sync hosts of every type together, in three passes (4.3, 4.5):
      1. **Delete** unmatched proxy hosts, redirection hosts, dead hosts and streams.
      2. **Update** matched hosts of every type.
      3. **Create** missing hosts of every type.

      NPM's `<domain> is already in use` check covers proxy, redirection and dead hosts together. A domain can move from one host type to another on the master, so all deletes happen before any create.
   6. Read every host type on the backup again, then reload nginx if refreshed certificates need it (4.2.1, step 4).
   7. Cleanup: delete managed certificates whose master certificate no longer exists, duplicate managed copies (4.2.2), and access lists that are not on the master (4.4). This comes last, so that hosts have already moved off them. An object that a host on the backup still references (including through a proxy host location's access list) is **not** deleted, for example because that host's update failed. NPM does not stop this itself: deleting a certificate breaks the host's nginx config, and deleting an access list silently removes it from every host that uses it, leaving them unprotected. It is retried on the next run.
3. Record the result per backup and per object, for the UI, logs and metrics.

A failure on one backup does not stop the others. A failure on one object is recorded and the run continues with the next object, unless `continueOnError: false` is set, which aborts that backup's run.

### 4.2 Certificates

Every certificate on the master is classified by its `provider`:

| Master certificate | Handling on backup |
|---|---|
| `letsencrypt` (HTTP or DNS challenge) | **Copied.** Files downloaded from the master and uploaded to the backup as a custom certificate (4.2.1). |
| `other` (custom, uploaded by hand) | **Copied** while NPM returns the files in its certificate list, up to v2.16.0 (4.2.3). |

#### 4.2.1 Copying

For a Let's Encrypt certificate on the master:

1. **Download.** `GET /api/nginx/certificates/{id}/download` on the master returns a zip of every `.pem` file in the certificate's certbot `live` directory. Symlinks are resolved, so entries carry certbot's archive names with a version number: `cert<N>.pem`, `chain<N>.pem`, `fullchain<N>.pem`, `privkey<N>.pem`. npm-sync identifies files by prefix (`cert`, `chain`, `privkey`; `fullchain` is not needed), reads them in memory and never writes them to disk.
2. **Create** (first copy only). On the backup: `POST /api/nginx/certificates` with `{"provider": "other", "nice_name": "<tagged name>", "domain_names": [], "meta": {}}`.
3. **Upload.** `POST /api/nginx/certificates/{backupId}/upload`, multipart, with file fields `certificate` (cert), `certificate_key` (private key) and `intermediate_certificate` (chain). NPM validates the files with openssl and rejects expired certificates. Uploading again to the same certificate replaces its files, so refreshes reuse the same backup certificate ID and hosts keep pointing at it.
4. **Reload nginx.** NPM's upload writes the new files but does **not** reload nginx, so the backup would keep serving the old certificate. If certificates were refreshed and no host write in the run reloaded nginx, npm-sync re-saves one **enabled**, non-excluded host that uses a refreshed certificate (a `PUT` with its unchanged payload). The host can be of any synced type. NPM regenerates that host's config and reloads nginx, and the reload applies the new files for every host. A disabled host can't be used, because it has no nginx config to regenerate. If no enabled host uses a refreshed certificate, no reload is needed: nginx reads the files when a host starts using the certificate.

The download happens again for each backup, so a private key is only held in memory for one copy. The backup's NPM treats the copy as a custom certificate and never tries to renew it.

#### 4.2.2 Identity and change detection

Backup copies are identified by a tag in their `nice_name`:

```
<master nice_name> [npm-sync:<master certificate id>]
```

Example: `example.com, *.example.com [npm-sync:12]`.

This keeps npm-sync stateless: the mapping from master certificate to backup certificate is read back from the backup on every run, with no local database. It is needed because NPM's upload sets a custom certificate's `domain_names` to the certificate's CN only, so backup copies cannot be matched by SANs.

Only the `[npm-sync:<id>]` suffix is used for matching. The name before it is for people and is set when the copy is created. NPM has no API to rename a certificate, so if the master's name changes later, the backup copy keeps the old name. If two backup certificates carry the same tag, the one with the lowest ID is used, and the others are deleted during cleanup.

**Usable copies.** A copy is usable when:

- the backup reports the certificate as uploaded: `meta.certificate` is present, and `meta.certificate_key` is either present or not reported at all (NPM up to v2.16.0 returns the file contents there; later releases omit `certificate_key`). A copy whose upload failed has neither; and
- its `expires_on` is in the future.

A copy that was created but whose upload failed is not usable: it has no files, and NPM sets its `expires_on` to the time it was created. Hosts never point to a copy that is not usable.

A copy is refreshed (copied and uploaded again) when it is not usable, or when the master certificate's `expires_on` differs from the copy's `expires_on`. The second case happens exactly when the master renews. For custom certificates, a copy is also refreshed when its certificate or chain differs from the master's (compared by hash; a new key always comes with a new certificate), so a re-upload with the same expiry is followed. Otherwise the certificate is left untouched, so routine runs download no key material.

`expires_on` is stored in each NPM instance's local time, so the master and backups must run with the same `TZ`, or every run sees a renewal.

A master certificate that has already expired is not copied (NPM rejects expired uploads). This is reported as an error.

#### 4.2.3 Custom certificates on the master

NPM stores an uploaded certificate's files as text in the certificate's `meta`, and up to and including v2.16.0 returns them, private key included, in `GET /api/nginx/certificates` (verified on v2.12.6 and v2.16.0; the code is the same back to v2.9.9). npm-sync copies custom certificates from there: the same create and upload as in 4.2.1, with the files taken from the master's list instead of the download endpoint, which refuses custom certificates. As soon as a list is read, the file contents in `meta` are replaced with `true`; the master's custom files are kept, outside `meta`, until the run ends, and the backups' are dropped at once.

NPM considers returning the key a bug ([issue #5128](https://github.com/NginxProxyManager/nginx-proxy-manager/issues/5128)) and removed it in [PR #5891](https://github.com/NginxProxyManager/nginx-proxy-manager/pull/5891), merged after v2.16.0. When the master runs a release with that change:

- a custom certificate with a usable copy from an earlier run keeps using it. The copy is not refreshed or deleted; changes on the master are no longer followed, and the object's message in the UI says so;
- a custom certificate without a usable copy cannot be copied. Hosts that use it are synced **without TLS** (`certificate_id: 0`, with `ssl_forced`, `http2_support`, `hsts_enabled` and `hsts_subdomains` switched off) and shown as **degraded** in the UI and logs, with the reason.

#### 4.2.4 Certificates npm-sync did not create

Certificates on a backup that do not carry an `[npm-sync:…]` tag are never modified or deleted. Hosts synced from the master never reference them. This is the one exception to strict follow: an untagged certificate does nothing unless a host uses it, and hand-made hosts on a backup are removed anyway (4.3). Deleting one would gain nothing and could destroy key material that exists nowhere else.

### 4.3 Proxy hosts

**Desired state.** For each master proxy host, the backup payload is built from its writable fields:

`domain_names, forward_scheme, forward_host, forward_port, certificate_id, ssl_forced, hsts_enabled, hsts_subdomains, trust_forwarded_proto, http2_support, block_exploits, caching_enabled, allow_websocket_upgrade, access_list_id, advanced_config, enabled, meta, locations`

These fields are rewritten:

- `certificate_id`: the backup ID of the usable managed copy of the master certificate (4.2.2). If this run's refresh failed, the copy from an earlier run is used as long as it is still usable. If there is no usable copy (the first copy failed, a custom certificate as in 4.2.3, or `certificates` is off), the host is synced without TLS and marked degraded.
- `access_list_id`, and `access_list_id` of each entry in `locations`: the backup ID of the matching access list (4.4), or `0` if the master has none. If the master uses an access list that was not synced to this backup, the host is **skipped** and reported as an error. Reasons include: `accessLists` is off, a password is missing or wrong, the name is used twice on the master, or the list failed to sync. A host is never synced with weaker protection than on the master.
- `meta`: only user-set keys are copied. Status keys that NPM writes itself (`nginx_online`, `nginx_err`) are dropped, both when sending and when comparing.

**Matching.** A backup host matches a master host when their `domain_names` are equal as sets (case-insensitive, ignoring order). If several backup hosts match, the one with the lowest ID is used and the others are unmatched.

**Applying.** Changes are applied in this order, to avoid NPM's `<domain> is already in use` error. Each step runs across all host types before the next step starts (4.1):

1. **Delete** backup hosts that match no master host.
2. **Update** matched hosts whose payload differs from the desired state.
3. **Create** master hosts that have no match.

When a host's domains change on the master (for example `a` becomes `a, b`), the old backup host is deleted in step 1 and the new one created in step 3, within the same run. Because matching uses the exact domain set, an update never changes domains, so it cannot collide with another host.

A skipped master host (for example because of its access list) still counts as matched. If the backup has a matching host, it is left as it is, not deleted. This means a temporary problem doesn't take a host down on the backup.

**Comparison.** Two hosts are equal when their normalised payloads are equal: `domain_names` compared as a set, filtered `meta`, `locations` without the `id` that NPM may fill in, and numbers compared by value. An unchanged host is never written, so NPM does not reload nginx without need.

**Strict follow.** Every proxy host on a backup is either synced from the master or deleted. Proxy hosts created by hand on a backup are removed, unless they are excluded (3.2).

### 4.4 Access lists

Access lists are matched by `name`. `satisfy_any`, `pass_auth`, `clients` (IP allow/deny rules, compared in order, because nginx applies them in order) and `items` (users) are copied.

**Duplicate names.** NPM does not require access list names to be unique, but npm-sync matches by name.

- If two access lists on the **master** share a name, neither is synced. This is reported as an error, and hosts that use either list are skipped (4.3). A backup list with that name is left alone.
- If two access lists on a **backup** share a name, the one with the lowest ID is used, and the others count as unmatched.

**Strict follow.** Every access list on a backup is either synced from the master or deleted during cleanup (4.1), including lists created by hand on the backup, unless a host still uses it. Certificates are the exception to strict follow (4.2.4).

**Basic-auth passwords cannot be read from the master.** NPM masks them in every GET response. npm-sync therefore needs each password supplied in its own config, in the same ways as the instance credentials (YAML, environment variable, or `_FILE` for Docker secrets; section 6):

```yaml
accessListPasswords:
  "Admin panels":
    alice: "s3cret"
```

```
ACCESSLIST1_NAME="Admin panels"
ACCESSLIST1_USER1_USERNAME=alice
ACCESSLIST1_USER1_PASSWORD=s3cret        # or ACCESSLIST1_USER1_PASSWORD_FILE=/run/secrets/alice
```

If any user of an access list has no supplied password, that access list is **not** synced and every host that uses it is **skipped**, so it is never exposed on a backup with weaker protection than on the master. This fails closed, and is reported as an error naming the list and user.

**Password hints.** NPM's GET responses replace each user's password with an empty string, but also return a `hint`: the password's first character followed by one `*` for each remaining character (counted in UTF-16 code units, as NPM computes it in JavaScript). This shows the first character and the length. npm-sync computes the same hint from each configured password and uses it in two ways:

- **Check against the master.** If a configured password's hint differs from the master's hint for that user, the configured password is wrong, for example a typo or a stale value. Its access list is handled like one with a missing password: not synced, hosts skipped, error reported. A matching hint does not prove that the password is right, but it catches most mistakes.
- **Detect changes on the backup.** On update, NPM keeps a user's existing password when the password field is empty, and removes users that are not sent. npm-sync sends an empty password when the backup already has the user with the configured password's hint, and the configured password otherwise. Unchanged passwords are therefore not rewritten, and an access list is only written, which reloads nginx, when something actually changed.

**Limitation.** A password change that keeps the same first character and the same length cannot be detected. To push such a change, start a run with `forcePasswords`, which sends every configured password. It can be started from the "Sync now (send passwords)" button in the UI, from `POST /api/v1/sync?forcePasswords=true`, or with the `--force-passwords` flag in one-shot mode.

Users that are in the config but not on the master are ignored, with a warning.

### 4.5 Redirection hosts, dead hosts, streams

Same pattern as proxy hosts: delete, then update, then create (4.1 runs each pass across all host types). Certificate IDs are rewritten through the managed-copy mapping (4.2.2), with the same degraded fallback.

| Type | Writable fields |
|---|---|
| Redirection host | `domain_names, forward_http_code, forward_scheme, forward_domain_name, preserve_path, certificate_id, ssl_forced, hsts_enabled, hsts_subdomains, http2_support, block_exploits, advanced_config, meta` |
| Dead host | `domain_names, certificate_id, ssl_forced, hsts_enabled, hsts_subdomains, http2_support, advanced_config, meta` |
| Stream | `incoming_port, forwarding_host, forwarding_port, tcp_forwarding, udp_forwarding, certificate_id, meta` |

- **Redirection hosts and dead hosts** are matched by domain set, like proxy hosts.
- **Enabled state.** Unlike proxy hosts, redirection hosts, dead hosts and streams do not accept `enabled` in their create or update payload. It is changed with `POST /{id}/enable` and `POST /{id}/disable`. npm-sync creates a host and then disables it if the master's is disabled, and only calls enable or disable when that is the only difference.
- **Streams** are matched by `incoming_port` together with their `tcp_forwarding` and `udp_forwarding` flags. NPM has no single protocol field. Changing the flags on the master means a delete and a create. Their create payload accepts `domain_names`, but only to request a certificate; it is not stored, so streams have no domains of their own. NPM does not stop two streams from using the same port, so if the master has more than one stream with the same key, none of them is synced and an error is reported.

### 4.6 Safety checks

Before anything is written to a backup:

- **Empty master.** For each switched-on object type (proxy hosts, redirection hosts, dead hosts, streams, access lists, certificates), if the master returns zero objects of that type while the backup has some, the backup's run is aborted. For certificates, only managed copies on the backup count. Excluded hosts (3.2) do not count on either side. This protects against a master that was reset, is misconfigured, or answered with an unexpected empty response. Override with `allowEmptyMaster: true`, for example for one run after deleting the last host of a type on purpose.
- **Delete limit.** If a run would delete more than `maxDeletes` objects on one backup, the backup's run is aborted. Hosts of every type, certificate copies and access lists count, including the delete half of a domain change (4.3). Default `10`. `0` means no limit. Larger intentional changes can be pushed by raising the limit for one run, or with a dry run first to check the plan.
- **Same instance.** npm-sync refuses to start if a backup URL points to the master: same scheme, port and path, and either the same host name or host names that resolve to a shared address. Resolution failures are not fatal (the instance may not be up yet); the names are still compared.
- **Dry run.** With `dryRun: true` every action is computed, logged and shown in the UI as "would be …", and nothing is written.

### 4.7 Authentication with NPM

npm-sync logs in with `POST /api/tokens` (`identity`, `secret`) at the start of every run, for the master and each backup. Tokens are kept in memory only. A `401` during a run triggers one re-login and retry.

**Two-factor authentication is not supported.** For a user with 2FA enabled, NPM answers the login with `requires_2fa: true` and a short-lived challenge token instead of a session token. npm-sync detects this and fails that instance with a clear error: "user has 2FA enabled; use a dedicated NPM user without 2FA for npm-sync" (section 9). It does not retry.

### 4.8 Retries

NPM's admin API can fail briefly, for example while it reloads nginx after a write. Every request to NPM is retried on transient errors:

- **Transient errors:** connection refused or reset, connection closed early, request timeout, and HTTP `502`, `503` and `504`.
- **Not retried:** any other `4xx`. These are validation errors such as `already in use` or an expired certificate, and repeating the request would not change the answer. A `401` is the exception: it gets one re-login and retry, as described in 4.7.
- **Attempts and backoff:** up to 3 attempts in total, waiting 1 s and then 2 s, with up to 20% random jitter. When the attempts run out, the object is recorded as `error` and the run continues as described in 4.1.

Not every request can simply be sent again. A request that timed out may still have succeeded on NPM. Each write is retried as follows:

| Request | Retry rule |
|---|---|
| `GET` | Repeated as-is. |
| `PUT` (update), certificate upload | Repeated as-is. Sending the same payload or files again gives the same result. |
| `DELETE` | Repeated. A `404` on a retry counts as success, because the first attempt already deleted the object. |
| `POST /{id}/enable`, `/disable` | Repeated. An "already enabled/disabled" answer on a retry counts as success. |
| `POST` (create) | **Not repeated blindly.** npm-sync first reads that object type from the backup again. If the object now exists, the create counts as successful. Hosts are found by their matching key (4.3, 4.5), certificates by tag, and access lists by name. Only if it does not exist is the create sent again. |

Hosts cannot end up duplicated, because NPM rejects a second host with the same domains (`already in use`). If a duplicate stream, certificate copy or access list is created anyway, the normal rules clean it up: it counts as unmatched (4.3, 4.4) or, for a certificate copy, carries the same tag and is deleted during cleanup (4.2.2).

### 4.9 NPM versions

| | Version |
|---|---|
| Minimum | **v2.9.9**. The certificate download endpoint first appears in this release. |
| Tested | **v2.16.0** (with v2.12.6 for the custom-certificate behaviour of 4.2.3). |

NPM has no LTS releases. Pin the master and all backups to the **same exact image tag** (for example `jc21/nginx-proxy-manager:2.16.0`) and upgrade them together.

At the start of each run, npm-sync reads `GET /api/` (no login needed) to get each instance's version. It:

- refuses certificate sync, with an error, on any instance older than the minimum (hosts then use existing copies or fall back to no TLS, as with `certificates` off);
- warns when a backup's version differs from the master's, because field sets can differ between releases.

### 4.10 NPM behaviour npm-sync relies on

Verified against NPM v2.16.0 (source and running instances). The fake NPM used in tests (`internal/npm/npmtest`) reproduces each of these; keep both in step when NPM changes.

- Error answers are JSON `{"error": {"code": …, "message": …}}`. Request bodies are validated against a schema with `additionalProperties: false`, so unknown fields are rejected.
- `GET /api/` answers `{"status": "OK", "setup": true, "version": {"major", "minor", "revision"}}`; `GET /api` redirects to it.
- Certificate `expires_on` and other timestamps are `YYYY-MM-DD HH:MM:SS` in the instance's local time (SQLite).
- The certificate download zip contains certbot's versioned archive names (4.2.1). The download refuses custom certificates.
- Uploading files does not reload nginx (4.2.1). Updating an enabled host regenerates its config and reloads nginx.
- NPM's upload sets a custom certificate's `domain_names` to the certificate's CN.
- The certificate list returns custom certificates' files in `meta`, private key included, up to v2.16.0 (4.2.3).
- `<domain> is already in use` covers proxy, redirection and dead hosts together.
- Deleting an access list sets `access_list_id` to `0` on every host and location that used it.
- Access list GETs return each user with an empty `password` and a `hint`. On update, an empty password keeps the user's password, and users not sent are removed.
- Redirection hosts, dead hosts and streams change `enabled` only through `/enable` and `/disable`, which answer `400` "already enabled/disabled" if nothing changes.
- A stream's `domain_names` is accepted on create but not stored.

## 5. Scheduling and triggers

| Trigger | Default | Notes |
|---|---|---|
| Cron | `0 */12 * * *` | Standard 5-field cron. Empty disables scheduled runs. |
| On start | `true` | Runs once immediately after startup. |
| Manual | — | "Sync now" button in the UI, or `POST /api/v1/sync`. |

Only one run happens at a time. A trigger while a run is in progress is ignored and reported as such (`409` from the API).

If cron is empty and the web server is disabled (`web.port: 0`), npm-sync runs once and exits with code `0` on success and `1` on failure (one-shot mode, for use from an external scheduler). This needs `runOnStart: true`; with nothing to do, npm-sync exits with code `2`. `--force-passwords` applies only in one-shot mode.

On `SIGINT` or `SIGTERM`, npm-sync stops the web server and the schedule, cancels a run in progress and exits.

## 6. Configuration

### 6.1 YAML

Default path `/config/config.yaml`, overridable with `--config` or `NPM_SYNC_CONFIG`. A missing file is fine at the default path (all settings can come from the environment) but an error at a path given explicitly.

```yaml
master:
  url: "http://npm-master.lan:81"
  username: "admin@example.com"
  password: "changeme"

backups:
  - url: "http://npm-backup.lan:81"
    username: "admin@example.com"
    password: "changeme"
    # features:            # optional per-backup overrides
    #   streams: false
    # excludeDomains:      # added to the global list (3.2)
    #   - "backup-only.example.com"

excludeDomains: []        # domain patterns never synced or touched (3.2)

cron: "0 */12 * * *"
runOnStart: true
dryRun: false
continueOnError: true
allowEmptyMaster: false
maxDeletes: 10            # 0 = no limit
httpTimeout: 60s          # certificate upload can be slow

features:
  certificates: true
  proxyHosts: true
  accessLists: true
  redirectionHosts: true
  deadHosts: true
  streams: true

accessListPasswords: {}   # access list name -> username -> password (4.4)

web:
  port: 8080              # 0 disables the web UI and API
  username: ""            # optional basic auth for UI and API
  password: ""
  metrics: false          # expose /metrics

log:
  level: info             # debug | info | warn | error
  format: console         # console | json
```

### 6.2 Environment variables

Every setting can also be set from the environment. Environment values override the YAML file. Any variable may instead be given as `<NAME>_FILE` pointing to a file, for Docker secrets (a trailing newline is removed). Setting both forms is an error.

```
MASTER_URL, MASTER_USERNAME, MASTER_PASSWORD
BACKUP1_URL, BACKUP1_USERNAME, BACKUP1_PASSWORD
BACKUP2_URL, ...
BACKUP1_FEATURES_STREAMS=false
EXCLUDE_DOMAINS="internal.example.com,*.lab.example.com"     # comma-separated
BACKUP1_EXCLUDE_DOMAINS="backup-only.example.com"
CRON, RUN_ON_START, DRY_RUN, CONTINUE_ON_ERROR, ALLOW_EMPTY_MASTER, MAX_DELETES, HTTP_TIMEOUT
FEATURES_CERTIFICATES, FEATURES_PROXY_HOSTS, FEATURES_ACCESS_LISTS, FEATURES_REDIRECTION_HOSTS, FEATURES_DEAD_HOSTS, FEATURES_STREAMS
WEB_PORT, WEB_USERNAME, WEB_PASSWORD, WEB_METRICS
LOG_LEVEL, LOG_FORMAT
```

`BACKUP<n>` overrides the n-th backup from the file, or adds it. Numbering starts at 1 and must have no gaps.

Access list passwords use `ACCESSLIST<n>_NAME`, `ACCESSLIST<n>_USER<m>_USERNAME` and `ACCESSLIST<n>_USER<m>_PASSWORD` (section 4.4). They are merged with `accessListPasswords` from the file.

### 6.3 Validation

Configuration is validated at startup, and npm-sync exits with code `2` and a message listing every problem: missing master, no backups, an invalid URL (http or https with a host), an invalid cron expression, a feature name that does not exist, an empty `excludeDomains` pattern, a negative `maxDeletes`, a non-positive `httpTimeout`, a `web.port` out of range, an unknown log level or format, and unknown keys anywhere in the YAML file. Environment variables with invalid values, or set both directly and as `_FILE`, are reported the same way. `--print-config` prints the effective configuration with passwords masked, then exits.

## 7. Web UI and API

### 7.1 UI

Served by the binary itself at `/`, from files embedded at build time (`internal/web/static`). No external CDN, no build step: plain HTML and JavaScript, and all text is inserted as text, never as HTML.

Styled like NPM's own interface: it embeds the same Tabler UI kit CSS that NPM uses (v1.5.1, stored gzip-compressed), with the system font stack, and has light and dark mode (stored under the same browser key as NPM's, `tabler-theme`).

Pages (client-side, by URL hash):

- **Dashboard**: master and backups with reachability (a `GET /api/` probe, cached for 15 seconds), NPM version, and last run result with counts of changes. Time of the last and next run. "Sync now" button, and "Sync now (send passwords)" for a `forcePasswords` run (4.4).
- **Objects**: per backup, a table of every object from the last run: type, name/domains, last action (created, updated, deleted, unchanged, skipped, excluded, degraded, error) and message, filterable by action and text. Certificates also show expiry date and when they were last copied (the copy's `modified_on`, which NPM updates on every upload). Skips that count as errors are shown in red.
- **Logs**: the last 1000 log lines, filterable by level, with a clear button.

The page refreshes every 10 seconds, and every 2 seconds while a run is in progress.

### 7.2 HTTP API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/status` | JSON: version, whether a run is in progress, dry run, cron and next run, readiness, reachability of every instance, enabled features per backup, and the last run result per backup and per object. |
| `POST` | `/api/v1/sync` | Start a run. `202` if started, `409` if one is running. `?forcePasswords=true` sends every access list password (4.4). |
| `GET` | `/api/v1/logs` | Recent log lines as JSON. `?level=` sets the minimum level. |
| `POST` | `/api/v1/logs/clear` | Clear the in-memory log buffer. |
| `GET` | `/healthz` | Process is alive. Always `200`. |
| `GET` | `/readyz` | `200` if the last run succeeded for every backup, otherwise `503` (also `503` before the first run finishes). See below. |
| `GET` | `/metrics` | Prometheus metrics, if `web.metrics` is on. |

A run **succeeded** for a backup when the backup's run was not aborted (by a safety check, a login failure, or `continueOnError: false`) and no object ended in state `error`. Objects skipped for a reason that is reported as an error, such as a missing access list password, count as `error`. `degraded` objects (synced without TLS, 4.2.3) do not fail readiness. They are visible on the dashboard and in `npm_sync_objects{state="degraded"}`.

Basic auth, if configured, protects the UI and `/api/*`, but not `/healthz`, `/readyz` or `/metrics`. Cross-origin browser requests that would write (`POST`) are rejected, so another website cannot make a browser start a run. Responses carry a strict Content-Security-Policy and `X-Frame-Options: DENY`.

### 7.3 Metrics

Written in the Prometheus text format without the Prometheus client library:

- `npm_sync_run_total{result}` (counter; `success` or `failure`)
- `npm_sync_run_duration_seconds` (duration of the last run)
- `npm_sync_last_run_timestamp_seconds`
- `npm_sync_objects{backup,type,state}` (objects in the last run)
- `npm_sync_certificate_expiry_timestamp_seconds{backup,certificate}`

## 8. Logging

Structured logging with Go's `log/slog`. The console format is coloured when attached to a terminal (unless `NO_COLOR` is set) and plain otherwise; `log.format: json` gives JSON lines. Each action is logged with backup, object type, name and action. Output is unbuffered, so `docker logs` shows it immediately. The last 1000 lines are also kept in memory for the UI and `/api/v1/logs`.

Passwords, tokens, private keys and certificate contents are never logged at any level.

## 9. Security

- **Private keys travel through npm-sync** when certificates are copied. They are held in memory only, never written to disk or logged, and buffers holding them are zeroed after use: for one copy for Let's Encrypt certificates, and for the duration of one run for custom certificates, whose files come with the master's certificate list. Run npm-sync and the NPM admin ports on a trusted network, or use HTTPS URLs for the NPM admin APIs.
- NPM admin credentials for the master and backups are required. A dedicated NPM user per instance is recommended. That user must not have 2FA enabled (4.7).
- The UI and API can trigger writes to every backup. Enable basic auth if port 8080 is reachable by anyone other than you.
- The Docker image runs as a non-root user on a minimal base image (`gcr.io/distroless/static`).

## 10. Implementation

### 10.1 Stack

- Go, pinned in `go.mod` and the Dockerfile. Standard library first: `net/http`, `log/slog`, `embed`, `archive/zip`, `mime/multipart`, `crypto/x509` (tests).
- Dependencies, kept minimal: a cron parser (`github.com/robfig/cron/v3`) and YAML (`gopkg.in/yaml.v3`). Nothing else.

### 10.2 Layout

```
cmd/npm-sync/main.go        entrypoint, flags, signal handling, one-shot mode
internal/config/            YAML + env loading, validation, same-instance check
internal/logging/           slog console handler and the in-memory log buffer
internal/npm/               NPM API client, retries, types
internal/npm/npmtest/       in-memory fake of the NPM API for tests (4.10)
internal/sync/              planner (pure: master state + backup state -> actions):
                              certs.go, accesslists.go, hosts.go, match.go
                            executor (applies actions, records results): sync.go
internal/scheduler/         cron, run-on-start, manual trigger, single-run lock
internal/web/               HTTP server, API handlers, metrics, embedded UI (static/)
test/integration/           docker compose with two real NPM instances, and the test
Dockerfile                  multi-stage build, static binary, non-root
.github/workflows/          CI and image release
```

The planner is pure, with no network access, so the sync logic can be unit-tested completely with fixtures. The executor runs the plan against the NPM client and records an `ObjectResult` for every object.

### 10.3 CI and releases

- **CI** (`ci.yml`, on pull requests and before every release): `gofmt`, `go vet` and `go test -race ./...`.
- **Release** (`release.yml`) builds and publishes the image to the GitHub Container Registry after CI passes:
  - **Image:** `ghcr.io/toyonix/npm-sync`, for `linux/amd64` and `linux/arm64`.
  - **On push to `main`:** push `:latest` and `:main`. `latest` always follows `main`.
  - **On a `v*` tag:** push `:<version>` (for example `:1.0.0`) and `:<major>.<minor>`, for pinning a release. Tags do not move `latest`.
  - Authenticates with the workflow's built-in `GITHUB_TOKEN`, so no extra secrets are needed.

### 10.4 Testing

- **Unit, planner:** create/update/delete ordering, domain changes, ID rewriting (certificates, access lists, location access lists), meta filtering, degraded fallback, exclusions, duplicate keys, stream matching, enabled toggling, certificate tags and change detection, access list hints and change detection.
- **Unit, executor** (`internal/sync`, against two fake NPMs): full runs and idempotence (a second run writes nothing), deletes before creates across host types, safety checks, a copy whose upload failed is never referenced and is refreshed, renewal with nginx reload, cleanup skipping referenced objects, fail-closed access lists, `forcePasswords`, custom certificates including NPM releases that hide the key, dry run, 2FA, old NPM versions, switched-off features.
- **Client:** retries and which errors are retried, `404` on a retried `DELETE` counted as success, a retried create that finds the object already exists, 401 re-login, the `requires_2fa` login response, certificate download and upload.
- **Config:** defaults, YAML, environment overrides and `_FILE`, validation errors, masking for `--print-config`, same-instance detection.
- **Web:** basic auth, sync trigger and `409`, cross-site rejection, readiness rules, logs, static files, metrics.
- **Integration:** `test/integration/run.sh` starts two real NPM instances with docker compose, seeds the master (a Let's Encrypt-style certificate registered with `seed-letsencrypt-cert.sh`, a custom certificate, an access list and every host type), runs npm-sync, and checks that planning again finds nothing to do, that the backup serves the custom certificate, and that a renewal is copied.

### 10.5 Conventions

- Keep planning pure and the executor thin. New behaviour starts as a planner change with a unit test.
- Every NPM behaviour the code depends on is listed in 4.10 and reproduced by the fake in `internal/npm/npmtest`. Check new assumptions against a real NPM (`test/integration/run.sh`).
- Never log or serialise passwords, tokens, keys or certificate contents. Certificate files live in `npm.CertFiles` and are wiped with `Wipe` after use.
- Writes must stay safe to retry (4.8).
- An object that cannot be synced safely is skipped and reported, never synced with weaker protection (fail closed).

## 11. Design choices

| Topic | Choice |
|---|---|
| Sync model | Strict one-way follow, like adguardhome-sync. |
| Certificates | Copied from the master. Backups never issue certificates and need no DNS tokens. |
| Custom certificates | Copied from the files NPM returns in its certificate list, up to NPM v2.16.0. After that, existing copies are kept and new ones fall back to no TLS (4.2.3). |
| Access list passwords | Supplied in npm-sync's config, YAML / env / `_FILE`, like adguardhome-sync's credentials. Checked against the master's hint and changes detected with the backup's hint; `forcePasswords` for changes the hint misses (4.4). |
| Hosts whose access list can't be synced | Skipped, never synced without protection (fail closed) (3.1, 4.3). |
| Host write ordering | Delete, update, create, each across all host types (4.1). |
| Delete limit | `maxDeletes` default `10` (4.6). |
| State | None stored: managed certificates are found by tag, everything else by name or matching key, on every run. |
| NPM login | Users with 2FA are not supported; use a dedicated user without 2FA (4.7). |
| NPM version | Minimum v2.9.9, tested v2.16.0, master and backups pinned to the same tag (4.9). |
| NPM forks | Not supported, including NPMplus. Upstream NPM only (section 1). |
| Excluded domains | `excludeDomains`, global and per backup. Excluded hosts are neither synced nor touched on backups (3.2). |
| Retries | 3 attempts on transient errors with backoff; creates check for the object before being sent again (4.8). |
| Image | `ghcr.io/toyonix/npm-sync` (10.3). |
| License | MIT, Jefferson J. Raimon and ToYoNiX. |

## 12. Known limitations

- **Custom certificates after NPM v2.16.0.** Releases that no longer return private keys in the certificate list cannot have new custom certificates copied (4.2.3), and the download endpoint refuses custom certificates. NPM still writes the files to `/data/custom_ssl/npm-<id>/` (`fullchain.pem`, `privkey.pem`) on the master.
- **Time zones.** The master and backups must run with the same `TZ` (4.2.2).
- **Access list passwords.** A change that keeps the first character and the length is only pushed with `forcePasswords` (4.4).
- **Same-instance check.** Two NPM instances reached by different names through one reverse proxy on the same address and port look like the same instance (4.6).
- **Empty master check per type.** Deleting the last host of a type on the master on purpose needs one run with `allowEmptyMaster: true` (4.6).
- **Two-factor authentication.** Not supported for the NPM users npm-sync logs in with (4.7).
