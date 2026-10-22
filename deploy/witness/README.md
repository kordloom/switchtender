# Hosted witness deployment

This directory runs the hosted witness at `witness.switchtender.com`. It holds the systemd unit
and its environment file, the cloudflared routing, the hourly snapshot to R2 with its timer, and
the restore. This README is the runbook.

The owner runs every step. A step marked **OWNER** creates an account, a DNS record, a tunnel, a
bucket, a monitor, or something paid, or it handles the passphrase of the key's offline copy. An
agent never runs one of those steps, and an agent never deploys the service.

`DROPLET` below stands for the droplet's address, which the DigitalOcean control panel shows.
`TUNNEL_ID` stands for the tunnel's id, which `cloudflared tunnel list` prints on the owner's Mac.

## How it fits together

One droplet runs `switchtender witness serve` as its own account, `switchtender-witness`,
listening on `127.0.0.1:9440` and nowhere else. cloudflared on the same droplet holds an outbound
connection to Cloudflare, which terminates TLS for `witness.switchtender.com` and forwards the push
witness's public routes to loopback: `/v1/` for enrollment, pushes, statements, and findings,
`/.well-known/loomseal.json` for the key list, and `/healthz`. Everything else answers 404 at the
edge. The droplet's firewall allows no inbound port but SSH.

Because the tunnel makes loopback reachable from the internet, the service runs with a read token
even though the binary would serve an open API on a loopback address. The token guards the
cross-server `/witness` routes, which the tunnel never forwards in any case.

| Path                                                 | What it holds                        |
|------------------------------------------------------|--------------------------------------|
| `/var/lib/switchtender-witness`                      | Every install's memory, snapshotted. |
| `/var/lib/switchtender-witness-key`                  | `witness-key.json`, never copied.    |
| `/etc/switchtender-witness/switchtender-witness.env` | The read token and the Slack URL.    |
| `/etc/switchtender-witness/rclone.conf`              | The R2 credentials for snapshots.    |
| `/usr/local/lib/switchtender-witness`                | The snapshot and restore scripts.    |

The key is a file on one machine plus one encrypted copy the owner keeps offline. No snapshot ever
holds it: `snapshot-to-r2.sh` refuses to run when a key file sits in the state directory, and
`restore.sh` refuses a snapshot that holds one.

## Done 2026-10-20

- The droplet `switchtender-witness`, DigitalOcean NYC1, Basic Regular at $6 a month, Ubuntu
  24.04.5, root login by SSH key only, ufw denying all inbound traffic except 22/tcp, and
  unattended-upgrades on. Done 2026-10-20.
- The Cloudflare Tunnel `switchtender-witness`, locally managed, with its credentials file in
  `/etc/cloudflared/` (mode 0400) on the droplet and in `~/.cloudflared/` on the owner's Mac.
  cloudflared 2026.10.2 runs as a systemd service and routes `witness.switchtender.com` to
  `http://127.0.0.1:9440`, everything else to 404. Done 2026-10-20.
- The CNAME record `witness.switchtender.com` pointing at the tunnel. It answers 502 until the
  service runs. Done 2026-10-20.
- R2 on the Cloudflare account, free tier, and the bucket `switchtender-witness-state`. Done
  2026-10-20.

## 1. Install the binary from a signed release

On the droplet, as root. The installer checks the cosign signature over the release checksums
only when cosign is on the path, so install cosign first.

```sh
curl -fsSLO https://github.com/sigstore/cosign/releases/latest/download/cosign-linux-amd64
install -m 0755 cosign-linux-amd64 /usr/local/bin/cosign && rm cosign-linux-amd64
curl -fsSL https://switchtender.com/install.sh -o install.sh
sh install.sh
switchtender version --verify
switchtender witness serve --help | grep -- --accept-enrollments
```

The installer must print `Signature verified: the checksums were signed by this project's release
workflow.` If it says instead that the signature was not checked, stop and find out why. The last
command must print the `--accept-enrollments` line, which shows the release carries the push
witness.

Fetch this directory at the same tag as the binary:

```sh
tag="v$(switchtender version | sed 's/^v//')"
install -d -m 0700 /root/witness-deploy && cd /root/witness-deploy
for f in switchtender-witness.service switchtender-witness.env.example cloudflared.yml.example \
  switchtender-witness-snapshot.service switchtender-witness-snapshot.timer \
  snapshot-to-r2.sh restore.sh; do
  curl -fsSLO "https://raw.githubusercontent.com/kordloom/switchtender/$tag/deploy/witness/$f"
done
```

## 2. Create the service account and install the unit

On the droplet, as root, in `/root/witness-deploy`:

