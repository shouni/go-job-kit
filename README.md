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
例は godoc にあります。

---

## 🧭 記録の罠 (Recording)

**埋め込みをやめると保存済みの `status.json` が読めなくなります。** Go は埋め込み構造体を JSON で
フラットに展開するので、`Status` を埋め込んだ型はフラットな 1 段の JSON として保存されます
（形は `jobstatus.Status` の godoc にあります）。既存の状態ファイルをそのまま読み書きできるのは
このためで、**入れ子のペイロードを導入しないでください。**

状態ファイルは常に最新の 1 世代だけを保持し、上書きで更新します。

**「未記録」と「あるはずなのに読めない」は別のエラーです。** `ErrNotFound` と `ErrUnavailable` を
分けているのは、両者で取るべき判断が正反対だからです。記録が無いのは正常な状態なので先へ進んで
よく、読めなかっただけの場合を「無い」とみなすと、完了済みのジョブを未完了と誤認して生成をまるごと
やり直します。`ErrInvalidJobID`（URL に紛れ込んだ不正な ID）を混ぜるのも同じ理由で危険で、再試行しても
直らない入力を 5xx で返すと再試行を招きます。ハンドラーでは 404 / 503 / 400 へ分けてください。

**再実行ガードは、読めなかったときにどちらへも倒しません。** `Begin` / `AlreadySucceeded` は未記録
（`ErrNotFound`）だけを「完了していない」とし、`ErrUnavailable` はエラーとして返します。「未完了」に
倒すと完了済みのジョブを作り直してガードが防ぐはずのコストを自分で発生させ、「完了済み」に倒すと
未完了のジョブがタスクごと ACK されて二度と実行されません。呼び出し側はそのまま Cloud Tasks の
再配信に委ねてください。

**逆向きの巻き戻しも塞いであります。** `Record` は、完了済みの記録へ `running` / `failed` を書こうと
したときは保存しません。再配信されたタスクは状態を組み立て直すため、そのまま書くと完了したジョブが
ポーリング中の画面にも再実行ガードにも「まだ終わっていない」と映ります。**`queued` は例外で、
そのまま保存します。** 同じジョブ ID での作り直し（再生成）を止めてしまわないためです。

**記録の失敗では生成を止めません。** `Recorder` は保存の失敗を警告ログに留め、呼び出し側へは
伝えません。状態はあくまで観測のための記録であり、書けなかったことを理由に生成を中断するほうが
害が大きいためです（読めなかったときの扱いは上のとおり別です）。

**アプリ側の状態保存 port は `StatusStore[T]` の形に揃えておくことを勧めます。** 揃えておけば
`*jobstatus.Store[T]` がそのまま port の実装になり、利用側ごとに同じアダプタが増えるのを防げます。

---

## 📄 一覧の罠 (Listing)

**並べ替えのキーは引数で必ず選びます。** `SelectIDs` / `LoadPage` に既定を置いていないのは、間違った
既定のまま呼べてしまうからです。ここで誤っても一覧は正常に返り、順序だけが静かに崩れます。
`nil` は「ID そのものの降順」で、これが時系列と一致するのは ID の先頭が生成時刻で始まる場合に
限られます。用途ごとに異なるプレフィックスが混在したり採番の形式が変わったりすると、文字列比較は
時刻ではなくプレフィックス順になるため、通常は `go-utils/jobid` の `SortKey` を渡してください。

**`LoadPage` は `load` のエラーを返しません。** 失敗した ID は警告ログを残して一覧から取り除き、
`PageMeta` を実件数へ補正して、ページ全体は返します。1 件の破損や競合する削除でページを失わせない
ためです。ジョブ ID だけの行を残したい場合は `load` の中でフォールバック値を返してください。
ただし `ctx` のキャンセル・期限切れはエラーとして返ります——軒並み失敗した結果の「0 件」を、正常な
空一覧と取り違えさせないためです。

**`joblist.Collect` はバケット全体の走査になります。** 呼び出しは `cache.IDList` 越しに行ってください。
拾えるのはディレクトリ名だけなので、メタデータの保存前に落ちたジョブも ID としては現れます。
一覧に見せるか除くかは読み込み側の判断です。

**`cache.IDList` のキーには一覧の単位（走査対象のプレフィックスなど）だけを使ってください。**
こちらは期限切れエントリを回収しないため、ジョブ ID のように増え続ける値をキーにすると溜まります。
回収を行う `cache.TTL` のほうは生成と同時に回収を始めるので、使い終わったら `Close` を呼びます。

**ジョブ ID の正規化はライブラリ内部で必ず行われます。** ジョブ ID は URL パスとストレージパスの
双方に現れるため、検証はセキュリティ境界を兼ねます。ストレージパスの組み立てもキャッシュキーの
生成も `go-utils/jobid` を通すので、呼び出し側で `jobid.Sanitize` を通す必要はありません。

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
