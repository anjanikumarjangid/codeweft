package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, src string) string {
	t.Helper()
	p := filepath.Join(dir, "ws.json")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	repos, err := Load(write(t, dir, `{"repos": [{"name": "orders", "path": "../orders-service"}, {"path": "inventory"}, {"path": "/srv/web"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Repo{
		{Name: "orders", Path: filepath.Join(filepath.Dir(dir), "orders-service")},
		{Name: "inventory", Path: filepath.Join(dir, "inventory")},
		{Name: "web", Path: "/srv/web"},
	}
	if len(repos) != len(want) {
		t.Fatalf("got %+v", repos)
	}
	for i := range want {
		if repos[i] != want[i] {
			t.Errorf("repo %d: got %+v, want %+v", i, repos[i], want[i])
		}
	}
}

func TestLoadErrors(t *testing.T) {
	for src, want := range map[string]string{
		`{"repos": []}`:                                     "lists no repositories",
		`{"repos": [{"name": "a"}]}`:                        "has no path",
		`{"repos": [{"path": "x/api"}, {"path": "y/api"}]}`: `two repositories are named "api"`,
		`{"repos": `:                                        "unexpected end",
	} {
		_, err := Load(write(t, t.TempDir(), src))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got error %v, want %q", src, err, want)
		}
	}
}
