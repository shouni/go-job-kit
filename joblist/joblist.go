// Package joblist は、ストレージの疑似ディレクトリ走査からジョブ ID の一覧を集めます。
//
// 「ジョブ 1 件 = プレフィックス直下のディレクトリ 1 つ」という配置は移植元の
// アプリ全てで共通で、その走査コード（区切り文字を指定して 1 ジョブを 1 エントリで
// 受け取り、疑似ディレクトリ名だけを拾う）も同じ形で 3 回書かれていました。
// その形をここに集めます。
//
// バケット全体の走査になるため、呼び出しは cache.IDList 越しに行うことを勧めます。
// 集めた ID の並べ替えとページ切り出しは paging が担います。
package joblist

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/shouni/go-remote-io/remoteio"
	"github.com/shouni/go-utils/jobid"
)

type options struct {
	keeps []func(jobID string) bool
}

// Option は Collect の挙動を変更します。
type Option func(*options)

// WithKeep は、集めるジョブ ID を絞り込みます。複数指定した場合は、すべてを
// 満たす ID だけが残ります（AND）。作業用ジョブの接頭辞を除くといった、
// アプリ固有の除外に使ってください。
func WithKeep(keep func(jobID string) bool) Option {
	return func(o *options) {
		if keep != nil {
			o.keeps = append(o.keeps, keep)
		}
	}
}

// WithValidIDsOnly は、jobid.Validate を通る ID だけを集めます。
//
// 既定で検証しないのは、ディレクトリ名が ID の形式を満たさないことが直ちに
// 異常とは限らないためです（現行の採番より前に作られたジョブなど）。一覧から
// 黙って消すか、読み込み側でフォールバック行として見せるかはアプリの判断なので、
// 消してよい場合にだけ指定してください。
func WithValidIDsOnly() Option {
	return WithKeep(jobid.IsValid)
}

// Collect は、prefix 直下の疑似ディレクトリ名をジョブ ID として集めます。
//
// 区切り文字 "/" を指定して走査するため、ジョブ 1 件を 1 エントリとして
// 受け取ります。指定しないと配下の成果物が全件返り、1 ジョブにつき成果物の
// 数だけ結果を受け取ったうえで、呼び出し側で重複を潰すことになります。
// 同じ走査をサーバー側へ寄せています。
//
//   - prefix 直下に直接置かれたオブジェクト（"/" で終わらないパス）はジョブでは
//     ないため対象外です
//   - 拾えるのはディレクトリ名だけなので、メタデータの保存前に落ちたジョブも
//     ID としては現れます。一覧に見せるかどうかは読み込み側で決めてください
//   - 返す並び順はストレージの列挙順のままです。「新しい順」への並べ替えは
//     paging.SelectIDs / paging.LoadPage が担います
//
// prefix の末尾に "/" が無ければ補います。補わないと "…/music" の走査が
// "…/music2/" 配下まで拾ってしまいます。
func Collect(ctx context.Context, reader remoteio.Lister, prefix string, opts ...Option) ([]string, error) {
	if reader == nil {
		return nil, errors.New("joblist: reader is not configured")
	}
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return nil, errors.New("joblist: prefix is empty")
	}
	prefix += "/"

	cfg := options{}
	for _, opt := range opts {
		opt(&cfg)
	}

	seen := map[string]bool{}
	var jobIDs []string
	err := reader.List(ctx, prefix, func(objectPath string) error {
		// 疑似ディレクトリだけを拾います。プレフィックス直下に置かれた
		// オブジェクトはジョブではないため対象外です。
		if !strings.HasSuffix(objectPath, "/") {
			return nil
		}
		id := path.Base(strings.TrimSuffix(objectPath, "/"))
		if id == "" || id == "." || id == "/" || seen[id] {
			return nil
		}
		for _, keep := range cfg.keeps {
			if !keep(id) {
				return nil
			}
		}
		seen[id] = true
		jobIDs = append(jobIDs, id)
		return nil
	}, remoteio.WithDelimiter("/"))
	if err != nil {
		return nil, fmt.Errorf("joblist: list (%s): %w", prefix, err)
	}
	return jobIDs, nil
}
