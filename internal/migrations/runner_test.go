package migrations

import (
	"testing"

	"github.com/slchris/portage-engine/internal/persistence"
)

func TestSupportedSchemaMatchesBinaryAndEmbeddedMigrations(t *testing.T) {
	t.Parallel()

	support, err := SupportedSchema()
	if err != nil {
		t.Fatalf("SupportedSchema() error = %v", err)
	}
	if support.Min != persistence.MinSchemaVersion {
		t.Fatalf("minimum = %d, want %d", support.Min, persistence.MinSchemaVersion)
	}
	if support.Max != persistence.MaxSchemaVersion {
		t.Fatalf("maximum = %d, want %d", support.Max, persistence.MaxSchemaVersion)
	}
	if support.EmbeddedLatest != support.Max {
		t.Fatalf("latest embedded migration = %d, binary maximum = %d", support.EmbeddedLatest, support.Max)
	}
}
