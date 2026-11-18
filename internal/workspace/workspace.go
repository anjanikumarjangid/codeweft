// Package workspace reads a workspace file: the repositories codewiki serves
// together, so that tools can answer across services.
//
// A workspace file is JSON:
//
//	{
//	  "repos": [
//	    {"name": "orders", "path": "../orders-service"},
//	    {"path": "../inventory"}
//	  ]
//	}
//
// Paths are relative to the file. A repository's name defaults to the base
// name of its directory; it labels results and selects a repository in
// tools. Each repository keeps its own index at PATH/.codewiki/index.db.
package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Repo is one repository of a workspace.
type Repo struct {
	Name string `json:"name"`
	Path string `json:"path"` // absolute after Load
}

type file struct {
	Repos []Repo `json:"repos"`
}

// Load reads a workspace file and resolves its repositories' paths.
func Load(path string) ([]Repo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("workspace %s: %w", path, err)
	}
	if len(f.Repos) == 0 {
		return nil, fmt.Errorf("workspace %s lists no repositories", path)
	}
	base := filepath.Dir(path)
	seen := map[string]bool{}
	for i := range f.Repos {
		r := &f.Repos[i]
		if r.Path == "" {
			return nil, fmt.Errorf("workspace %s: repository %d has no path", path, i+1)
		}
		if !filepath.IsAbs(r.Path) {
			r.Path = filepath.Join(base, r.Path)
		}
		if r.Path, err = filepath.Abs(r.Path); err != nil {
			return nil, err
		}
		if r.Name == "" {
			r.Name = filepath.Base(r.Path)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("workspace %s: two repositories are named %q; give them names", path, r.Name)
		}
		seen[r.Name] = true
	}
	return f.Repos, nil
}
