// Command artifact-lifecycle audits, replicates, or garbage-collects one
// published binhost channel. It has no listener and should run as a scheduled,
// separately credentialed production job.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	artifactstorage "github.com/slchris/portage-engine/internal/storage"
)

var (
	operation   = flag.String("operation", "audit", "audit, replicate, or gc")
	binhostPath = flag.String("binhost-path", "", "catalog-owned binhost path")
	arch        = flag.String("arch", "amd64", "binhost architecture")
	retention   = flag.Duration("retention", 168*time.Hour, "GC retention window")
	scratchDir  = flag.String("scratch-dir", "", "bounded local scratch directory")
)

func main() {
	flag.Parse()
	source, err := storageFromEnvironment("", *operation == "gc")
	if err != nil {
		log.Fatal(err)
	}
	switch strings.ToLower(strings.TrimSpace(*operation)) {
	case "audit":
		report, err := artifactstorage.AuditPublishedChannel(
			source, *binhostPath, *arch, *scratchDir,
		)
		finish(report, err)
	case "replicate":
		destinationStore, err := storageFromEnvironment("REPLICA_", false)
		if err != nil {
			log.Fatal(err)
		}
		destination, ok := destinationStore.(artifactstorage.VersionedStorage)
		if !ok {
			log.Fatal("replication destination must support versioned CAS")
		}
		report, err := artifactstorage.ReplicatePublishedChannel(
			source, destination, *binhostPath, *arch, *scratchDir,
		)
		finish(report, err)
	case "gc":
		deleted, err := artifactstorage.GarbageCollectPublishedGenerations(
			source, *binhostPath, *arch, *retention, time.Now().UTC(),
			*scratchDir,
		)
		finish(map[string]any{
			"binhost_path":        *binhostPath,
			"deleted_generations": deleted,
		}, err)
	default:
		log.Fatal("-operation must be audit, replicate, or gc")
	}
}

func storageFromEnvironment(
	prefix string,
	allowDelete bool,
) (artifactstorage.Storage, error) {
	read := func(name string) string {
		return strings.TrimSpace(os.Getenv(prefix + name))
	}
	if read("STORAGE_S3_BUCKET") == "" || read("STORAGE_S3_REGION") == "" {
		return nil, fmt.Errorf("%sSTORAGE_S3_BUCKET and %sSTORAGE_S3_REGION are required",
			prefix, prefix)
	}
	usePathStyle, err := strconv.ParseBool(firstNonEmpty(
		read("STORAGE_S3_USE_PATH_STYLE"), "false",
	))
	if err != nil {
		return nil, fmt.Errorf("%sSTORAGE_S3_USE_PATH_STYLE: %w", prefix, err)
	}
	deleteEnabled := false
	if configured := read("STORAGE_S3_ALLOW_DELETE"); configured != "" {
		enabled, parseErr := strconv.ParseBool(configured)
		if parseErr != nil {
			return nil, parseErr
		}
		deleteEnabled = enabled
	}
	allowDelete = allowDelete && deleteEnabled
	if strings.EqualFold(*operation, "gc") && prefix == "" && !allowDelete {
		return nil, fmt.Errorf(
			"GC requires STORAGE_S3_ALLOW_DELETE=true and a retention-scoped lifecycle role",
		)
	}
	return artifactstorage.NewStorage(&artifactstorage.Config{
		Type: "s3", S3Bucket: read("STORAGE_S3_BUCKET"),
		S3Region:       read("STORAGE_S3_REGION"),
		S3Prefix:       read("STORAGE_S3_PREFIX"),
		S3Endpoint:     read("STORAGE_S3_ENDPOINT"),
		S3UsePathStyle: usePathStyle,
		S3AllowDelete:  allowDelete,
	})
}

func finish(value any, err error) {
	if err != nil {
		log.Fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		log.Fatal(err)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
