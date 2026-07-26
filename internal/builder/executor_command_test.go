package builder

import "testing"

func TestConstructEmergeCommandUsesExactVersionAtom(t *testing.T) {
	executor := NewBuildExecutor(t.TempDir(), t.TempDir())

	command := executor.constructEmergeCommand(PackageSpec{
		Atom:    "app-misc/screenfetch",
		Version: "3.9.9",
	}, &ConfigBundle{}, t.TempDir())

	if got, want := command[len(command)-1], "=app-misc/screenfetch-3.9.9"; got != want {
		t.Fatalf("versioned emerge atom = %q, want %q", got, want)
	}
}

func TestConstructEmergeCommandLeavesUnversionedAtomUnchanged(t *testing.T) {
	executor := NewBuildExecutor(t.TempDir(), t.TempDir())

	command := executor.constructEmergeCommand(PackageSpec{
		Atom: "app-misc/screenfetch",
	}, &ConfigBundle{}, t.TempDir())

	if got, want := command[len(command)-1], "app-misc/screenfetch"; got != want {
		t.Fatalf("unversioned emerge atom = %q, want %q", got, want)
	}
}
