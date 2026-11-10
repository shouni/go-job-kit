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

// newStore returns a Store over in-memory storage seeded with objects.
//
// This used to hold a hand-written remoteio.Lister that even copied the delimiter-based
// collapsing of pseudo-directories itself. That shape could drift from the real thing
// unnoticed, so it was replaced with the memio remoteio ships (it passes the same
// conformance suite as the real backends).
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

// Only pseudo-directory names are picked up as job IDs; direct objects and duplicates are dropped.
func TestCollectKeepsOnlyPseudoDirectories(t *testing.T) {
	t.Parallel()

	store := newStore(t,
		"jobs/job-a/status.json",
		"jobs/job-b/status.json",
		"jobs/job-b/result.mp4", // several artifacts of one job still give one ID
		"jobs/readme.txt",       // an object directly under the prefix is not a job
	)

	got, err := joblist.Collect(context.Background(), store, "gs://bucket/jobs")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if want := []string{"job-a", "job-b"}; !slices.Equal(got, want) {
		t.Errorf("Collect() = %v, want %v", got, want)
	}
}

// A trailing "/" is added to prefix. Without it a scan of "…/music" would pick up "…/music2/" too.
func TestCollectNormalizesPrefix(t *testing.T) {
	t.Parallel()

	store := newStore(t,
		"music/job-a/status.json",
		"music2/job-b/status.json", // an adjacent prefix is not picked up
	)

	got, err := joblist.Collect(context.Background(), store, "gs://bucket/music")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if want := []string{"job-a"}; !slices.Equal(got, want) {
		t.Errorf("Collect() = %v, want %v", got, want)
	}
}

// App-specific exclusions (such as a prefix for working jobs) can be plugged in with WithKeep.
// Several are combined with AND.
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

// WithValidIDsOnly keeps only IDs that pass jobid.Validate.
func TestCollectWithValidIDsOnly(t *testing.T) {
	t.Parallel()

	store := newStore(t,
		"jobs/comp-20260816-abcd/status.json",
		"jobs/-leading-hyphen/status.json", // an ID not starting with an alphanumeric is invalid
		"jobs/日本語/status.json",             // outside the allowed characters
	)

	got, err := joblist.Collect(context.Background(), store, "gs://bucket/jobs/", joblist.WithValidIDsOnly())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if want := []string{"comp-20260816-abcd"}; !slices.Equal(got, want) {
		t.Errorf("Collect() = %v, want %v", got, want)
	}
}

// A scan failure is returned in a form that tells which prefix failed.
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
		t.Errorf("error does not include the prefix: %v", err)
	}
}

// Configuration mistakes are returned before scanning.
func TestCollectRejectsMissingConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := joblist.Collect(context.Background(), nil, "gs://bucket/jobs/"); err == nil {
		t.Error("Collect(nil reader) error = nil, want an error")
	}
	if _, err := joblist.Collect(context.Background(), newStore(t), "  "); err == nil {
		t.Error("Collect(empty prefix) error = nil, want an error")
	}
}