```sh
useradd --system --home-dir /var/lib/switchtender-witness --shell /usr/sbin/nologin \
  switchtender-witness
install -d -m 0700 -o root -g root /etc/switchtender-witness
install -m 0600 -o root -g root switchtender-witness.env.example \
  /etc/switchtender-witness/switchtender-witness.env
sed -i "s/^SWITCHTENDER_WITNESS_TOKEN=\$/SWITCHTENDER_WITNESS_TOKEN=$(openssl rand -hex 32)/" \
  /etc/switchtender-witness/switchtender-witness.env
install -m 0644 switchtender-witness.service /etc/systemd/system/
systemctl daemon-reload
```

The unit runs this command line, every flag of which `switchtender witness serve --help` lists:

```sh
/usr/local/bin/switchtender witness serve --listen 127.0.0.1:9440 --accept-enrollments \
  --enrollment-policy licensed --state-dir /var/lib/switchtender-witness \
  --key-dir /var/lib/switchtender-witness-key
```

`--enrollment-policy licensed` accepts an enrollment only with a license that includes the hosted
witness: Enterprise, or Team with the add-on. `--max-installs-per-license` is left at its default
of 10, the number the order form states.

## 3. OWNER: Create the Slack webhook for #switchtender

In the settings of the Slack app that posts to `#switchtender`, turn on Incoming Webhooks and add a
webhook for `#switchtender`. Copy its URL. On the droplet, open
`/etc/switchtender-witness/switchtender-witness.env` in an editor and paste the URL after
`SWITCHTENDER_WITNESS_NOTIFY_SLACK=`. The URL is the credential, so it goes nowhere else.

Every refusal and every note about an enrolled install is posted there, `silent_install` included.

## 4. Start the service, which creates the key

```sh
systemctl enable --now switchtender-witness
systemctl status --no-pager switchtender-witness
journalctl -u switchtender-witness -o cat --no-pager | grep -m1 'witness: watching'
journalctl -u switchtender-witness -o cat --no-pager | grep -m1 'Slack'
ls -l /var/lib/switchtender-witness-key
curl -fsS http://127.0.0.1:9440/healthz
curl -fsS http://127.0.0.1:9440/.well-known/loomseal.json
```

The first start creates `witness-key.json` in the key directory, mode 0600, owned by
`switchtender-witness`. The journal line ends with the key id, as in `witness: watching 0 servers,
state /var/lib/switchtender-witness, key sha256:...`, and the key list carries the same id. Write
the key id down for steps 5 and 6. The next line, `witness: findings go to 1 Slack webhook`, shows
the URL from step 3 was read. The URL itself is never printed.

## 5. OWNER: Back up the key offline

On the droplet, as root. age asks for a passphrase on the terminal, which the owner types:

```sh
apt-get install -y age
tar -C /var/lib/switchtender-witness-key -cf - witness-key.json \
  | age -p -o /root/witness-key.tar.age
```

On the owner's Mac:

```sh
scp root@DROPLET:/root/witness-key.tar.age .
```

Move the file to the owner's offline storage with the key id and the date beside it, then remove
it from the droplet with `rm /root/witness-key.tar.age`. The tar keeps the key file's modification
time, which the witness publishes as the key's `valid_from`, so a restored key publishes the same
key list.

## 6. Publish the key id

The service publishes the key list at `https://witness.switchtender.com/.well-known/loomseal.json`
once step 7 routes the tunnel, and every enrollment answer carries it. Commit the key id to
`docs/witness.md` in place of the placeholder there, so the id is in the repository's history and
in the release that carries it. Rotating the witness key ships with the enrollment client, which is
what lets an enrolled install accept a new key on the word of the one it holds.

## 7. Route the tunnel to the service

On the droplet, compare `/etc/cloudflared/config.yml` with `cloudflared.yml.example`. Keep the
`tunnel:` and `credentials-file:` lines as they are and replace the `ingress:` block with the
example's. Then check and restart:

```sh
cloudflared tunnel --config /etc/cloudflared/config.yml ingress validate
cloudflared tunnel --config /etc/cloudflared/config.yml ingress rule \
  https://witness.switchtender.com/witness/servers
systemctl restart cloudflared
```

The rule check must name the `http_status:404` rule. From anywhere:

```sh
curl -fsS https://witness.switchtender.com/healthz
curl -fsS https://witness.switchtender.com/.well-known/loomseal.json
curl -s -o /dev/null -w '%{http_code}\n' https://witness.switchtender.com/witness/servers
```

The answers are `{"ok":true}`, the key list with the key id from step 4, and `404`.

## 8. OWNER: Add the rate limit and cache rules

In the Cloudflare dashboard, on the `switchtender.com` zone:

