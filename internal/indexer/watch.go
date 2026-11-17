package indexer

import (
	"context"
	"fmt"
	"hash/fnv"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// fingerprint summarises the indexable files under root by path, size and
// modification time. It is cheap enough to poll every couple of seconds
// and needs no platform-specific file-system events.
func fingerprint(root string, opts Options) (uint64, error) {
	h := fnv.New64a()
	exclude := excludeSet(opts.Exclude)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // files can disappear mid-walk while editing
		}
		if d.IsDir() {
			if p != root && skipDir(p, d.Name(), exclude) {
				return filepath.SkipDir
			}
			return nil
		}
		if languageFor(p) == nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		fmt.Fprintf(h, "%s|%d|%d\n", p, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return h.Sum64(), err
}

// Watch keeps the index up to date until ctx is cancelled. It first runs
// one incremental Reindex to catch up on edits made while nothing was
// watching, then polls the tree every interval and re-indexes when anything
// changed. report is called after each re-index that changed the index or
// failed.
//
// The baseline fingerprint is taken before the catch-up run, so a file
// saved while that run is in progress still differs from the baseline and
// is picked up on the next tick rather than missed.
func Watch(ctx context.Context, root string, st *store.Store, opts Options, interval time.Duration, report func(Result, error)) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	last, err := fingerprint(root, opts)
	if err != nil {
		return err
	}
	res, err := Reindex(root, st, opts)
	if report != nil && (err != nil || res.Files > 0 || res.Removed > 0) {
		report(res, err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		fp, err := fingerprint(root, opts)
		if err != nil || fp == last {
			continue
		}
		last = fp
		res, err := Reindex(root, st, opts)
		if report != nil {
			report(res, err)
		}
	}
}
