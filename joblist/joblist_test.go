package joblist_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/shouni/go-remote-io/remoteio"

	"github.com/shouni/go-job-kit/joblist"
)

// fakeLister は remoteio.Lister のインメモリ実装です。
// 区切り文字付きの走査が返す形（疑似ディレクトリは "/" 終わり）で paths を流します。
type fakeLister struct {
	paths []string
	err   error
	// gotPrefix は List が受け取ったプレフィックスの記録です。
	gotPrefix string
	// gotOptCount は List が受け取ったオプション数の記録です（区切り文字の指定を数える）。
	gotOptCount int
}

func (f *fakeLister) List(_ context.Context, prefix string, callback func(path string) error, opts ...remoteio.ListOption) error {
	f.gotPrefix = prefix
	f.gotOptCount = len(opts)
	if f.err != nil {
		return f.err
	}
	for _, p := range f.paths {
		if err := callback(p); err != nil {
			return err
		}
	}
	return nil
}

// 疑似ディレクトリ名だけをジョブ ID として拾い、直下のオブジェクトと重複は除くこと。
func TestCollectKeepsOnlyPseudoDirectories(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{paths: []string{
		"gs://bucket/jobs/job-b/",
		"gs://bucket/jobs/readme.txt", // 直下のオブジェクトはジョブではない
		"gs://bucket/jobs/job-a/",
		"gs://bucket/jobs/job-b/", // 重複は 1 件に潰す
	}}

	got, err := joblist.Collect(context.Background(), lister, "gs://bucket/jobs")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if want := []string{"job-b", "job-a"}; !slices.Equal(got, want) {
		t.Errorf("Collect() = %v, want %v", got, want)
	}
}

// prefix の末尾に "/" を補うこと。補わないと "…/music" の走査が "…/music2/" まで拾う。
// 区切り文字の指定（オプション 1 つ）で走査していることも確かめる。
func TestCollectNormalizesPrefix(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{}
	if _, err := joblist.Collect(context.Background(), lister, "gs://bucket/music"); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if lister.gotPrefix != "gs://bucket/music/" {
		t.Errorf("List prefix = %q, want %q", lister.gotPrefix, "gs://bucket/music/")
	}
	if lister.gotOptCount != 1 {
		t.Errorf("List options = %d, want 1（区切り文字の指定）", lister.gotOptCount)
	}
}

// アプリ固有の除外（作業用ジョブの接頭辞など）を WithKeep で差し込めること。
// 複数指定は AND になること。
func TestCollectWithKeep(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{paths: []string{
		"gs://bucket/jobs/mv-100/",
		"gs://bucket/jobs/regen-keyframe-200/",
		"gs://bucket/jobs/short-300/",
	}}

	got, err := joblist.Collect(context.Background(), lister, "gs://bucket/jobs/",
		joblist.WithKeep(func(id string) bool { return !strings.HasPrefix(id, "regen-keyframe-") }),
		joblist.WithKeep(func(id string) bool { return id != "short-300" }),
	)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if want := []string{"mv-100"}; !slices.Equal(got, want) {
		t.Errorf("Collect() = %v, want %v", got, want)
	}
}

// WithValidIDsOnly は jobid.Validate を通る ID だけを残すこと。
func TestCollectWithValidIDsOnly(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{paths: []string{
		"gs://bucket/jobs/comp-20260816-abcd/",
		"gs://bucket/jobs/-leading-hyphen/", // 先頭が英数字でない ID は不正
		"gs://bucket/jobs/日本語/",             // 使用可能な文字の外
	}}

	got, err := joblist.Collect(context.Background(), lister, "gs://bucket/jobs/", joblist.WithValidIDsOnly())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if want := []string{"comp-20260816-abcd"}; !slices.Equal(got, want) {
		t.Errorf("Collect() = %v, want %v", got, want)
	}
}

// 走査の失敗は、どのプレフィックスで失敗したか分かる形で返すこと。
func TestCollectWrapsListError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("list failed")
	lister := &fakeLister{err: wantErr}

	_, err := joblist.Collect(context.Background(), lister, "gs://bucket/jobs/")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Collect() error = %v, want wrapping %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "gs://bucket/jobs/") {
		t.Errorf("エラーにプレフィックスが含まれていない: %v", err)
	}
}

// 構成の誤りは走査の前に返すこと。
func TestCollectRejectsMissingConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := joblist.Collect(context.Background(), nil, "gs://bucket/jobs/"); err == nil {
		t.Error("Collect(nil reader) error = nil, want an error")
	}
	if _, err := joblist.Collect(context.Background(), &fakeLister{}, "  "); err == nil {
		t.Error("Collect(empty prefix) error = nil, want an error")
	}
}
