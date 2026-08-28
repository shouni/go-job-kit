package joblist_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/shouni/go-remote-io/remoteio"
	"github.com/shouni/go-remote-io/remoteio/memio"

	"github.com/shouni/go-job-kit/joblist"
)

const testBucket = "bucket"

// newStore は、インメモリのストレージへ objects を流し込んだ Store を返します。
//
// 以前はここに remoteio.Lister の手書き実装があり、区切り文字による疑似ディレクトリの
// 畳み込みまで自前で写していました。本物とずれても気づけない形だったので、
// remoteio が配っている memio に置き換えています（本物と同じ適合性スイートを通ります）。
func newStore(t *testing.T, objects ...string) remoteio.Store {
	t.Helper()

	h := memio.New(memio.WithScheme(remoteio.SchemeGCS))
	for _, name := range objects {
		if err := h.Seed(remoteio.BuildURI(remoteio.SchemeGCS, testBucket, name), []byte("x")); err != nil {
			t.Fatalf("Seed(%s) error = %v", name, err)
		}
	}
	return remoteio.NewStore(h)
}

// 疑似ディレクトリ名だけをジョブ ID として拾い、直下のオブジェクトと重複は除くこと。
func TestCollectKeepsOnlyPseudoDirectories(t *testing.T) {
	t.Parallel()

	store := newStore(t,
		"jobs/job-a/status.json",
		"jobs/job-b/status.json",
		"jobs/job-b/result.mp4", // 同じジョブの成果物が複数あっても ID は 1 件
		"jobs/readme.txt",       // 直下のオブジェクトはジョブではない
	)

	got, err := joblist.Collect(context.Background(), store, "gs://bucket/jobs")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if want := []string{"job-a", "job-b"}; !slices.Equal(got, want) {
		t.Errorf("Collect() = %v, want %v", got, want)
	}
}

// prefix の末尾に "/" を補うこと。補わないと "…/music" の走査が "…/music2/" まで拾う。
func TestCollectNormalizesPrefix(t *testing.T) {
	t.Parallel()

	store := newStore(t,
		"music/job-a/status.json",
		"music2/job-b/status.json", // 隣接するプレフィックスは拾わない
	)

	got, err := joblist.Collect(context.Background(), store, "gs://bucket/music")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if want := []string{"job-a"}; !slices.Equal(got, want) {
		t.Errorf("Collect() = %v, want %v", got, want)
	}
}

// アプリ固有の除外（作業用ジョブの接頭辞など）を WithKeep で差し込めること。
// 複数指定は AND になること。
func TestCollectWithKeep(t *testing.T) {
	t.Parallel()

	store := newStore(t,
		"jobs/mv-100/status.json",
		"jobs/regen-keyframe-200/status.json",
		"jobs/short-300/status.json",
	)

	got, err := joblist.Collect(context.Background(), store, "gs://bucket/jobs/",
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

	store := newStore(t,
		"jobs/comp-20260816-abcd/status.json",
		"jobs/-leading-hyphen/status.json", // 先頭が英数字でない ID は不正
		"jobs/日本語/status.json",             // 使用可能な文字の外
	)

	got, err := joblist.Collect(context.Background(), store, "gs://bucket/jobs/", joblist.WithValidIDsOnly())
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
	h := memio.New(
		memio.WithScheme(remoteio.SchemeGCS),
		memio.WithFailure(func(op, _ string) error {
			if op == "list" {
				return wantErr
			}
			return nil
		}),
	)

	_, err := joblist.Collect(context.Background(), remoteio.NewStore(h), "gs://bucket/jobs/")
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
	if _, err := joblist.Collect(context.Background(), newStore(t), "  "); err == nil {
		t.Error("Collect(empty prefix) error = nil, want an error")
	}
}
