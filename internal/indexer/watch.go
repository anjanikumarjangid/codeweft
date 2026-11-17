package indexer

import (
	"context"
	"fmt"
	"hash/fnv"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// fingerprint summarises the indexable files under root by path, size and
// modification time. It is cheap enough to poll every couple of seconds
// and needs no platform-specific file-system events.
func fingerprint(root string, opts Options) (uint64, error) {
	h := fnv.New64a()
	skip := map[string]bool{}
	for k := range defaultSkipDirs {
		skip[k] = true
	}
	for _, d := range opts.Exclude {
		skip[strings.TrimSpace(d)] = true
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // files can disappear mid-walk while editing
		}
		if d.IsDir() {
			if p != root && (skip[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
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

// Watch keeps the index up to date until ctx is cancelled: it polls the
// tree every interval and runs an incremental Reindex when anything
// changed. report is called after each re-index.
func Watch(ctx context.Context, root string, st *store.Store, opts Options, interval time.Duration, report func(Result, error)) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	last, err := fingerprint(root, opts)
	if err != nil {
		return err
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
