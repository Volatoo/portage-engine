package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	artifactstorage "github.com/slchris/portage-engine/internal/storage"
)

func (s *Server) objectBinhostHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requested := strings.TrimPrefix(r.URL.Path, "/binpkgs/")
		clean := path.Clean(strings.ReplaceAll(requested, "\\", "/"))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") ||
			strings.HasPrefix(clean, "/") || clean != requested {
			http.NotFound(w, r)
			return
		}
		profile, relative, ok := s.objectBinhostSelection(clean)
		if !ok {
			http.NotFound(w, r)
			return
		}
		pointer, manifest, err := s.loadObjectChannel(
			profile.BinhostPath, profile.Arch,
		)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		objectKey := ""
		expectedDigest := ""
		expectedSize := int64(-1)
		if relative == "Packages" {
			objectKey, err = artifactstorage.PublishedGenerationKey(
				profile.BinhostPath, profile.Arch,
				pointer.GenerationID, "Packages",
			)
			expectedDigest = pointer.PackagesSHA256
		} else {
			for _, artifact := range manifest.Artifacts {
				if artifact.RelativePath == relative {
					objectKey = artifact.ObjectKey
					expectedDigest = artifact.SHA256
					expectedSize = artifact.Size
					break
				}
			}
		}
		if err != nil || objectKey == "" {
			http.NotFound(w, r)
			return
		}
		s.servePublishedObject(
			w, r, objectKey, relative, expectedDigest, expectedSize,
		)
	})
}

func (s *Server) objectBinhostSelection(
	requested string,
) (binhostProfile, string, bool) {
	s.binpkgMu.RLock()
	defer s.binpkgMu.RUnlock()
	var selected binhostProfile
	selectedRelative := ""
	selectedPrefixLength := -1
	for _, profile := range s.binhostProfiles {
		prefix := profile.BinhostPath + "/"
		if strings.HasPrefix(requested, prefix) {
			relative := strings.TrimPrefix(requested, prefix)
			if relative != "" && len(prefix) > selectedPrefixLength {
				selected = profile
				selectedRelative = relative
				selectedPrefixLength = len(prefix)
			}
		}
	}
	return selected, selectedRelative, selectedPrefixLength >= 0
}

func (s *Server) loadObjectChannel(
	binhostPath, arch string,
) (artifactstorage.ChannelPointer, artifactstorage.GenerationManifest, error) {
	channelKey, err := artifactstorage.StableChannelKey(binhostPath, arch)
	if err != nil {
		return artifactstorage.ChannelPointer{}, artifactstorage.GenerationManifest{}, err
	}
	document, err := artifactstorage.DownloadBytes(
		s.artifactStorage, channelKey, s.objectReadScratch(), 1<<20,
	)
	if err != nil {
		return artifactstorage.ChannelPointer{}, artifactstorage.GenerationManifest{}, err
	}
	pointer, err := artifactstorage.ParseChannelPointer(document)
	if err != nil || pointer.BinhostPath != binhostPath ||
		pointer.Architecture != arch {
		return artifactstorage.ChannelPointer{}, artifactstorage.GenerationManifest{},
			fmt.Errorf("invalid stable channel pointer")
	}
	manifestDocument, err := artifactstorage.DownloadBytes(
		s.artifactStorage, pointer.ManifestKey, s.objectReadScratch(), 4<<20,
	)
	if err != nil || digestDocument(manifestDocument) != pointer.ManifestSHA256 {
		return artifactstorage.ChannelPointer{}, artifactstorage.GenerationManifest{},
			fmt.Errorf("invalid generation manifest digest")
	}
	manifest, err := artifactstorage.ParseGenerationManifest(manifestDocument)
	if err != nil || manifest.GenerationID != pointer.GenerationID ||
		manifest.BinhostPath != binhostPath ||
		manifest.PackagesSHA256 != pointer.PackagesSHA256 {
		return artifactstorage.ChannelPointer{}, artifactstorage.GenerationManifest{},
			fmt.Errorf("invalid generation manifest identity")
	}
	return pointer, manifest, nil
}

func (s *Server) servePublishedObject(
	w http.ResponseWriter,
	r *http.Request,
	objectKey, relative, expectedDigest string,
	expectedSize int64,
) {
	w.Header().Set("ETag", `"`+expectedDigest+`"`)
	if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match == `"`+expectedDigest+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	scratch, err := os.MkdirTemp(s.objectReadScratch(), "binhost-read-*")
	if err != nil {
		http.Error(w, "Binhost unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	local := filepath.Join(scratch, filepath.Base(relative))
	if err := s.artifactStorage.Download(objectKey, local); err != nil {
		http.NotFound(w, r)
		return
	}
	digest, size, err := digestPublishedFile(local)
	if err != nil || digest != expectedDigest ||
		(expectedSize >= 0 && size != expectedSize) {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(local) // #nosec G304 -- server-owned scratch path.
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if relative == "Packages" {
		w.Header().Set("Cache-Control", "public, max-age=30, must-revalidate")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	http.ServeContent(w, r, filepath.Base(relative), info.ModTime(), file)
}

func (s *Server) objectReadScratch() string {
	base := strings.TrimSpace(s.config.DataDir)
	if base == "" {
		base = os.TempDir()
	}
	scratch := filepath.Join(base, "binhost-read-cache")
	_ = os.MkdirAll(scratch, 0o750)
	return scratch
}

func digestDocument(document []byte) string {
	digest := sha256.Sum256(document)
	return hex.EncodeToString(digest[:])
}

func digestPublishedFile(name string) (string, int64, error) {
	file, err := os.Open(name) // #nosec G304 -- caller supplies server-owned scratch.
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("published object is not a regular file")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
