#!/usr/bin/env python3
"""Merge the stale library rows a Plex re-import leaves behind.

After a re-import every file has a new Plex rating key. The catalogue refresh inserts the new
key as its own mus_library_tracks row and leaves the old row, which still carries the
recording, checksum, sweep schedule and every item/quarantine/swap/blocked-candidate
reference. This does what internal/musicengine/identity_merge.go (mergeLibraryRowKey) does for
one track, for every track at once: the oldest row at a path with a stale key stays, other
rows' references move to it, fresh Plex facts are copied over, the duplicates are deleted and
the stayer takes Plex's current key.

Only a path that Plex lists exactly once is touched. A path Plex does not list, or lists more
than once, is skipped and counted. A stale row that has a checksum is merged only if the file
on disk still hashes to it.

Run it on Beast with the app STOPPED and the database backed up (Beast 2026-10-16: 81,639
merges, 16 skipped, about a minute):

  docker stop rock-media-center
  sudo sh -c 'cp -a /var/lib/rock-media-center/arrmada.db* /home/olares/rmc-db-backup/'
  sudo python3 scripts/merge-stale-plex-keys.py            # dry run: prints the plan
  sudo python3 scripts/merge-stale-plex-keys.py --apply
  docker start rock-media-center
"""
import argparse
import collections
import hashlib
import json
import sqlite3
import sys
import urllib.request

REFERENCING = ("mus_items", "mus_quarantine", "mus_swap_journal", "mus_blocked_candidates")


def plex_paths(url, token):
    """Every music track's rating keys, keyed by file path."""
    def get(path):
        req = urllib.request.Request(url + path, headers={"X-Plex-Token": token, "Accept": "application/json"})
        with urllib.request.urlopen(req, timeout=300) as r:
            return json.loads(r.read().decode("utf-8", "replace"))["MediaContainer"]

    out = collections.defaultdict(set)
    for sec in get("/library/sections").get("Directory", []):
        if sec.get("type") != "artist":
            continue
        for m in get("/library/sections/%s/all?type=10" % sec["key"]).get("Metadata", []):
            for md in m.get("Media", []):
                for pt in md.get("Part", []):
                    if pt.get("file"):
                        out[pt["file"]].add(m["ratingKey"])
    return out


def plan_merges(db, by_path, stats):
    rows = collections.defaultdict(list)
    for i, k, p, sha in db.execute("SELECT id, plex_rating_key, path, sha256 FROM mus_library_tracks ORDER BY id"):
        rows[p].append((i, k, sha))
    plan = []
    for p, rs in rows.items():
        if len(rs) < 2:
            stats["single"] += 1
            continue
        keys = by_path.get(p)
        if not keys:
            stats["skip_not_in_plex"] += 1
            continue
        if len(keys) != 1:
            stats["skip_ambiguous"] += 1
            continue
        live = next(iter(keys))
        if not any(k == live for _, k, _ in rs):
            stats["skip_live_key_has_no_row"] += 1  # the next catalogue refresh inserts it
            continue
        canon = next((i for i, k, _ in rs if k != live), None)
        if canon is None:
            stats["already_ok"] += 1
            continue
        if any(s for _, k, s in rs if k != live):
            try:
                with open(p, "rb") as f:
                    h = hashlib.sha256(f.read()).hexdigest()
            except OSError as e:
                stats["skip_unreadable"] += 1
                print("unreadable %s: %s" % (p, e))
                continue
            if any(s and s != h for _, k, s in rs if k != live):
                stats["skip_sha_mismatch"] += 1
                print("checksum mismatch, left alone: %s" % p)
                continue
            stats["sha_verified"] += 1
        plan.append((p, live, canon, [i for i, _, _ in rs if i != canon]))
    return plan


def apply_merges(db, plan):
    db.execute("BEGIN IMMEDIATE")
    for _, live, canon, others in plan:
        for x in others:
            for tbl in REFERENCING:
                db.execute("UPDATE %s SET track_id = ? WHERE track_id = ?" % tbl, (canon, x))
            xkey = db.execute("SELECT plex_rating_key FROM mus_library_tracks WHERE id = ?", (x,)).fetchone()[0]
            if xkey == live:
                # The new row holds what Plex just said; keep that and the stayer's history.
                db.execute(
                    """UPDATE mus_library_tracks SET
                         user_rating = (SELECT user_rating FROM mus_library_tracks WHERE id = :x),
                         play_count = (SELECT play_count FROM mus_library_tracks WHERE id = :x),
                         playlist_count = (SELECT playlist_count FROM mus_library_tracks WHERE id = :x),
                         last_seen_at = MAX(last_seen_at, (SELECT last_seen_at FROM mus_library_tracks WHERE id = :x)),
                         recording_id = COALESCE(recording_id, (SELECT recording_id FROM mus_library_tracks WHERE id = :x)),
                         sha256 = CASE WHEN sha256 = '' THEN (SELECT sha256 FROM mus_library_tracks WHERE id = :x) ELSE sha256 END
                       WHERE id = :canon""", {"x": x, "canon": canon})
            db.execute("DELETE FROM mus_library_tracks WHERE id = ?", (x,))
        db.execute("UPDATE mus_library_tracks SET plex_rating_key = ? WHERE id = ?", (live, canon))
    db.commit()


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--db", default="/var/lib/rock-media-center/arrmada.db")
    ap.add_argument("--plex", default="http://127.0.0.1:32400")
    ap.add_argument("--token-file", default="/home/olares/.config/plex-mirror/token")
    ap.add_argument("--apply", action="store_true", help="write the merges (default: dry run)")
    a = ap.parse_args()
    token = open(a.token_file).read().strip()
    by_path = plex_paths(a.plex, token)
    print("plex paths", len(by_path))
    db = sqlite3.connect(a.db, timeout=60)
    stats = collections.Counter()
    plan = plan_merges(db, by_path, stats)
    print(dict(stats), "to merge", len(plan))
    if not a.apply:
        return
    apply_merges(db, plan)
    print("merged", len(plan))
    for tbl in REFERENCING:
        n = db.execute("SELECT COUNT(*) FROM %s WHERE track_id IS NOT NULL AND track_id NOT IN (SELECT id FROM mus_library_tracks)" % tbl).fetchone()[0]
        print(tbl, "dangling", n)
        if n:
            sys.exit("dangling references in %s: restore the backup" % tbl)
    print("paths still with 2+ rows:", db.execute("SELECT COUNT(*) FROM (SELECT path FROM mus_library_tracks GROUP BY path HAVING COUNT(*) > 1)").fetchone()[0])
    db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
    print("integrity:", db.execute("PRAGMA integrity_check").fetchone()[0])


if __name__ == "__main__":
    main()
