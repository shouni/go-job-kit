# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

All four packages (`paging`, `jobstatus`, `cache`, `joblist`) are implemented and tested,
released through v1.1.2, and consumed by four apps — `ap-comp`, `ap-mv`, `ap-story` and
`ap-voice`, all pinned to v1.1.2. (`ap-story` is the comic app: it started life as `ap-comic`
and was rebuilt under the new name with fresh git history, so `ap-comic` commit hashes resolve
only in the retired GitHub repository, not in any local checkout.)

**Every exported entry point has real callers** — the original three apps use all of
`jobstatus.Store`, `jobstatus.Recorder`, `joblist.Collect`, `paging.SelectIDs`,
`paging.LoadPage`, `cache.TTL`, `cache.IDList`; `ap-voice` uses the `jobstatus` and `paging`
halves. Nothing here is speculative, which is worth keeping true: an API with no caller cannot
tell you whether its shape is right.

`joblist` (added after v1.0.5, shipped in the v1.1.x line) was the newest extraction:
`ap-comp` (`repository/history_query.go`), `ap-mv` and `ap-story`
(`repository/history_listing.go`) each carried the same pseudo-directory scanner — delimiter
listing, keep only "/"-suffixed entries, `path.Base`, dedup — differing only in per-app
filters, which became `WithKeep` / `WithValidIDsOnly`. All three have since migrated and now
call `joblist.Collect`. `ap-voice` is the deliberate exception: its listing walks every object
*without* a delimiter because one pass also derives per-job `HasAudio`, so it has no scanner
to replace.