- Security, WAF, Rate limiting rules, Create rule. Name it `witness enroll`. Match URI Path equals
  `/v1/enroll`, and add Hostname equals `witness.switchtender.com` when the rule builder offers it.
  Count by IP. Block above 5 requests in 10 seconds, for 10 seconds. The witness also limits
  enrollments per license, so this rule only stops one address from flooding the endpoint.
- Caching, Cache Rules, Create rule. Name it `witness reads`. Match Hostname equals
  `witness.switchtender.com` and URI Path starts with `/v1/installs/`. Mark it eligible for cache
  and set the edge TTL to follow the origin's cache-control header. The service sends
  `public, max-age=60` on statements and findings, so a read is served from the edge for at most a
  minute. Pushes are POST requests, which Cloudflare never caches.

## 9. Snapshot the state to R2 every hour

**OWNER:** in the Cloudflare dashboard, R2, Manage API tokens, create a token with Object Read &
Write permission on the bucket `switchtender-witness-state` alone. Keep the Access Key ID, the
Secret Access Key, and the S3 endpoint the same page shows, which has the form
`https://ACCOUNT_ID.r2.cloudflarestorage.com`.

On the droplet, as root, in `/root/witness-deploy`:

```sh
apt-get install -y rclone
install -m 0600 -o root -g root /dev/null /etc/switchtender-witness/rclone.conf
```

Open `/etc/switchtender-witness/rclone.conf` in an editor and give it the token's values:

```ini
[r2]
type = s3
provider = Cloudflare
access_key_id = ACCESS_KEY_ID
secret_access_key = SECRET_ACCESS_KEY
endpoint = https://ACCOUNT_ID.r2.cloudflarestorage.com
acl = private
no_check_bucket = true
```

`no_check_bucket` keeps rclone from trying to create the bucket, which a token scoped to one bucket
may not do. Then install the snapshot and take the first one by hand:

```sh
RCLONE_CONFIG=/etc/switchtender-witness/rclone.conf rclone lsf r2:switchtender-witness-state
install -d -m 0755 /usr/local/lib/switchtender-witness
install -m 0755 snapshot-to-r2.sh restore.sh /usr/local/lib/switchtender-witness/
install -m 0644 switchtender-witness-snapshot.service switchtender-witness-snapshot.timer \
  /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now switchtender-witness-snapshot.timer
systemctl start switchtender-witness-snapshot.service
journalctl -u switchtender-witness-snapshot -o cat --no-pager | tail -3
```

The first command lists nothing and reports no error. The journal shows `snapshot: uploaded
witness-state-...tar.gz` and `snapshot: kept 1 snapshots`.

Retention keeps the 48 newest snapshots or as many as fit in 5 GiB, whichever is fewer, and always
the newest. The head logs grow about 26 MB a year per install at a 60 second cadence before
compression, so the byte budget is what holds the bucket at half of R2's free 10 GB as installs are
added. The journal says when the budget, not the count, decided. Both limits are settings in
`snapshot-to-r2.sh`, changed with `systemctl edit switchtender-witness-snapshot.service`.

A failed snapshot marks the unit failed and posts nothing. `systemctl list-timers` and
`systemctl status switchtender-witness-snapshot` show it.

## 10. OWNER: Add the uptime monitor

Sign up for an external HTTP monitor that alerts the owner's phone, and check
`https://witness.switchtender.com/healthz` every five minutes, alerting when the status is not 200
or the body lacks `"ok":true`. The check fails when either the service or the tunnel is down.

The order form states 99.5 percent a month, a budget of about 3 hours 39 minutes. A rehearsed
restore takes under an hour, so a fault must reach the owner within about two and a half hours to
keep the month. Installs lose no coverage from an outage shorter than their one-day push queue,
because queued beats are proven and countersigned on reconnect.

## 11. Test with an enrollment from a throwaway install

On a machine that holds the license issuer key, mint a one-day test license with the add-on:

```sh
switchtender license mint --tier team --addon hosted-witness --days 1 --hosts 500 \
  --org "Witness test" --key ISSUER_KEY_FILE --out witness-test-license.json
```

Start a throwaway server under that license with span beats on (`--span-cadence`) and enroll it
with `switchtender witness enroll`, the enrollment command this release ships, against
`https://witness.switchtender.com` and pinned to the key id from step 4. `switchtender witness
enroll --help` lists its flags. Then check:

- The enrollment prints the witness key id from step 4.
- After a few beats, `https://witness.switchtender.com/v1/installs/<key id>/attestation`, where
  `<key id>` is the throwaway install's producer key id, answers a statement with
  `refusals_total` 0.
- A bundle exported from the throwaway install passes `switchtender verify` held to that statement
  with `--witness`, as `switchtender verify --help` describes.
