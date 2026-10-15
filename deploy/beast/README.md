# Rock Media Center on Beast (unit U16)

Installs Rock Media Center as one new container beside the existing stack. Nothing else on
Beast is changed: Radarr, Sonarr, Plex, Navidrome, Rock Wave, gluetun, qBittorrent, slskd and
SABnzbd keep their current configuration. Files here: `compose.yml`, `rmc-watchdog.sh`,
`calibration.tsv` (proposed, Blair confirms before the calibration run), this file.

Every command below is one line. Run the Beast ones as `olares` (`ssh beast`). Written but not
yet run; the facts in "What was checked" were read from Beast on 2026-10-15.

## What was checked on Beast (read-only)

| Fact | Command that showed it |
|---|---|
| LAN IP 10.0.0.13, Tailscale IP 100.114.152.41 | `ssh beast 'ip -4 -o addr; tailscale ip -4'` |
| Host port 8787 is free (8080 zigbee2mqtt, 8091 Strata, 8765/8766/8791 NOC are taken) | `ssh beast 'ss -tlnp'` |
| `lidarr-stack_default` exists and holds gluetun at 172.25.0.4 | `ssh beast 'docker network ls; docker inspect gluetun'` |
| qBittorrent and slskd use `container:08a4fc04...` which is gluetun's id | `ssh beast 'docker inspect qbittorrent slskd gluetun'` (HostConfig.NetworkMode, Id) |
| gluetun firewall on: `FIREWALL_ENABLED_DISABLING_IT_SHOOTS_YOU_IN_YOUR_FOOT=on`, `FIREWALL_INPUT_PORTS=8090,5030`, iptables policy DROP, output allowed only via `tun0` and the VPN endpoint | container Env via `docker inspect gluetun`, and `docker exec gluetun iptables -S` |
| SABnzbd published on host `0.0.0.0:8085`; Plex native on `*:32400` | `ssh beast 'docker ps; ss -tlnp'` |
| `/mnt/nas/music` (10.0.0.7:/volume3/music, 1.3T free) and `/mnt/nas/downloads` (10.0.0.7:/volume2/downloads, 5.0T free) are NFS mounts on the host; music files are owned 1024:100 | `ssh beast 'mount; df -h; stat -c "%u:%g %n" /mnt/nas/music/*'` |
| `/var` is a separate 1.2T local volume, `/` has 46G | `ssh beast 'df -h / /var'` |
| `docker compose` v5.3.1, `jq`, `curl`, `python3`, `git` present; `shellcheck` is not | `ssh beast 'docker compose version; which jq curl python3 git shellcheck'` |
| The image takes `PUID`/`PGID`, chowns `/data`, then drops to that user | `docker/entrypoint.sh` in this repo |
| The 2026-10-11 audit has 65,740 rows | `ssh beast 'wc -l /home/olares/library-audit/scan.log'` |

Permissions (checked 2026-10-15): `/mnt/nas/downloads` itself is mode `d---------` owned by root,
so only its `beast-qbt` subfolder is mounted. `beast-qbt` is `1000:1000` mode 755 (seen from
inside the SABnzbd container: `docker exec sabnzbd ls -ldn /downloads`), and music folders are
`1024:100` mode 775. The app therefore runs as `1000:100`: owner of `beast-qbt`, group-writer of
the music folders. Placed files are `1000:100` mode 664. A music folder that is not group-writable
makes that placement fail with a clear reason instead of writing elsewhere.

## Install

1. Put the repo on Beast. Pick one. Pushing the fork is Blair's call, so the second is the default.
   - `ssh beast 'git clone <fork-url> /home/olares/projects/rock-media-center'`
   - `rsync -a --delete --exclude .git --exclude node_modules --exclude web/dist /Users/sunygxc/projects/arrmada/ beast:/home/olares/projects/rock-media-center/`
2. As uid 1000:100, create the engine's working folders under `beast-qbt` and confirm the music share is readable (throwaway container, removed on exit, from an image already on Beast). Both lines must print `ok`.
   - `ssh beast 'docker run --rm --user 1000:100 -v /mnt/nas/downloads/beast-qbt:/d alpine sh -c "mkdir -p /d/rmc/staging /d/rmc/quarantine && touch /d/rmc/.probe && rm /d/rmc/.probe && echo ok"'`
   - `ssh beast 'docker run --rm --user 1000:100 -v /mnt/nas/music:/m:ro alpine sh -c "ls /m >/dev/null && echo ok"'`
   - If either fails, stop and ask Blair before changing any NAS permission.
