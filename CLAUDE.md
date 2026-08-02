# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

All three packages (`paging`, `jobstatus`, `cache`) are implemented and tested, released as
v1.0.1, and consumed by `ap-comp`, `ap-mv` and `ap-comic` — all three pinned to v1.0.1.

**The API is now load-bearing.** A breaking change here means a migration in three services,
so add to the surface rather than reshaping it, and tag a new minor when you do. Two constraints
in particular cannot be relaxed without touching stored data: `jobstatus.Status` is embedded by
the consumers so its JSON stays flat (see below), and `paging.PageMeta`'s tags are what their
HTTP responses already return.

Unreleased on top of v1.0.1 (tag as v1.1.0): `jobstatus.Recorder`, `cache.IDList`,
`paging.LoadPage`. All three are additive — v1.0.1 call sites compile unchanged. Each one
absorbs scaffolding the three apps still carry after adopting v1.0.1; see "What the apps
can hand over next" below. Nothing in the apps uses them yet.

## Commands

```bash
go build ./...
go vet ./...
go test ./... -race -cover
go test ./jobstatus -run TestStoredJSONStaysFlat   # single test
test -z "$(gofmt -l .)"                            # CI fails on unformatted files
golangci-lint run ./...
```

## Why this repository exists

`ap-comp`, `ap-mv` and `ap-comic` each carried the same job scaffolding — submit to Cloud Tasks,
record progress to GCS, page through history — in their own `internal/` trees, several files
byte-identical apart from the import path. This library holds that shared skeleton; the three
now delegate to it.

**The original implementations are the primary reference** — they carry the operational knowledge
(Cloud Run quirks, at-least-once retry handling) behind each decision. They are no longer at the
paths below, because adopting this library rewrote or deleted them. Read them from the commit
*before* each migration:

```bash
git -C ../ap-comp  show 7827cea^:internal/repository/history_page.go     # paging
git -C ../ap-comp  show 7827cea^:internal/repository/job_status.go       # jobstatus
git -C ../ap-comic show 250ff0d^:internal/domain/job_status.go
git -C ../ap-mv    show aa619d4^:internal/repository/history_cache.go    # cache
```

The migration commits themselves (`ap-comp 7827cea`, `ap-comic 250ff0d`, `ap-mv aa619d4`) show
what each app kept locally versus handed over.

The copies had drifted before adoption. These divergences are settled here rather than
reproduced, so do not reintroduce them as caller options:

1. `ap-comp` normalized cache keys via `jobid.Sanitize`; `ap-mv` used the raw job ID. `cache.TTL`
   normalizes internally.
2. `ap-comp`'s job status repository had `Delete`; `ap-comic`'s did not. `jobstatus.Store` has it.
3. `ap-comp` and `ap-comic` ran `go cache.Start()` on their ttlcaches; `ap-mv` never did, so its
   expired entries were never reclaimed. `cache.NewTTL` starts the janitor itself, which is why
   it also owns `Close`.
4. `ap-mv` sorts history by a timestamp extracted from the job ID, the others sort the ID
   lexically — **this one is not drift**. See below.

`paging`'s default lexical sort is only correct when job IDs share a single prefix.
`ap-comp` (`{timestamp}-{uuid8}`, no prefix) and `ap-comic` (`c{timestamp}-{hex8}`, one fixed
prefix) satisfy that. `ap-mv` builds IDs with `jobid.New`, whose format is
`{prefix}-{timestamp}-{rand}`, and uses seven prefixes (`video-recipe`, `recipe`, `mv`, `short`,
`regen-keyframe`, `regen-section`, `regen-zip`) — so lexical order groups by prefix and puts
older jobs first. `ap-mv` must pass `paging.WithSortKey`.

Since `go-utils` v1.5.0 the key to pass is `jobid.SortKey` — no app needs to hand-roll the
extraction. It reads all three formats above, so it is also the right key once IDs minted by one
service start appearing in another's list (they already cross via `ap-mcp`). `jobid.New`'s doc
comment used to claim lexical sort always yields newest-first; that was corrected in the same
release to say it holds only for single-prefix use.

## What the apps can hand over next

Adopting v1.0.1 moved the storage format, ID normalization and page arithmetic here, but left
three pieces of scaffolding duplicated in all three `internal/` trees. The unreleased additions
above exist to absorb them. The apps still have to be migrated — none of this has happened yet.

