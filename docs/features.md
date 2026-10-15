# Everything Rock Media Center does

Rock Media Center does the jobs of Radarr, Sonarr, Overseerr and Tautulli in one
place, with one database and one web UI. It ships with qBittorrent and FlareSolverr already
wired up.

## Library and downloads

- **Movies** — search, grab, import, rename and upgrade. Keep several versions of a movie (say 4K and 1080p).
- **TV** — episodes and season packs, anime numbering (TheXEM and TheTVDB), and airing shows kept up to date.
- **Music** (preview, off by default) — artists, albums and whole-discography grabs. Switch it on in Settings → System → Modules.
- **Quality profiles** — describe the file you want (resolution, codecs, HDR, audio, a bitrate window) once, and it drives what's grabbed, how releases rank, which files in your library fit, and when upgrades stop.
- **Indexers** — any Torznab indexer, one-click sync from Prowlarr, and built-in TorrentLeech and 1337x. FlareSolverr handles Cloudflare-protected trackers.
- **Downloads** — the bundled qBittorrent sets itself up. Imports hardlink instead of copying, seeding rules clean up, stalled downloads fail over, and mismatched downloads wait in a review queue.
- **Usenet** (optional) — SABnzbd beside qBittorrent: `install.sh --with-sabnzbd` runs one that Rock Media Center sets up and connects to by itself, or connect your own. Newznab indexers (added in Rock Media Center or synced from Prowlarr) are searched with the torrent ones; the better release wins wherever it is, or prefer one network. A release SABnzbd can't finish is blocklisted and replaced; a full disk or a provider problem is reported instead. You bring your own Usenet provider and indexers ([Usenet](configuration.md#usenet-sabnzbd)).
- **Safety nets** — a blocklist, a recycle bin, nightly database backups and a full history for every item.

## Requests and people

- **Discover and requests** — browse and request movies and TV, Overseerr-style. Auto-approve per user, and import your existing Overseerr requests.
- **Users** — admin, manager, requester and read-only roles, with optional Plex or Jellyfin sign-in. Requesters get Discover, Requests and Calendar; from outside your network, everything but Calendar.
- **Calendar** — what's coming up.
- **Notifications** — Apprise alerts for admins; an in-app inbox and web push for requesters.
- **Problem reports** — anyone can report a problem with a title, and many have a one-click fix.

## Insights

- **Insights** — Plex and Jellyfin watch history and stats, plus Plex buffering diagnostics, Tautulli-style.

## Media servers

Connect Plex, Jellyfin or both (Settings → Connections); how Rock Media Center uses each is Settings → Media servers.

- **Watch buttons** — a request counts as ready once either server has the title, and the requester's notice and the title's page get a Watch button for each server that has it, their own server's first.
- **Library updates** — after every import, upgrade, rename or delete, the server is asked to rescan just that folder, with path mappings when its folders have other names.
- **Sign-in** — family can sign in with Plex, or with their Jellyfin username and password (or Jellyfin's Quick Connect code); each server has its own switch and auto-approve choices.
- **Insights** — plays recorded from each server, with a server badge on every stream. Jellyfin keeps no watch history Rock Media Center can read, so its history starts when its watch monitoring is switched on, and it doesn't report buffering.
- **Several versions of a movie** — Jellyfin shows them as one title only when every file name starts with the movie folder's name, as Rock Media Center's default naming does. With a naming scheme that doesn't, Jellyfin lists each version as its own movie; Plex groups them either way.