3. Create the data directory on local disk: `ssh beast 'sudo install -d -o 1000 -g 100 -m 750 /var/lib/rock-media-center'` (the entrypoint chowns it to PUID:PGID anyway)
4. Create `.env` next to the compose file (mode 600, never committed): `ssh beast 'cd /home/olares/projects/rock-media-center/deploy/beast && install -m 600 /dev/null .env'`
   Variables it may hold (all optional, the compose file has defaults; set values only by editing the file on Beast):
   - `RMC_LAN_IP` (default 10.0.0.13), `RMC_TS_IP` (default 100.114.152.41), `RMC_PORT` (default 8787): where the web port is published.
   - `RMC_VERSION`, `RMC_COMMIT`: build stamps shown on the dashboard.
   - `ARRMADA_PLEX_TOKEN`: optional alternative to entering the Plex token in the UI.
   - Any other `ARRMADA_*` variable the app documents. Secrets (Spotify, NZBgeek, Telegram) are entered in the UI, not here.
5. Build and start (first build takes several minutes; the repo's Dockerfile builds the web UI and Go binary): `ssh beast 'cd /home/olares/projects/rock-media-center/deploy/beast && docker compose --env-file .env -f compose.yml up -d --build'`
6. Confirm healthy, bound only to LAN and Tailscale: `ssh beast 'docker ps --filter name=rock-media-center --format "{{.Status}} {{.Ports}}"'` should show `healthy` and two bindings, `10.0.0.13:8787->7878` and `100.114.152.41:8787->7878`, with no `0.0.0.0`.
7. Open `http://10.0.0.13:8787` (or `http://100.114.152.41:8787` over Tailscale) and create the admin account on the first-run form. No public hostname or tunnel route is created.

## Setup in the running app

Use the UI; each item is stored in the app's database under `/var/lib/rock-media-center`.

1. SABnzbd: in SABnzbd (`http://10.0.0.13:8085`, Config, Categories) add category `rmc-music` with no special folder. In the app add SABnzbd at `http://10.0.0.13:8085` with its API key and set the engine's download category to `rmc-music`.
2. NZBgeek indexer row (newznab, search categories 3040 then 3000), with a browser User-Agent in the row's User-Agent field (U17), for example a current desktop Chrome string. Do not add it through Prowlarr; Prowlarr's NZBgeek search returned 0 results. Check: a manual search in the app returns results.
3. qBittorrent client: `http://gluetun:8090`, category `rmc-music`, with the same path mapping as SABnzbd (`/downloads` -> `/mnt/nas/downloads/beast-qbt`): qBittorrent also sees that folder as `/downloads`.
4. slskd client: `http://gluetun:5030`, with its API key from slskd's config. slskd saves finished files under `beast-qbt/slskd/complete/<remote folder>/` (`directories.downloads: /downloads/complete` in `/var/docker-stacks/droppedneedle/slskd/slskd.yml`); the engine looks there. slskd runs as root and leaves root-owned 755 folders, so the engine cannot move its files and copies them into staging instead, leaving slskd's original in `complete/` (checked 2026-10-15). Running slskd as 1000:1000 would make it a move; that changes the droppedneedle stack, so it is Blair's call.
5. Spotify: Client ID and secret of the existing developer app (currently hard-coded in `spotify-sync.py` on Beast; copy from there, do not paste them into any repo file). Watched playlist: "My Shazam Tracks".
6. Plex: URL `http://10.0.0.13:32400`, token entered in the UI, music section 5.
7. Telegram alerts: add one Apprise URL in the app's notification settings in the form `tgram://<bot-token>/<chat-id>` (Apprise's Telegram scheme; confirm against the Apprise page if the test message does not arrive). Use the test button; a message must arrive.
8. Leave "automatic swaps" OFF. The app refuses to turn it on until both release checks are recorded (KTD16).

## VPN evidence (KTD20)

Record all four outputs in the install notes. A lane is unavailable if either check fails.

1. qBittorrent and slskd share gluetun's namespace: `ssh beast 'docker inspect -f "{{.Name}} {{.HostConfig.NetworkMode}}" qbittorrent slskd'` must show `container:<gluetun id>` for both.
2. That id is gluetun's: `ssh beast 'docker inspect -f "{{.Id}}" gluetun'` must equal the id in line 1.
3. Kill switch setting on: `ssh beast 'docker inspect -f "{{range .Config.Env}}{{println .}}{{end}}" gluetun | grep ^FIREWALL_ENABLED'` must show `=on`.
4. Kill switch is actually loaded: `ssh beast 'docker exec gluetun iptables -S OUTPUT'` must show policy `DROP` and accept rules only for `lo`, `tun0`, the VPN endpoint and local subnets.
5. Egress is the VPN's, not Beast's: `ssh beast 'docker exec qbittorrent wget -qO- https://ifconfig.me'` must differ from `ssh beast 'curl -s https://ifconfig.me'`.