1. **`statusRecorder`** — `ap-comp/internal/pipeline/job_status.go`,
   `ap-mv/internal/worker/pipeline/job_status.go`, `ap-comic/internal/pipeline/job_status.go`.
   Same three behaviours in each (terminal guard, carry over `Attempts`/`QueuedAt`/`Title` from
   the previous record, warn-and-continue on save failure), ~110 lines apiece.
   → `jobstatus.Recorder`. The apps' `ports.JobStatusStore` is `Save(ctx, status)` /
   `Get(ctx, jobID) (*T, error)`; `jobstatus.StatusStore` matches `*jobstatus.Store` instead
   (`Save(ctx, jobID, status)`, value return), so a port-shaped store needs a ~10-line adapter.
   Note `ap-comic` carries `Title` over unconditionally while the others only fill it when
   empty; `Status.CarryOver` uses the fill-when-empty rule and `ap-comic`'s `markSucceeded`
   sets its title through `apply` afterwards, so the outcome is unchanged.
2. **Job ID list cache** — `ap-comp/internal/repository/history_query.go`,
   `ap-mv/internal/repository/job_id_cache.go`. Both wrap a `ttlcache` of `[]string` with a
   1-minute TTL, clone-on-read and explicit invalidation. `ap-comic` has no such cache and
   re-lists `comics/` on every history request. → `cache.IDList`.
3. **Concurrent page load** — `listHistoryPage` in `ap-comp`, `buildHistoriesConcurrently` in
   `ap-comic`, inline in `ap-mv`'s `ListHistoryPage`. → `paging.LoadPage`. `ap-comp` and
   `ap-comic` write into an indexed slice to keep the order; `ap-mv` appends under a mutex and
   then re-sorts by the same key it passed to `WithSortKey` — the ordering rule written out
   twice, in two places that have to stay in step. `LoadPage` keeps `SelectIDs`' order by
   construction, so `ap-mv`'s mutex and re-sort both go away.

## Architecture

Packages sit at the repository root (`jobstatus/`, `paging/`, `cache/`) — no `pkg/` prefix.

Design constraints that are not visible from any single file:

- **Job IDs are a security boundary.** They land in both URL paths and storage paths. Normalization
  via `go-utils/jobid` belongs inside this library — at storage-path construction and at cache-key
  construction — so callers cannot forget it.
- **`paging` assumes job IDs sort lexically into chronological order**, which holds for IDs from
  `jobid.New` (timestamp-prefixed). Descending lexical sort is what "newest first" means here.
- **Existing `status.json` objects in GCS must stay readable.** The apps' current structs serialize
  flat: `job_id`, `command`, `state`, `title`, `error`, `attempts`, `queued_at`, `updated_at`, plus
  app-specific fields (`storage_uri` / `recipe_storage_uri` in ap-comp, `output_dir` in ap-comic).
  The intended approach is for apps to embed `jobstatus.Status` in their own struct, since Go
  flattens embedded structs in JSON. Do not introduce a nested payload field.
- **Status records keep one generation only** — overwritten in place, written with `no-store` so
  CDN and browsers do not cache them.
- **A missing status and an unreadable one are the same error.** `remoteio` does not type its
  not-found, so `Store.Get` maps every read failure to `ErrNotFound` — handlers 404 rather than
  500 during a GCS outage, and the retry guard treats "cannot read" as "not finished". Both are
  deliberate (continuing beats stalling), but `Get` wraps the underlying error alongside
  `ErrNotFound` so the distinction survives into logs. Do not collapse that wrapping.
- **No app domain types.** Music, video, and comic result types stay in their own repositories.
  This library handles state and IDs. Auth, HTTP, and notification concerns belong to `gcp-kit`,
  `go-http-kit`, and `go-notifier` respectively.

## Dependencies

Planned: `go-remote-io` (status file I/O), `go-utils/jobid` (ID validation), `jellydator/ttlcache/v3`.

Pin these to the versions the consuming apps already use — currently `go-remote-io v1.7.2`,
`go-utils v1.3.0` — so adopting this library does not force an upgrade elsewhere. Version drift
across the sibling modules is a live problem (`go-prompt-kit` is split across four versions today).
