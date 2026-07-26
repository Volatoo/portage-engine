package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/persistence"
)

func (s *Server) reconcileArtifacts() error {
	if s.jobLedger == nil {
		return nil
	}
	root := filepath.Clean(s.binpkgRoot)
	var inventory []persistence.ArtifactInventory
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		name := strings.ToLower(entry.Name())
		if !strings.HasSuffix(name, ".gpkg.tar") && !strings.HasSuffix(name, ".tbz2") {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		inventory = append(inventory, persistence.ArtifactInventory{
			Location: "/binpkgs/" + filepath.ToSlash(rel),
			Digest:   hex.EncodeToString(hash.Sum(nil)), SizeBytes: size,
		})
		return nil
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	report, err := s.jobLedger.ReconcileArtifacts(ctx, inventory)
	cancel()
	if err == nil && (report.Missing > 0 || report.Corrupt > 0 || report.Orphaned > 0) {
		// The low-cardinality status endpoint exposes exact counts. Avoid
		// logging paths/digests here because they may reveal package policy.
		log.Printf("Artifact reconciliation: matched=%d missing=%d corrupt=%d orphaned=%d",
			report.Matched, report.Missing, report.Corrupt, report.Orphaned)
	}
	return err
}