Do not drop the tunnel to watch traffic stop: Sonarr shares that qBittorrent. Needs Blair's go-ahead.

## Watchdog (KTD13)

The script reads `GET /api/v1/musicengine/heartbeat` (returns `{"last_tick": <unix seconds>}` with no session; 503 while the engine is switched off) and sends one Telegram message per outage and one on recovery. Needs `curl` and `jq` (both on Beast).

1. Install the script: `scp deploy/beast/rmc-watchdog.sh beast:/home/olares/bin/rmc-watchdog.sh` (from the repo root on Goat), then `ssh beast 'chmod 755 /home/olares/bin/rmc-watchdog.sh'`.
2. Create the env file with mode 600 and put three lines in it, `TELEGRAM_BOT_TOKEN=...`, `TELEGRAM_CHAT_ID=...` (the bot and chat `spotify-sync.py` already uses; do not paste values into chat or the repo) and `RMC_URL=http://10.0.0.13:8787`: `ssh beast 'install -m 600 /dev/null /home/olares/.config/rmc-watchdog.env'` then edit it on Beast.
3. Dry run, expect `ok (last tick Ns ago)` and exit 0: `ssh beast '/home/olares/bin/rmc-watchdog.sh; echo exit=$?'`
4. Cron line (every 10 minutes at :07, :17, ... so it does not share a tick with the existing :00 and */15 jobs; output to the journal so there is no flat log to rotate): `ssh beast "(crontab -l; echo '7-59/10 * * * * /home/olares/bin/rmc-watchdog.sh 2>&1 | logger -t rmc-watchdog') | crontab -"`
5. Prove it: `ssh beast 'docker stop rock-media-center'`, wait for at least two cron runs (within 20 minutes a Telegram message must arrive, exactly one), then `ssh beast 'docker start rock-media-center'` and wait for the recovery message. Read the run history with `ssh beast 'journalctl -t rmc-watchdog --since "-1h"'`.

Exit codes: 0 healthy, 1 unhealthy (alert sent or already sent), 2 config error, 3 unhealthy and the Telegram send failed (retried next run).

## Release checks (all three must be recorded before automatic swaps are enabled)

Order matters.

1. Calibration run (R13). Blair reads and confirms `deploy/beast/calibration.tsv` first (the rows are proposed). Then run the app's calibration check over it (admin page, "calibration run", paste or upload the file). Every row must match its expected verdict. The two required special cases are the Kate Bush "Nocturn" row (quiet-intro 24-bit, expected GENUINE) and the Einaudi "Melodia africana II" row (365 kbps, full spectrum, expected GENUINE). Any mismatch: stop, do not enable swaps, report the row.
2. Plex path test (R13). Pick a scratch track that Blair names. Before the swap read through the Plex API its rating key, user rating, play count (`viewCount`) and playlist membership: `curl -s "http://10.0.0.13:32400/library/metadata/<ratingKey>?X-Plex-Token=$PLEX_TOKEN"` and `curl -s "http://10.0.0.13:32400/playlists?X-Plex-Token=$PLEX_TOKEN"` (token read from the app's Plex setting or your shell, never pasted into files). Run one controlled swap from the app on that track, wait for the Plex scan, read the same values again. Rating key, rating, play count and playlist membership must be unchanged. Also confirm Plex ignored the dot-prefixed temp file during the scan (no stray track appeared). Record the result in the app's "Plex path test" action.
3. First intake run (U16 verification list). Run intake over "My Shazam Tracks", then compare against `/home/olares/shazam-library-check.json` with: `ssh beast 'jq . /home/olares/shazam-library-check.json | head -60'` and the app's item list. Required: songs the file lists as already genuine in the library produce no download; Eric Church's "Springsteen" is fetched as the studio recording; no recording is duplicated.
4. Only after 1 and 2 are recorded as passed, the app lets you turn on automatic swaps.

## Rollback

Stops and removes only this container; nothing else on Beast was changed. `ssh beast 'cd /home/olares/projects/rock-media-center/deploy/beast && docker compose -f compose.yml down'`

Also remove the watchdog cron line (`ssh beast "crontab -l | grep -v rmc-watchdog | crontab -"`). The data directory `/var/lib/rock-media-center`, the built image `rock-media-center:local`, `/home/olares/bin/rmc-watchdog.sh` and the env file are left in place; delete them only if Blair says to.
