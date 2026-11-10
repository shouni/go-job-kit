// Package joblist collects a list of job IDs by scanning storage pseudo-directories.
//
// The layout "one job = one directory directly under a prefix" is shared by every app this
// was ported from, and the scanning code (list with a delimiter so each job arrives as one
// entry, then keep only the pseudo-directory names) was written in the same shape three
// times. This package holds that shape.
//
// It scans a whole bucket prefix, so calling it through cache.IDList is recommended.
// Sorting the collected IDs and cutting pages is paging's job.
package joblist

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"path"
	"strings"

	"github.com/shouni/go-remote-io/remoteio"
	"github.com/shouni/go-utils/jobid"
)

// Lister is just the listing capability Collect needs.
// remoteio.Store satisfies it as is.
//
// The composite interface from remoteio is not taken directly because this package is a
// hub referenced by five apps, and demanding more operations than needed would make the
// callers' tests provide implementations they never use.
type Lister interface {
	List(ctx context.Context, name string, opts ...remoteio.ListOption) iter.Seq2[remoteio.Entry, error]
}

type options struct {
	keeps []func(jobID string) bool
}

// Option changes how Collect behaves.
type Option func(*options)

// WithKeep narrows the collected job IDs. With several, only IDs that satisfy all of
// them remain (AND). Use it for app-specific exclusions such as dropping a prefix used
// for working jobs.
func WithKeep(keep func(jobID string) bool) Option {
	return func(o *options) {
		if keep != nil {
			o.keeps = append(o.keeps, keep)
		}
	}
}

// WithValidIDsOnly collects only IDs that pass jobid.Validate.
//
// Validation is off by default because a directory name that does not match the ID format
// is not necessarily an anomaly (jobs created before the current ID scheme, for example).
// Whether to silently drop it from the list or show it as a fallback row on the read side is
// the app's decision, so specify this only when dropping is acceptable.
func WithValidIDsOnly() Option {
	return WithKeep(jobid.IsValid)
}

// Collect collects the pseudo-directory names directly under prefix as job IDs.
//
// It scans with the delimiter "/", so each job arrives as one entry. Without it every
// artifact underneath would be returned, one result per artifact of each job, and the caller
// would have to deduplicate. This pushes that same scan to the server side.
//
//   - Objects placed directly under prefix (paths not ending in "/") are not jobs and are
//     skipped
//   - Only directory names are picked up, so a job that failed before saving its metadata
//     still appears as an ID. The read side decides whether to show it in the list
//   - The order returned is the storage's enumeration order. Sorting newest first is
//     paging.SelectIDs / paging.LoadPage's job
//
// A missing trailing "/" on prefix is added. Without it a scan of "…/music" would also
// pick up entries under "…/music2/".
func Collect(ctx context.Context, reader Lister, prefix string, opts ...Option) ([]string, error) {
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
	for entry, err := range reader.List(ctx, prefix, remoteio.WithDelimiter("/")) {
		if err != nil {
			return nil, fmt.Errorf("joblist: list (%s): %w", prefix, err)
		}

		// Keep only pseudo-directories. Objects placed directly under the prefix are
		// not jobs and are skipped.
		//
		// This used to be decided by whether the name ended in "/". The storage knew
		// "this is a collapsed level", but that was flattened into a string and the
		// caller had to guess it again.
		if !entry.IsPrefix {
			continue
		}
		id := path.Base(strings.TrimSuffix(entry.Name, "/"))
		if id == "" || id == "." || id == "/" || seen[id] {
			continue
		}
		if !keepAll(cfg.keeps, id) {
			continue
		}
		seen[id] = true
		jobIDs = append(jobIDs, id)
	}
	return jobIDs, nil
}

// keepAll reports whether id satisfies every registered filter.
func keepAll(keeps []func(string) bool, id string) bool {
	for _, keep := range keeps {
		if !keep(id) {
			return false
		}
	}
	return true
}
