# StrmSync

A fork-local storage driver that mirrors one or more source paths into a local
tree of `.strm` files and keeps that tree up to date on a schedule.

It exists alongside the upstream `Strm` driver rather than replacing it. Use
`Strm` for a virtual view; use `StrmSync` when you want files on disk for a
media server to index.

## How it differs from upstream Strm

| | upstream `Strm` | `StrmSync` |
|---|---|---|
| when files are written | from inside `ObjsUpdateHook`, on a goroutine `op` spawns with `context.WithoutCancel` | by the scan itself, synchronously, on its own goroutine |
| what triggers a write | somebody browsing the directory | a scheduled full pass |
| local layout | `SaveStrmLocalPath/<basename>` | `localPath` + the mount-relative path |
| deletion | `os.RemoveAll` on a subtree | `.strm` only, `os.Remove` only |

The pull model is the reason most of the hazards in the hook model do not
apply here: the write is cancellable, it has natural backpressure, and there is
no shared global trie to race.

## Deletion safety

`localMode: sync` is the only mode that deletes anything. Four independent
guards stand between a bad listing and a lost library:

1. **Only `.strm` is ever a candidate.** The managed set does not widen when the
   attachment switches are turned on, so a scraper's `poster.jpg`, `.nfo` or
   `.actors/` is not something this code can delete regardless of
   configuration. The cost is that an attachment whose source file disappears
   is left behind.
2. **An empty remote listing never deletes.** If a directory comes back with no
   entries while the local side has some, the batch is refused outright.
3. **A per-directory cap** (`maxDeletePerDir`, default 50) refuses a batch
   larger than itself. Orphaned directories are counted as their contents, not
   as one entry.
4. **A per-pass budget** (cap × 4) bounds the whole sweep, because a listing
   failure spread thinly over a thousand directories produces a thousand
   individually plausible deletions. Spending it latches deletions off for the
   rest of the pass; writing continues.

`os.RemoveAll` does not appear in this package. A directory is removed only
once it is genuinely empty, and only through `os.Remove`, so losing the race
against a scraper writing into it is harmless.

`disableDeleteProtect` turns off guards 2, 3 and 4 together. Guard 1 is
structural and cannot be turned off.

## Operational notes

- **`siteUrl` is effectively required.** A scheduled pass has no HTTP request in
  its context, so `common.GetApiUrl` returns `""` and every generated strm
  would lose its host. `Init` refuses to start without it unless `withoutUrl`
  is set.
- **`scanIntervalMinutes` must exceed how long a pass takes.** A tick that
  lands while the previous pass is still running is dropped, and `time.Ticker`
  does not make it up, so the effective interval silently becomes 2× or 3×.
  Aliyun's list limiter caps at 3.9/s: roughly 100 seconds per 385 directories.
- **A pass sees the unfiltered tree.** There is no user or meta in a background
  context, so `internal/fs` applies neither the `hide` regex nor per-user
  permissions. Anything under a configured source path gets a strm file, even
  if browsing the same mount in the web UI would not show it.
- **Every listed directory fires `objsUpdateHook`.** `fs.List` has no way to
  suppress it. Two things are registered globally:
  - the search indexer, which is a no-op unless `auto_update_index` is on —
    consider leaving it off, since a full pass would otherwise rewrite the
    index every interval;
  - upstream `Strm`'s `UpdateLocalStrm`. **Do not point a `StrmSync` storage and
    an upstream `Strm` storage with `SaveStrmToLocal` at the same source
    path.** Our scan would drive upstream's uncancellable writer, including its
    `os.RemoveAll` deletions, at scan speed.
- **`Refresh: true` on every pass** bypasses the source storage's directory
  cache on read and repopulates it on write. Expect the source's cache to hold
  the whole tree for its `cache_expiration`, and expect one real API call per
  directory per pass. `scanRateLimitPerSec` is the throttle; 0 means rely on the
  source driver's own limiter.
- **`localPath` should belong to this storage alone.** It must be absolute and
  not a filesystem root. Orphan pruning is skipped at the mount root precisely
  so that a neighbouring tree survives, but sharing the directory is still a bad
  idea.
- **Case-insensitive filesystems.** If the source renames `movie.mkv` to
  `Movie.mkv`, the on-disk name will not change while the content comparison
  says the file is already correct, and the deletion pass may then remove it and
  rewrite it on the next pass. Linux filesystems are unaffected; SMB shares and
  HFS+ are not.

## Relationship to upstream

`util.go` and the `Get`/`List`/`Link` bodies in `driver.go` are adapted from
`drivers/strm` at the revision recorded in `UPSTREAM_BASELINE`. Nothing keeps
them in sync automatically, so `.github/workflows/fork-upstream-drift.yml` fails
when upstream touches those files past that point. Port what applies, then move
the SHA forward.

The only upstream file this driver modifies is `drivers/all.go`, which gains one
blank import.