- The same install with no license is refused at enrollment, and the witness writes nothing.

Then remove the test record so it does not raise `silent_install` a day later. `KEY_HEX` is the
throwaway install's producer key id without its `sha256:` prefix. The service must be stopped,
because it holds every record in memory while it runs. The rename is the step the witness's own
retention takes to retire a record, and the next start finishes the deletion of any directory
named that way:

```sh
systemctl stop switchtender-witness
mv /var/lib/switchtender-witness/installs/KEY_HEX \
  /var/lib/switchtender-witness/installs/KEY_HEX.retired
systemctl start switchtender-witness
ls /var/lib/switchtender-witness/installs
```

The listing no longer shows `KEY_HEX` or `KEY_HEX.retired`.

## Restore

`restore.sh` restores the newest snapshot, or the one named on its command line. It refuses to run
while the service runs, when the key file is missing, or on a snapshot that holds a key. It moves
the current state directory aside to `/var/lib/switchtender-witness.before-restore-TIME` rather
than deleting it, cuts back any head log or finding record the snapshot caught partway through a
line, and leaves the `memory-restored` marker. On the next start the service records one
`memory_restored` note per enrolled install, naming the size its memory now holds, and removes the
marker. Each install's next push names a size the witness no longer holds, the witness answers with
the size it does hold, and the install proves again from there. The notes never fail a verify.

### Same droplet, state lost or damaged

```sh
systemctl stop switchtender-witness
/usr/local/lib/switchtender-witness/restore.sh
systemctl start switchtender-witness
journalctl -u switchtender-witness -o cat --no-pager | grep memory_restored
```

### New droplet

1. **OWNER:** power off the old droplet if it still runs, so two connectors never serve the tunnel
   with different memories. Create a new droplet like the first, Ubuntu 24.04, and on it run
   `ufw default deny incoming`, `ufw allow 22/tcp`, `ufw enable`, and
   `apt-get install -y unattended-upgrades`.
2. Install cloudflared and move the tunnel to it. The DNS record points at the tunnel, not the
   droplet, so it does not change. On the droplet:

   ```sh
   curl -fsSLO \
     https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64.deb
   dpkg -i cloudflared-linux-amd64.deb
   install -d -m 0755 /etc/cloudflared
   ```

   On the owner's Mac, `scp ~/.cloudflared/TUNNEL_ID.json root@DROPLET:/etc/cloudflared/`. On the
   droplet, `chmod 0400 /etc/cloudflared/TUNNEL_ID.json`, write `/etc/cloudflared/config.yml` from
   `cloudflared.yml.example` with the real id, and run `cloudflared service install`.
3. Run steps 1 and 2. Do not start the service yet: without the key it would create a new one.
4. **OWNER:** restore the key from the offline copy. On the owner's Mac,
   `scp witness-key.tar.age root@DROPLET:/root/`. On the droplet, where age asks for the
   passphrase:

   ```sh
   apt-get install -y age
   install -d -m 0700 -o switchtender-witness -g switchtender-witness \
     /var/lib/switchtender-witness-key
   age -d /root/witness-key.tar.age | tar -C /var/lib/switchtender-witness-key -xpf -
   chown switchtender-witness:switchtender-witness \
     /var/lib/switchtender-witness-key/witness-key.json
   rm /root/witness-key.tar.age
   ```

5. Paste the Slack webhook URL into the env file again, from the Slack app's settings.
6. Write `/etc/switchtender-witness/rclone.conf` as step 9 does. If the token's values were not
   kept, the owner creates a new token in the dashboard and deletes the old one.
7. Restore and start:

   ```sh
   install -d -m 0755 /usr/local/lib/switchtender-witness
   install -m 0755 snapshot-to-r2.sh restore.sh /usr/local/lib/switchtender-witness/
   /usr/local/lib/switchtender-witness/restore.sh
   systemctl enable --now switchtender-witness
   journalctl -u switchtender-witness -o cat --no-pager | grep -m1 'witness: watching'
   curl -fsS https://witness.switchtender.com/.well-known/loomseal.json
   ```

   The journal line and the key list must name the key id published in step 6. A different id
   means the key was not restored: stop the service at once and repeat step 4.
8. Install the snapshot timer as step 9 does.

## Routine checks

```sh
systemctl status --no-pager switchtender-witness switchtender-witness-snapshot.timer
journalctl -u switchtender-witness --since -1d -o cat --no-pager | grep -E 'REFUSAL|NOTE'
RCLONE_CONFIG=/etc/switchtender-witness/rclone.conf \
  rclone lsf r2:switchtender-witness-state/snapshots | sort | tail -1
```

The last command names the newest snapshot, whose name is its UTC time.
