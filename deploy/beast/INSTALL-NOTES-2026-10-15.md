# Beast install record, 2026-10-15

What was installed and what was observed. No secret values appear here; each secret's source
is named instead.

## Running

- Container `rock-media-center`, image `rock-media-center:local`, commit 74d22059, healthy.
- Published only on `10.0.0.13:8787` and `100.114.152.41:8787`  ; container port 7878; runs as 1000:100.
- Data: `/var/lib/rock-media-center` (1000:100, 750). Staging and quarantine:
  `/mnt/nas/downloads/beast-qbt/rmc/{staging,quarantine}` (created as 1000:100).
- Admin account: Blair's email login; password in `~/.config/rock-media-center/admin.env` on Goat (mode 600).

## Secrets in `deploy/beast/.env` on Beast (mode 600), and where each came from

| Variable | Source |
|---|---|
| `ARRMADA_SPOTIFY_CLIENT_ID`, `ARRMADA_SPOTIFY_CLIENT_SECRET` | `/home/olares/scripts/spotify-sync.py` |
| `ARRMADA_GLUETUN_CONTROL_KEY` | gluetun env `HTTP_CONTROL_SERVER_AUTH_DEFAULT_ROLE` |
| `ARRMADA_SLSKD_API_KEY` | `/var/docker-stacks/droppedneedle/slskd/slskd.yml` (`api_keys.repair`) |
| `ARRMADA_PLEX_TOKEN` | Plex `Preferences.xml` `PlexOnlineToken` (checked: section 5 answers 200) |

## Set up in the app (stored in the app database)

- SABnzbd `http://10.0.0.13:8085`, key from `/var/docker-stacks/sabnzbd/config/sabnzbd.ini`, path
  map `/downloads` -> `/mnt/nas/downloads/beast-qbt`. "Add missing categories" created
  `rmc-music` (and the unused `arrmada`, `arrmada-tv`, `arrmada-music`) in SABnzbd.
- qBittorrent `http://gluetun:8090`, user `sunygxc`, password from Sonarr's download client,
  same path map. Test passed after the ban below expired.
- Indexers: NZBgeek (`https://api.nzbgeek.info/api`, key from Prowlarr, browser User-Agent,
  categories 3040/3000) and TPB through Prowlarr torznab (`http://10.0.0.13:9696/3/api`). Both
  tests passed.
- Telegram notification for the six `musicengine.*` events, bot and chat from
  `spotify-sync.py`. No test message was sent.
- Engine settings: playlist 5hLOEmkXHaXQDDbvoFFLX8, floor 100 GB, margin 25 GB, quarantine share
  200 GB, automatic swaps OFF, Plex section 5, slskd `http://gluetun:5030`, gluetun control
  `http://gluetun:8000`. Lanes usenet, torrent and soulseek report available with VPN ok.

## VPN evidence (KTD20), all pass

1. `qbittorrent` and `slskd` NetworkMode `container:08a4fc046786...`.
2. gluetun Id `08a4fc046786d01d5897bac86707aa709b6656301369967dd0446e70873cf3b7` (same).
3. `FIREWALL_ENABLED_DISABLING_IT_SHOOTS_YOU_IN_YOUR_FOOT=on`.
4. `iptables -S OUTPUT`: policy DROP; accept only lo, established, local subnets from
   172.25.0.4, the VPN endpoint udp/1194, and `tun0`.
5. Egress through qBittorrent is the VPN provider address, which differs from Beast's own public address (both checked with ifconfig.me; values not recorded here).
6. From the app container: gluetun `/v1/vpn/status` -> `running` (with the control key).

## Watchdog

`/home/olares/bin/rmc-watchdog.sh`, env `/home/olares/.config/rmc-watchdog.env` (600), cron
`7-59/10 * * * *` to the journal (`journalctl -t rmc-watchdog`). Dry run: `ok (last tick 26s
ago)`, exit 0. The outage test (stop the container, expect one Telegram alert and one recovery)
was not run; it sends real messages.

## Problems found during the install, and fixes

- MusicBrainz rejected the ISRC lookup (`release-groups is not a valid inc parameter for the
  isrc resource`, 400) so no song could be pinned. Fixed in d1c0b855.
- slskd runs as root and leaves root-owned 755 folders, so the app cannot move its files; the
  app now copies them into staging and leaves slskd's original (f70dd93b). slskd's finished
  files are in `slskd/complete/`, not `slskd/`.
- slskd saves a second file of the same name as `<stem>_<digits><ext>`; the app now finds it
  (74d22059).
- The first compose file set `ARRMADA_QBITTORRENT_URL`/`ARRMADA_SABNZBD_URL`, which made the app
  treat Beast's shared clients as its own. Removed in 2d5087a2;
  the bundled qBittorrent row it created logged in with no password and qBittorrent banned the
  app's IP 172.25.0.3 for one hour (05:39-06:39). Sonarr was not affected.
- 1,564 of about 6,000 artist folders under `/mnt/nas/music` were 755 (not group-writable), so a
  placement or swap into one of them stopped with a reason. Fixed later the same day (below).

## First intake run, compared with `/home/olares/shazam-library-check.json` (06:45)

- Eric Church "Springsteen": already in the library as the studio recording
  (`Eric Church/Chief/09 - Springsteen.flac`), so nothing was downloaded.
- 30 of the 43 HAVE-OK songs matched the library with no download; the rest were waiting on
  Blair or still running at the time.
- Camila Cabello "Real Friends" (HAVE-OK in the old check) was downloaded: the library copy is
  the solo album version (ISRC USSM11706923, 3:34), the Spotify entry is the single with Swae
  Lee (ISRC USSM11806903, 3:44). Different recordings, so not a duplicate (R6).
- The four remixes were pinned to the remix recordings, not the originals.
- 18 songs went to Blair only because Spotify relinked the track id (R6 sends every relink to
  Blair).

## Release checks (both passed 2026-10-15) and automatic swaps

- Calibration (R13): 23 of 23 files classified as expected, list confirmed by Blair
  (`deploy/beast/calibration.tsv`).
- Plex path test (R13), track picked by Blair: 3 Doors Down "Kryptonite", swap 19 at 07:58 ET.
  Plex rating key 241948 unchanged; a 4-star rating and 1 play set for the test survived the
  swap, as did its "Gym Jiggling" membership; one Plex track at the path and none from
  dot-prefixed temp files; the old file is in quarantine and the swap is undoable. The test
  rating and play were cleared afterwards.
- Automatic swaps switched ON after both passed (plan KTD16).

## slskd user and music folder permissions (Blair, 2026-10-15)

- slskd now runs as 1000:1000 (`user: "1000:1000"` in `/var/docker-stacks/droppedneedle/docker-compose.yml`;
  backup `docker-compose.yml.bak-20261015-slskd-uid`). Its app folder and
  `beast-qbt/slskd` were chowned to 1000:1000. Checked after the restart: logged in to Soulseek
  through the VPN, API answers, writes to `/downloads/complete`; 7 remotely queued transfers kept.
  The engine now moves Soulseek downloads instead of copying them.
- 8,782 directories under `/mnt/nas/music` (8,664 mode 755, 118 mode 700, all 1024:users) got
  group rwx; other-user bits unchanged. Original modes: `/home/olares/music-dir-modes-before-20261015.tsv`
  (mode, owner, path) for rollback. Afterwards no directory lacks group rwx, and a 1000:100 write
  probe succeeded in formerly-755 folders.
