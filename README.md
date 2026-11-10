# ✍️ Go Job Kit

[![CI](https://github.com/shouni/go-job-kit/actions/workflows/ci.yml/badge.svg)](https://github.com/shouni/go-job-kit/actions/workflows/ci.yml)
[![Status](https://img.shields.io/badge/Status-Active-brightgreen)](#)
[![Language](https://img.shields.io/badge/Language-Go-blue)](https://go.dev/)
[![Go Version](https://img.shields.io/github/go-mod/go-version/shouni/go-job-kit)](https://go.dev/)
[![GitHub tag (latest by date)](https://img.shields.io/github/v/tag/shouni/go-job-kit)](https://github.com/shouni/go-job-kit/tags)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Reference](https://pkg.go.dev/badge/github.com/shouni/go-job-kit.svg)](https://pkg.go.dev/github.com/shouni/go-job-kit)

English | [日本語](README.ja.md)

## 🚀 About - submit, record, list. The artifacts' domain stays out

This library takes on the shared parts of **asynchronous jobs that are submitted to Cloud Tasks and write their artifacts to object storage**.
The skeleton — "submit → record progress → page through the history" — has the same shape whether the
output is music, video or comics, so that skeleton, and only that, lives here.

**It does not reach into the artifacts' own domain.** The types describing what was generated stay in
the apps; this library handles state and job IDs only. Authentication, HTTP and notifications belong to
`gcp-kit` / `go-http-kit` / `go-notify` respectively.

If you keep job state in Firestore, use
[gcp-kit's `jobstatus`](https://github.com/shouni/gcp-kit) instead of this library. `joblist` /
`paging` / `cache` here are tools for "listing from object storage, which has no queries", so a
backend with queries does not need them.

---

## 📦 Package Structure

Which package handles which phase of a job's life.

| Package | Phase | Role |
| --- | --- | --- |
| `jobstatus` | Submit, run, read | Reads and writes progress as a status file (`Store`), and guards against re-runs and carries state over at the worker's entry point (`Recorder`) |
| `joblist` | Listing | Collects the pseudo-directory names directly under a prefix as job IDs (`Collect`) |
| `paging` | Listing | Sorts job IDs newest first, cuts out a page, and loads that page concurrently (`SelectIDs` / `LoadPage`) |
| `cache` | Listing | Keeps the result of a listing scan (`IDList`) and per-job metadata (`TTL`) in memory |

`paging` depends on the standard library only. Arguments, return values and errors for every symbol are on
[pkg.go.dev](https://pkg.go.dev/github.com/shouni/go-job-kit).

---

## 🚦 Usage

Define the status type, build a `Store`, and record `queued` right after submitting.

```go
// Where artifacts are stored differs per service, so Status holds only the common fields.
// Add service-specific fields to the type that embeds it.
type JobStatus struct {
    jobstatus.Status
    OutputDir string `json:"output_dir,omitempty"`
}

// A remoteio.Store can be passed as storage directly.
store := jobstatus.NewStore[JobStatus](
    storage,
    jobstatus.UnderJobDir("gs://"+bucket+"/jobs"), // → .../jobs/{jobID}/status.json
)

// Save stamps JobID and UpdatedAt.
err := store.Save(ctx, jobID, JobStatus{State: jobstatus.StateQueued, Command: "generate"})
```

Examples for the worker's entry point (`Recorder.Begin`) and the listing side (`joblist.Collect` → `cache.IDList` → `paging.LoadPage`)
are in the godoc. **The costly pitfalls are written in each godoc too** — do not tip the re-run guard
either way when the status could not be read; `LoadPage` drops rows rather than returning load failures;
there is no default sort key; and stopping the embedding makes stored `status.json` files unreadable.

---

## 🤝 Dependencies

| Package | Purpose |
| --- | --- |
| [shouni/go-remote-io](https://github.com/shouni/go-remote-io) | Reading and writing status files and scanning listings (GCS / S3 / local, transparently) |
| [shouni/go-utils](https://github.com/shouni/go-utils) | Job ID validation and normalization (`jobid`) |
| [jellydator/ttlcache](https://github.com/jellydator/ttlcache) | In-memory cache with TTL |
| [golang.org/x/sync](https://pkg.go.dev/golang.org/x/sync/singleflight) | Collapses concurrent cache misses into a single listing scan (`cache.IDList`) |

---

## 📜 License

This project is released under the [MIT License](https://opensource.org/licenses/MIT).