**The API is load-bearing.** A breaking change here means a migration in four services, so
prefer adding to the surface over reshaping it; reshape only when the current shape is what
makes callers get it wrong (`paging`'s sort key is the one case so far), and land the apps in
the same round. Two constraints cannot be relaxed at all without touching stored data:
`jobstatus.Status` is embedded by the consumers so its JSON stays flat (see below), and
`paging.PageMeta`'s tags are what their HTTP responses already return.

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

`ap-comp`, `ap-mv` and the comic app (then `ap-comic`, now `ap-story`) each carried the same
job scaffolding — submit to Cloud Tasks, record progress to GCS, page through history — in
their own `internal/` trees, several files byte-identical apart from the import path. This
library holds that shared skeleton; the apps now delegate to it.

**The original implementations are the primary reference** — they carry the operational knowledge
(Cloud Run quirks, at-least-once retry handling) behind each decision. They are no longer at the
paths below, because adopting this library rewrote or deleted them. Read them from the commit
*before* each migration:

```bash
git -C ../ap-comp  show 7827cea^:internal/repository/history_page.go     # paging
git -C ../ap-comp  show 7827cea^:internal/repository/job_status.go       # jobstatus
git -C ../ap-mv    show aa619d4^:internal/repository/history_cache.go    # cache
```

The comic app's copy (`ap-comic 250ff0d^`, `internal/domain/job_status.go`) is GitHub-only:
`ap-story` starts with fresh history, so that hash resolves in the retired `ap-comic`
repository but in no local checkout.

The migration commits themselves (`ap-comp 7827cea`, `ap-comic 250ff0d`, `ap-mv aa619d4`) show
what each app kept locally versus handed over.

The copies had drifted before adoption. These divergences are settled here rather than
reproduced, so do not reintroduce them as caller options:

1. `ap-comp` normalized cache keys via `jobid.Sanitize`; `ap-mv` used the raw job ID. `cache.TTL`
   normalizes internally.
2. `ap-comp`'s job status repository had `Delete`; the comic app's did not. `jobstatus.Store` has it.
3. `ap-comp` and the comic app ran `go cache.Start()` on their ttlcaches; `ap-mv` never did, so its
   expired entries were never reclaimed. `cache.NewTTL` starts the janitor itself, which is why
   it also owns `Close`.
4. `ap-mv` sorted history by a timestamp extracted from the job ID, the others sorted the ID
   lexically — **this one was not drift**. All three now extract the timestamp via
   `jobid.SortKey`. See below.

Lexical sort on the job ID is only correct when IDs share a single prefix, and this has now
stopped holding for every app — **all three must pass `jobid.SortKey`**. That option used to be
a `WithSortKey` variadic with a lexical default; it is now a required `sortKey` parameter on
`SelectIDs` / `LoadPage`, because getting it wrong returns a perfectly normal-looking list with
a silently wrong order. `nil` still means "compare the IDs themselves", but it has to be typed
out at the call site.

`ap-mv` always had to: it builds IDs with `jobid.New` (`{prefix}-{timestamp}-{rand}`) across
seven prefixes (`video-recipe`, `recipe`, `mv`, `short`, `regen-keyframe`, `regen-section`,
`regen-zip`), so lexical order groups by prefix and puts older jobs first.

`ap-story` moved to `jobid.New("c")` after `go-utils` v1.5.0, changing its format from
`c{timestamp}-{hex8}` to `c-{timestamp}-{hex12}`. Lexical order now splits old from new
regardless of date — `-` (0x2D) sorts below `2` (0x32), so every newly minted ID lands *after*
every legacy one. Old jobs stay in GCS indefinitely, so the two formats coexist for good.

`ap-comp` moved to `jobid.New("comp")` in the same round, from an unprefixed
`{timestamp}-{uuid8}`. Its own IDs happen to keep sorting correctly — a letter outranks a digit,
so every new ID lands above every legacy one, and every new job really is newer — but that is a
coincidence of the prefix's first byte, not a property to rely on. It also accepts
caller-supplied `job_id` and receives IDs from other services via `ap-mcp`, which breaks the
coincidence outright.

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

Packages sit at the repository root (`jobstatus/`, `joblist/`, `paging/`, `cache/`) — no
`pkg/` prefix. `joblist` collects the IDs, `paging` orders and slices them, `cache` keeps both
the scan result (`IDList`) and per-job metadata (`TTL`) warm; `paging` alone stays
stdlib-only, which is why the storage-touching scanner is its own package rather than a
`paging` helper.

Design constraints that are not visible from any single file:

- **Job IDs are a security boundary.** They land in both URL paths and storage paths. Normalization
  via `go-utils/jobid` belongs inside this library — at storage-path construction and at cache-key
  construction — so callers cannot forget it.
- **Existing `status.json` objects in GCS must stay readable.** The apps' current structs serialize
  flat: `job_id`, `command`, `state`, `title`, `error`, `attempts`, `queued_at`, `updated_at`, plus
  app-specific fields (`storage_uri` / `recipe_storage_uri` in ap-comp, `output_dir` in ap-story).
  The intended approach is for apps to embed `jobstatus.Status` in their own struct, since Go
  flattens embedded structs in JSON. Do not introduce a nested payload field.
- **Status records keep one generation only** — overwritten in place, written with `no-store` so
  CDN and browsers do not cache them.
- **A missing status and an unreadable one are different errors.** `remoteio` wraps not-found
  in `os.ErrNotExist`, so `Store.Get` returns `ErrNotFound` only when the record does not exist
  and `ErrUnavailable` when it exists but could not be read (handlers map them to 404 / 503; no
  extra storage round-trip). The split exists because the two demand opposite decisions:
  missing means "proceed", but treating unreadable as missing would re-run finished jobs —
  `Recorder.AlreadySucceeded` therefore returns the error instead of guessing either way, so
  the worker can hand the task back to Cloud Tasks redelivery. Both errors wrap the underlying
  cause so the reason survives into logs; do not collapse that wrapping. A third error,
  `ErrInvalidJobID`, covers an ID that fails normalization — a caller-input problem (400), not a
  storage one; without it, handlers answer a malformed URL with a 5xx and invite a retry that
  cannot succeed.
- **`Recorder.Record` refuses to roll a finished job back, and `queued` is exempt.** The rule
  itself is in `rolledBack`; what is invisible from here is why the exemption has to exist.
  Same-ID resubmission is a normal flow in the apps — `ap-comp` accepts a caller-supplied
  `job_id`, `ap-voice` re-submits from the history detail page with the same ID — and every
  submission path writes `queued` before enqueueing. Blocking that write would leave the record
  at `succeeded`, so the re-run guard would read the regeneration as already done and ACK the
  task without ever running it.
- **`status.json` goes through `encoding/json/v2`, not `json.Decoder`.** The reason is the
  bullet above, reached from the read side: the v1 decoder took the last of duplicate keys, so a
  half-written record followed by a second write decoded as the *newer* state and rolled a
  finished job back. `Store.Get` carries the details.
- **No app domain types.** Music, video, and comic result types stay in their own repositories.
  This library handles state and IDs. Auth, HTTP, and notification concerns belong to `gcp-kit`,
  `go-http-kit`, and `go-notify` respectively.

## Dependencies

`go-remote-io` (status file I/O), `go-utils/jobid` (ID validation), `jellydator/ttlcache/v3`,
and `golang.org/x/sync` (singleflight, deduplicating concurrent `cache.IDList` misses).

Pin the shouni modules to the versions the consuming apps already use, so adopting this
library does not force an upgrade elsewhere. Check the apps' `go.mod` before bumping here; the
kit lagging behind them is harmless (MVS resolves upward), but running ahead forces upgrades.

The kit is currently **ahead**: it needs `go-remote-io v1.10.1` and `go-utils v1.7.0`, while
all four apps still pin `go-remote-io v1.9.0` and `go-utils v1.6.1`. That is deliberate —
`jobstatus.ErrInvalidJobID` wraps the `jobid.ErrEmpty` / `ErrTooLong` / `ErrInvalidFormat`
sentinels that only exist from `go-utils v1.7.0` — but it means the next tag drags all four
apps forward. Bump them in the same round rather than letting MVS do it silently.
