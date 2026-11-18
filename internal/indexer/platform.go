package indexer

import (
	"fmt"
	"go/build"
	"path/filepath"
	"runtime"
	"strings"
)

// goPlatform decides which Go files belong to the selected build
// configuration. Go code often declares the same name once per operating
// system or build tag; indexing all of those files at once merges
// declarations that never coexist (a stub struct and the real interface of
// the same name), so by default only one configuration is indexed, as the
// go command and gopls do.
type goPlatform struct {
	all bool
	ctx build.Context
}

// newPlatform parses "GOOS/GOARCH", "all", or "" (the running platform).
func newPlatform(spec string) (goPlatform, error) {
	if spec == "all" {
		return goPlatform{all: true}, nil
	}
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH = runtime.GOOS, runtime.GOARCH
	ctx.CgoEnabled = true
	ctx.BuildTags, ctx.ToolTags, ctx.ReleaseTags = nil, build.Default.ToolTags, build.Default.ReleaseTags
	if spec != "" {
		goos, goarch, ok := strings.Cut(spec, "/")
		if !ok || goos == "" || goarch == "" {
			return goPlatform{}, fmt.Errorf("platform %q: want GOOS/GOARCH (such as linux/amd64) or all", spec)
		}
		ctx.GOOS, ctx.GOARCH = goos, goarch
	}
	return goPlatform{ctx: ctx}, nil
}

// match reports whether the Go file at path is part of the configuration.
// Unreadable files are kept: the parser reports them later.
func (p goPlatform) match(path string) bool {
	if p.all {
		return true
	}
	ok, err := p.ctx.MatchFile(filepath.Dir(path), filepath.Base(path))
	return ok || err != nil
}

// String names the configuration, as stored with the index.
func (p goPlatform) String() string {
	if p.all {
		return "all"
	}
	return p.ctx.GOOS + "/" + p.ctx.GOARCH
}
