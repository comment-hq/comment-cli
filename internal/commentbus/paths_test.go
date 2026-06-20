package commentbus

import (
	"os"
	"testing"
)

func TestResolvePathsAndEnsureBaseDirs(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if paths.Socket == "" || paths.History == "" || paths.OwnerCapability == "" {
		t.Fatalf("missing resolved paths: %+v", paths)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.Bus)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("bus dir mode = %o, want 0700", got)
	}
}
