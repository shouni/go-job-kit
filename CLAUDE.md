# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

All three packages (`paging`, `jobstatus`, `cache`) are implemented and tested, released as
v1.0.2, and consumed by `ap-comp`, `ap-mv` and `ap-comic` — all three pinned to v1.0.2.

**Every exported entry point is used by all three apps** — `jobstatus.Store`,
`jobstatus.Recorder`, `paging.SelectIDs`, `paging.LoadPage`, `cache.TTL`, `cache.IDList`.
Nothing here is speculative, which is worth keeping true: an API with no caller cannot tell you
whether its shape is right.

**The API is load-bearing.** A breaking change here means a migration in three services,
so add to the surface rather than reshaping it, and tag a new minor when you do. Two constraints
in particular cannot be relaxed without touching stored data: `jobstatus.Status` is embedded by
the consumers so its JSON stays flat (see below), and `paging.PageMeta`'s tags are what their
HTTP responses already return.

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
4. `ap-mv` sorted history by a timestamp extracted from the job ID, the others sorted the ID
   lexically — **this one was not drift**. All three now extract the timestamp via
   `jobid.SortKey`. See below.

`paging`'s default lexical sort is only correct when job IDs share a single prefix, and this
has now stopped holding for every app — **all three must pass `paging.WithSortKey(jobid.SortKey)`**.

`ap-mv` always had to: it builds IDs with `jobid.New` (`{prefix}-{timestamp}-{rand}`) across
seven prefixes (`video-recipe`, `recipe`, `mv`, `short`, `regen-keyframe`, `regen-section`,
`regen-zip`), so lexical order groups by prefix and puts older jobs first.

`ap-comic` moved to `jobid.New("c")` after `go-utils` v1.5.0, changing its format from
`c{timestamp}-{hex8}` to `c-{timestamp}-{hex12}`. Lexical order now splits old from new
regardless of date — `-` (0x2D) sorts below `2` (0x32), so every newly minted ID lands *after*
every legacy one. Old jobs stay in GCS indefinitely, so the two formats coexist for good.

`ap-comp` (`{timestamp}-{uuid8}`, no prefix) is the only one whose own IDs still sort lexically,
but it accepts caller-supplied `job_id` and receives IDs from other services via `ap-mcp`, so it
uses the sort key too.

`jobid.SortKey` (added in `go-utils` v1.5.0) reads all of these formats, so no app hand-rolls the
extraction any more. `jobid.New`'s doc comment used to claim lexical sort always yields
newest-first; that was corrected in the same release to say it holds only for single-prefix use.

## What the apps handed over

The scaffolding the three apps duplicated after adopting v1.0.1 — status recorder, job-ID list
cache, concurrent page load — has all moved here (`jobstatus.Recorder`, `cache.IDList`,
`paging.LoadPage`). Two results of that migration still constrain things:

- **Their `JobStatusStore` port is now `StatusStore`'s shape** (`Save(ctx, jobID, status)`,
  value return). Each app briefly carried a 12-line adapter to bridge the two; writing it three
  times was the argument for moving the port instead. `*jobstatus.Store[T]` now satisfies the
  port directly, so none of them wraps it any more. Keep the shapes aligned — a divergence here
  reintroduces three adapters, not one.
- **`ap-comp`'s success record now inherits `Title`.** It used to carry `Attempts`/`QueuedAt` but
  not `Title`; `CarryOver`'s fill-when-empty rule means a success recorded without one picks up
  the running record's. Judged an improvement (the title settles mid-generation), not reverted.

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
