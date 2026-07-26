package builder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path"
	"strings"
	"time"

	"github.com/slchris/portage-engine/pkg/config"
)

// mirror.go pushes freshly built binpkgs to an internal mirror's artifact API
// (session-cookie login + multipart upload; files are served back under
// <base>/local/<dir>/…, which then acts as a LAN binhost for verification and
// for clients).

type mirrorUploader struct {
	base   string // e.g. http://10.31.0.2
	user   string
	pass   string
	dir    string // artifact directory on the mirror, e.g. "portage-engine"
	client *http.Client
}

// newMirrorUploader returns nil when no upload target is configured.
func newMirrorUploader(cs *config.CloudSettings) *mirrorUploader {
	if cs == nil || cs.UploadURL == "" {
		return nil
	}
	jar, _ := cookiejar.New(nil)
	dir := strings.Trim(cs.UploadDir, "/")
	if dir == "" {
		dir = "portage-engine"
	}
	return &mirrorUploader{
		base: strings.TrimRight(cs.UploadURL, "/"),
		user: cs.UploadUser,
		pass: cs.UploadPassword,
		dir:  dir,
		client: &http.Client{
			Timeout: 5 * time.Minute,
			Jar:     jar,
			// The upload API is a credential-bearing control path. Refuse all
			// redirects so a 307/308 cannot replay the login body or upload to
			// an unreviewed origin, especially in trusted-LAN HTTP deployments.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// binhostURL is the public base URL of the uploaded package tree.
func (u *mirrorUploader) binhostURL() string {
	return u.base + "/local/" + u.dir
}

// login establishes the session cookie the artifact API requires.
func (u *mirrorUploader) login() error {
	body, _ := json.Marshal(map[string]string{"username": u.user, "password": u.pass})
	resp, err := u.client.Post(u.base+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mirror login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !is2xx(resp.StatusCode) {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("mirror login failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// is2xx reports whether the status is any 2xx success (the mirror returns 201
// Created for uploads, not 200).
func is2xx(code int) bool { return code >= 200 && code < 300 }

// uploadLocalFile uploads one file into <dir>/<subdir>/ (subdir "" or "." for
// the tree root) and returns the mirror's public URL for it.
func (u *mirrorUploader) uploadLocalFile(localPath, subdir string) (string, error) {
	f, err := os.Open(localPath) // #nosec G304 -- path comes from the server's own binhost dir.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return u.uploadStream(path.Base(strings.ReplaceAll(localPath, "\\", "/")), f, subdir)
}

// uploadBytes uploads in-memory content (e.g. the armored signing pubkey).
func (u *mirrorUploader) uploadBytes(name string, data []byte, subdir string) (string, error) {
	return u.uploadStream(name, bytes.NewReader(data), subdir)
}

func (u *mirrorUploader) uploadStream(name string, r io.Reader, subdir string) (string, error) {
	dir := u.dir
	if subdir != "" && subdir != "." {
		dir += "/" + strings.Trim(subdir, "/")
	}

	// Stream the multipart body so large packages never sit in memory whole.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fw, err := mw.CreateFormFile("file", name)
		if err == nil {
			_, err = io.Copy(fw, r)
		}
		if err == nil {
			err = mw.WriteField("directory", dir)
		}
		if err == nil {
			err = mw.WriteField("overwrite", "true")
		}
		if err == nil {
			err = mw.Close()
		}
		_ = pw.CloseWithError(err)
	}()

	req, err := http.NewRequest(http.MethodPost, u.base+"/api/artifacts", pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mirror upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !is2xx(resp.StatusCode) {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("mirror upload failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Artifact struct {
			URL string `json:"url"`
		} `json:"artifact"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Artifact.URL == "" {
		return u.binhostURL() + "/" + strings.TrimPrefix(dir+"/"+name, u.dir+"/"), nil
	}
	return out.Artifact.URL, nil
}
