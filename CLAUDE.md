# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

All three packages (`paging`, `jobstatus`, `cache`) are implemented and tested. No consumer has
adopted them yet — `ap-comp` / `ap-mv` / `ap-comic` still run their own copies, so this library
has no released tag and its API can still change without a migration.

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

`ap-comp`, `ap-mv`, and `ap-comic` each implement the same job scaffolding — submit to Cloud Tasks,
record progress to GCS, page through history — in their own `internal/` trees. Several files are
identical apart from the import path. This library extracts only that shared skeleton.

**The origin code is the primary reference.** When implementing a package here, read the existing
implementations first; they carry the operational knowledge (Cloud Run quirks, at-least-once
retry handling) that motivated each decision:

| Package | Origin |
| --- | --- |
| `paging` | `../ap-comp/internal/repository/history_page.go` (byte-identical to `../ap-comic`'s, modulo comments) |
| `jobstatus` | `../ap-comp/internal/repository/job_status.go` + `../ap-comp/internal/domain/job_status.go`, and the `../ap-comic` counterparts |
| `cache` | `../ap-comp/internal/repository/history_cache.go`, `../ap-mv/internal/repository/history_cache.go` |

The three apps' copies have already drifted. Known divergences this library settles rather than
reproduces:

1. `ap-comp` normalizes cache keys via `jobid.Sanitize`; `ap-mv` uses the raw job ID.
2. `ap-comp`'s job status repository has `Delete`; `ap-comic`'s does not.
3. `ap-comp` and `ap-comic` run `go cache.Start()` on their ttlcaches; `ap-mv` never does, so its
   expired history/recipe entries are never reclaimed. `cache.NewTTL` starts the janitor itself
   so this cannot be forgotten.
4. `ap-mv` sorts history by a timestamp extracted from the job ID, the others sort the ID
   lexically — **this one is not drift**. See below.

`paging`'s default lexical sort is only correct when job IDs share a single prefix.
`ap-comp` (`{timestamp}-{uuid8}`, no prefix) and `ap-comic` (`c{timestamp}-{hex8}`, one fixed
prefix) satisfy that. `ap-mv` builds IDs with `jobid.New`, whose format is
`{prefix}-{timestamp}-{rand}`, and uses seven prefixes (`video-recipe`, `recipe`, `mv`, `short`,
`regen-keyframe`, `regen-section`, `regen-zip`) — so lexical order groups by prefix and puts
older jobs first. `ap-mv` must pass `paging.WithSortKey`. Note that `jobid.New`'s doc comment in
`go-utils` claims lexical sort always yields newest-first; that holds only for single-prefix use.

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
- **No app domain types.** Music, video, and comic result types stay in their own repositories.
  This library handles state and IDs. Auth, HTTP, and notification concerns belong to `gcp-kit`,
  `go-http-kit`, and `go-notifier` respectively.

## Dependencies

Planned: `go-remote-io` (status file I/O), `go-utils/jobid` (ID validation), `jellydator/ttlcache/v3`.

Pin these to the versions the consuming apps already use — currently `go-remote-io v1.7.2`,
`go-utils v1.3.0` — so adopting this library does not force an upgrade elsewhere. Version drift
across the sibling modules is a live problem (`go-prompt-kit` is split across four versions today).
