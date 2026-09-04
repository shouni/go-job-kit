# ✍️ Go Job Kit

[![CI](https://github.com/shouni/go-job-kit/actions/workflows/ci.yml/badge.svg)](https://github.com/shouni/go-job-kit/actions/workflows/ci.yml)
[![Status](https://img.shields.io/badge/Status-Active-brightgreen)](#)
[![Language](https://img.shields.io/badge/Language-Go-blue)](https://go.dev/)
[![Go Version](https://img.shields.io/github/go-mod/go-version/shouni/go-job-kit)](https://go.dev/)
[![GitHub tag (latest by date)](https://img.shields.io/github/v/tag/shouni/go-job-kit)](https://github.com/shouni/go-job-kit/tags)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Reference](https://pkg.go.dev/badge/github.com/shouni/go-job-kit.svg)](https://pkg.go.dev/github.com/shouni/go-job-kit)

## 🚀 概要 (About) - 投入・記録・一覧。成果物のドメインには踏み込まない

**Cloud Tasks へ投入し、オブジェクトストレージへ成果物を書き出す非同期ジョブ**の共通部分を引き受けます。
「投入する → 進行状況を記録する → 履歴をページングして一覧する」という骨格は、生成物が音楽であれ
動画であれ漫画であれ同じ形になるので、その骨格だけをここに置いています。

**成果物そのもののドメインには踏み込みません。** 生成物を表す型はアプリ側に残り、ここが扱うのは
状態とジョブ ID だけです。認証・HTTP・通知はそれぞれ `gcp-kit` / `go-http-kit` / `go-notify` の担当です。

ジョブ状態を Firestore に置く場合は、このライブラリではなく
[gcp-kit の `jobstatus`](https://github.com/shouni/gcp-kit) を使ってください。ここの `joblist` /
`paging` / `cache` は「クエリの無いオブジェクトストレージで一覧を出す」ための道具なので、
クエリのあるバックエンドでは不要になります。

---

## 📦 パッケージ構成 (Package Structure)

ジョブの一生の、どの局面を誰が担当するかです。

| パッケージ | 局面 | 役割 |
| --- | --- | --- |
| `jobstatus` | 投入・実行・参照 | 進行状況を状態ファイルとして読み書きし（`Store`）、ワーカーの入口で再実行ガードと引き継ぎを行う（`Recorder`） |
| `joblist` | 一覧 | プレフィックス直下の疑似ディレクトリ名をジョブ ID として集める（`Collect`） |
| `paging` | 一覧 | ジョブ ID を新しい順に並べ替えてページを切り出し、そのページ分を並行に読み込む（`SelectIDs` / `LoadPage`） |
| `cache` | 一覧 | 一覧走査の結果（`IDList`）とジョブごとのメタデータ（`TTL`）をインメモリに保持する |

`paging` は標準ライブラリだけに依存します。各シンボルの引数・戻り値・エラーは
[pkg.go.dev](https://pkg.go.dev/github.com/shouni/go-job-kit) にあります。

---

## 🚦 使い方 (Usage)

状態の型を定義し、`Store` を組み立てて、投入直後に `queued` を記録するまでです。

```go
// 成果物の保存先はサービスごとに形が違うため、共通フィールドだけを Status が持ちます。
// サービス固有のフィールドは、これを埋め込んだ型に足してください。
type JobStatus struct {
    jobstatus.Status
    OutputDir string `json:"output_dir,omitempty"`
}

// storage には remoteio.Store をそのまま渡せます。
store := jobstatus.NewStore[JobStatus](
    storage,
    jobstatus.UnderJobDir("gs://"+bucket+"/jobs"), // → .../jobs/{jobID}/status.json
)

// JobID と UpdatedAt は Save が打刻します。
err := store.Save(ctx, jobID, JobStatus{State: jobstatus.StateQueued, Command: "generate"})
```

ワーカー側の入口（`Recorder.Begin`）と一覧側（`joblist.Collect` → `cache.IDList` → `paging.LoadPage`）の
例は godoc にあります。**踏むと高くつく点も、それぞれの godoc に書いてあります** — 状態を読めなかった
ときに再実行ガードをどちらへも倒さないこと、`LoadPage` が load の失敗を返さず行を落とすこと、
並べ替えキーに既定を置いていないこと、埋め込みをやめると保存済みの `status.json` が読めなくなること。

---

## 🤝 依存関係 (Dependencies)

| パッケージ | 用途 |
| --- | --- |
| [shouni/go-remote-io](https://github.com/shouni/go-remote-io) | 状態ファイルの読み書きと一覧走査（GCS / S3 / ローカルを透過的に扱う） |
| [shouni/go-utils](https://github.com/shouni/go-utils) | ジョブ ID の検証・正規化 (`jobid`) |
| [jellydator/ttlcache](https://github.com/jellydator/ttlcache) | TTL 付きインメモリキャッシュ |
| [golang.org/x/sync](https://pkg.go.dev/golang.org/x/sync/singleflight) | 同時のキャッシュミスで一覧走査を 1 回にまとめる（`cache.IDList`） |

---

## 📜 ライセンス (License)

このプロジェクトは [MIT License](https://opensource.org/licenses/MIT) の下で公開されています。
