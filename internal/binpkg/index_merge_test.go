package binpkg

import (
	"strings"
	"testing"
	"time"
)

func TestMergePackagesIndexesPreservesPreviousAndOverlaysPath(t *testing.T) {
	previous := []byte(`ARCH: amd64
TIMESTAMP: 1
VERSION: 0
PACKAGES: 2

CPV: app-misc/jq-1.7
SIZE: 10
PATH: app-misc/jq/jq-1.7.gpkg.tar

CPV: sys-apps/coreutils-9.5
SIZE: 20
PATH: sys-apps/coreutils/coreutils-9.5.gpkg.tar
`)
	current := []byte(`ARCH: amd64
TIMESTAMP: 2
VERSION: 0
PACKAGES: 2

CPV: app-misc/jq-1.7
BUILD_ID: 2
SIZE: 11
PATH: app-misc/jq/jq-1.7.gpkg.tar

CPV: app-editors/vim-9.1
SIZE: 30
PATH: app-editors/vim/vim-9.1.gpkg.tar
`)
	now := time.Unix(1234, 0).UTC()
	merged, err := MergePackagesIndexes(previous, current, "amd64", now)
	if err != nil {
		t.Fatal(err)
	}
	text := string(merged)
	if !strings.Contains(text, "PACKAGES: 3") ||
		!strings.Contains(text, "TIMESTAMP: 1234") ||
		strings.Count(text, "PATH: app-misc/jq/jq-1.7.gpkg.tar") != 1 ||
		!strings.Contains(text, "BUILD_ID: 2") ||
		!strings.Contains(text, "CPV: sys-apps/coreutils-9.5") {
		t.Fatalf("unexpected merged Packages:\n%s", text)
	}
	store := NewStore(t.TempDir())
	count, err := store.LoadPackagesIndex(merged, "amd64")
	if err != nil || count != 3 || len(store.Snapshot()) != 3 {
		t.Fatalf("loaded count=%d snapshot=%d err=%v",
			count, len(store.Snapshot()), err)
	}
}

func TestMergePackagesIndexesRejectsEscapingPath(t *testing.T) {
	document := []byte(`VERSION: 0
PACKAGES: 1

CPV: app-misc/jq-1.7
SIZE: 10
PATH: ../escape.gpkg.tar
`)
	if _, err := MergePackagesIndexes(
		nil, document, "amd64", time.Now(),
	); err == nil {
		t.Fatal("escaping Packages PATH was accepted")
	}
}
