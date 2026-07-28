// Package gpg provides GPG signing functionality for binary packages.
package gpg

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const activeKeyFile = ".portage-engine-active-key"

// Signer handles GPG signing of packages.
type Signer struct {
	keyID      string
	keyPath    string
	enabled    bool
	gnupgHome  string // Custom GNUPG_HOME directory
	autoCreate bool   // Auto-create key if not exists
	keyName    string // Name for auto-generated key
	keyEmail   string // Email for auto-generated key

	// pubKeyMu guards the cached public key, which may be populated
	// concurrently by HTTP handlers calling GetPublicKey.
	pubKeyMu  sync.Mutex
	publicKey string // Cached public key in ASCII armor format
}

// SignerOption is a functional option for configuring the Signer.
type SignerOption func(*Signer)

// WithGnupgHome sets a custom GNUPG_HOME directory.
func WithGnupgHome(home string) SignerOption {
	return func(s *Signer) {
		s.gnupgHome = home
	}
}

// WithAutoCreate enables automatic key generation if key doesn't exist.
func WithAutoCreate(name, email string) SignerOption {
	return func(s *Signer) {
		s.autoCreate = true
		s.keyName = name
		s.keyEmail = email
	}
}

// NewSigner creates a new GPG signer.
func NewSigner(keyID, keyPath string, enabled bool, opts ...SignerOption) *Signer {
	s := &Signer{
		keyID:   keyID,
		keyPath: keyPath,
		enabled: enabled,
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// IsEnabled returns whether GPG signing is enabled.
func (s *Signer) IsEnabled() bool {
	return s.enabled
}

// KeyID returns the GPG key ID.
func (s *Signer) KeyID() string {
	return s.keyID
}

// SetKeyID sets the GPG key ID.
func (s *Signer) SetKeyID(keyID string) {
	s.keyID = keyID
}

// Initialize initializes the signer, creating a key if necessary.
func (s *Signer) Initialize() error {
	if !s.enabled {
		return nil
	}

	// If we have a key ID, check if it exists
	if s.keyID != "" {
		if s.keyExists(s.keyID) {
			log.Printf("GPG key %s found", s.keyID)
			return nil
		}
		log.Printf("Configured GPG key %s not found", s.keyID)
	}

	// If auto-create is enabled and no key ID was configured, serialize key
	// discovery/generation across signer processes sharing the same GNUPGHOME.
	if s.autoCreate {
		if s.gnupgHome == "" {
			return fmt.Errorf("GPG auto-create requires a dedicated GNUPGHOME")
		}
		if err := os.MkdirAll(s.gnupgHome, 0700); err != nil {
			return fmt.Errorf("create GNUPGHOME: %w", err)
		}
		if err := os.Chmod(s.gnupgHome, 0700); err != nil { // #nosec G302 -- 0700 is the required secure mode for a GnuPG directory, not a regular file.
			return fmt.Errorf("secure GNUPGHOME: %w", err)
		}
		lock, err := os.OpenFile(filepath.Join(s.gnupgHome, ".portage-engine-key-init.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return fmt.Errorf("open GPG key initialization lock: %w", err)
		}
		defer func() { _ = lock.Close() }()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			return fmt.Errorf("lock GPG key initialization: %w", err)
		}
		defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

		return s.initializeAutoKey()
	}

	if s.keyID == "" {
		return fmt.Errorf("GPG enabled but no key ID configured")
	}

	return nil
}

func (s *Signer) initializeAutoKey() error {
	markerPath := filepath.Join(s.gnupgHome, activeKeyFile)
	if marker, err := os.ReadFile(markerPath); err == nil { // #nosec G304 -- GNUPGHOME is operator-configured and the marker file name is fixed.
		keyID := strings.TrimSpace(string(marker))
		if keyID == "" || !s.keyExists(keyID) {
			return fmt.Errorf("active GPG key marker %s references a missing key; restore the key or configure GPG_KEY_ID explicitly", markerPath)
		}
		s.keyID = keyID
		log.Printf("Reusing active GPG key %s", keyID)
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read active GPG key marker: %w", err)
	}

	keyIDs, err := s.findSecretKeyIDs(s.keyEmail)
	if err != nil {
		return fmt.Errorf("discover existing GPG keys: %w", err)
	}
	if len(keyIDs) > 0 {
		s.keyID = keyIDs[0]
		if len(keyIDs) > 1 {
			log.Printf("WARNING: found %d matching secret keys; pinning the first key %s in %s", len(keyIDs), s.keyID, markerPath)
		} else {
			log.Printf("Reusing existing GPG key %s", s.keyID)
		}
		return writeKeyMarker(markerPath, s.keyID)
	}

	log.Printf("Auto-creating GPG key for %s <%s>", s.keyName, s.keyEmail)
	keyID, err := s.generateKey()
	if err != nil {
		return fmt.Errorf("failed to generate GPG key: %w", err)
	}
	s.keyID = keyID
	if err := writeKeyMarker(markerPath, keyID); err != nil {
		return err
	}
	log.Printf("Generated GPG key: %s", keyID)
	return nil
}

func writeKeyMarker(path, keyID string) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create active GPG key marker: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("secure active GPG key marker: %w", err)
	}
	if _, err := temp.WriteString(keyID + "\n"); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write active GPG key marker: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync active GPG key marker: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close active GPG key marker: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish active GPG key marker: %w", err)
	}
	return nil
}

// keyExists checks if a GPG key exists.
func (s *Signer) keyExists(keyID string) bool {
	args := s.buildBaseArgs()
	args = append(args, "--list-secret-keys", keyID)

	cmd := exec.Command("gpg", args...) // #nosec G204 -- fixed gpg executable with an argument vector, not a shell.
	if s.gnupgHome != "" {
		cmd.Env = append(os.Environ(), "GNUPGHOME="+s.gnupgHome)
	}

	return cmd.Run() == nil
}

// generateKey generates a new GPG key.
func (s *Signer) generateKey() (string, error) {
	// Ensure GNUPGHOME exists
	if s.gnupgHome != "" {
		if err := os.MkdirAll(s.gnupgHome, 0700); err != nil {
			return "", fmt.Errorf("failed to create GNUPGHOME: %w", err)
		}
	}

	name := s.keyName
	if name == "" {
		name = "Portage Engine"
	}
	email := s.keyEmail
	if email == "" {
		email = "portage@localhost"
	}

	// Generate key using batch mode
	batchConfig := fmt.Sprintf(`%%no-protection
Key-Type: RSA
Key-Length: 4096
Subkey-Type: RSA
Subkey-Length: 4096
Name-Real: %s
Name-Email: %s
Expire-Date: 0
%%commit
`, name, email)

	args := s.buildBaseArgs()
	args = append(args, "--batch", "--gen-key")

	cmd := exec.Command("gpg", args...) // #nosec G204 -- fixed gpg executable with an argument vector, not a shell.
	cmd.Stdin = strings.NewReader(batchConfig)
	if s.gnupgHome != "" {
		cmd.Env = append(os.Environ(), "GNUPGHOME="+s.gnupgHome)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gpg --gen-key failed: %w, stderr: %s", err, stderr.String())
	}

	// Get the key ID of the newly generated key
	return s.getLatestKeyID(email)
}

// getLatestKeyID gets the key ID for a given email.
func (s *Signer) getLatestKeyID(email string) (string, error) {
	keyIDs, err := s.findSecretKeyIDs(email)
	if err != nil {
		return "", err
	}
	if len(keyIDs) == 0 {
		return "", fmt.Errorf("could not find key ID in output")
	}
	return keyIDs[len(keyIDs)-1], nil
}

func (s *Signer) findSecretKeyIDs(identity string) ([]string, error) {
	args := s.buildBaseArgs()
	args = append(args, "--batch", "--with-colons", "--list-secret-keys")

	cmd := exec.Command("gpg", args...) // #nosec G204 -- fixed gpg executable with an argument vector, not a shell.
	if s.gnupgHome != "" {
		cmd.Env = append(os.Environ(), "GNUPGHOME="+s.gnupgHome)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to list keys: %w, stderr: %s", err, strings.TrimSpace(stderr.String()))
	}

	var keyIDs []string
	seen := make(map[string]struct{})
	currentKeyID := ""
	for _, line := range strings.Split(stdout.String(), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) <= 4 {
			continue
		}
		switch fields[0] {
		case "sec":
			currentKeyID = fields[4]
			if identity == "" && currentKeyID != "" {
				if _, ok := seen[currentKeyID]; !ok {
					seen[currentKeyID] = struct{}{}
					keyIDs = append(keyIDs, currentKeyID)
				}
			}
		case "uid":
			if identity == "" || currentKeyID == "" || len(fields) <= 9 || !strings.Contains(fields[9], identity) {
				continue
			}
			if _, ok := seen[currentKeyID]; !ok {
				seen[currentKeyID] = struct{}{}
				keyIDs = append(keyIDs, currentKeyID)
			}
		}
	}
	return keyIDs, nil
}

// buildBaseArgs builds base GPG arguments.
func (s *Signer) buildBaseArgs() []string {
	var args []string
	if s.keyPath != "" {
		if _, err := os.Stat(s.keyPath); err == nil {
			args = append(args, "--no-default-keyring", "--keyring", s.keyPath)
		}
	}
	return args
}

// GetPublicKey returns the public key in ASCII armor format.
func (s *Signer) GetPublicKey() (string, error) {
	s.pubKeyMu.Lock()
	defer s.pubKeyMu.Unlock()

	if s.publicKey != "" {
		return s.publicKey, nil
	}

	if s.keyID == "" {
		return "", fmt.Errorf("no key ID configured")
	}

	args := s.buildBaseArgs()
	args = append(args, "--armor", "--export", s.keyID)

	cmd := exec.Command("gpg", args...) // #nosec G204 -- fixed gpg executable with an argument vector, not a shell.
	if s.gnupgHome != "" {
		cmd.Env = append(os.Environ(), "GNUPGHOME="+s.gnupgHome)
	}

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to export public key: %w", err)
	}

	s.publicKey = stdout.String()
	return s.publicKey, nil
}

// ExportPublicKey exports the public key to a file.
func (s *Signer) ExportPublicKey(path string) error {
	key, err := s.GetPublicKey()
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, []byte(key), 0600); err != nil {
		return fmt.Errorf("failed to write public key: %w", err)
	}

	log.Printf("Public key exported to: %s", path)
	return nil
}

// GetSecretKey returns the secret key in ASCII armor format.
// This is used for package signing in containers.
func (s *Signer) GetSecretKey() (string, error) {
	if s.keyID == "" {
		return "", fmt.Errorf("no key ID configured")
	}

	args := s.buildBaseArgs()
	args = append(args, "--armor", "--export-secret-keys", s.keyID)

	cmd := exec.Command("gpg", args...) // #nosec G204 -- fixed gpg executable with an argument vector, not a shell.
	if s.gnupgHome != "" {
		cmd.Env = append(os.Environ(), "GNUPGHOME="+s.gnupgHome)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to export secret key: %w, stderr: %s", err, stderr.String())
	}

	return stdout.String(), nil
}

// ExportSecretKey exports the secret key to a file.
// This is needed for container-based package signing.
func (s *Signer) ExportSecretKey(path string) error {
	key, err := s.GetSecretKey()
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, []byte(key), 0600); err != nil {
		return fmt.Errorf("failed to write secret key: %w", err)
	}

	log.Printf("Secret key exported successfully")
	return nil
}

// ExportKeyPair exports both public and secret keys to a directory.
// Returns the paths to the public and secret key files.
func (s *Signer) ExportKeyPair(dir string) (publicKeyPath, secretKeyPath string, err error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", "", fmt.Errorf("failed to create directory: %w", err)
	}

	publicKeyPath = filepath.Join(dir, "public.asc")
	secretKeyPath = filepath.Join(dir, "secret.asc")

	if err := s.ExportPublicKey(publicKeyPath); err != nil {
		return "", "", fmt.Errorf("failed to export public key: %w", err)
	}

	if err := s.ExportSecretKey(secretKeyPath); err != nil {
		return "", "", fmt.Errorf("failed to export secret key: %w", err)
	}

	return publicKeyPath, secretKeyPath, nil
}

// SignPackage signs a package file with GPG.
func (s *Signer) SignPackage(packagePath string) error {
	if !s.enabled {
		log.Printf("GPG signing disabled, skipping signature for %s", packagePath)
		return nil
	}

	if s.keyID == "" {
		return fmt.Errorf("GPG key ID not configured")
	}

	signaturePath := packagePath + ".sig"

	log.Printf("Signing package: %s with key %s", packagePath, s.keyID)

	args := s.buildBaseArgs()
	args = append(args,
		"--detach-sign",
		"--armor",
		"--batch",
		"--yes",
		"--local-user", s.keyID,
		"--output", signaturePath,
		packagePath,
	)

	cmd := exec.Command("gpg", args...) // #nosec G204 -- fixed gpg executable with an argument vector, not a shell.
	if s.gnupgHome != "" {
		cmd.Env = append(os.Environ(), "GNUPGHOME="+s.gnupgHome)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to sign package: %w, stderr: %s", err, stderr.String())
	}

	log.Printf("Package signed successfully: %s", signaturePath)
	return nil
}

// VerifyPackage verifies a package signature.
func (s *Signer) VerifyPackage(packagePath string) error {
	signaturePath := packagePath + ".sig"

	if _, err := os.Stat(signaturePath); os.IsNotExist(err) {
		return fmt.Errorf("signature file not found: %s", signaturePath)
	}

	log.Printf("Verifying package signature: %s", packagePath)

	args := s.buildBaseArgs()
	args = append(args, "--verify", signaturePath, packagePath)

	cmd := exec.Command("gpg", args...) // #nosec G204 -- fixed gpg executable with an argument vector, not a shell.
	if s.gnupgHome != "" {
		cmd.Env = append(os.Environ(), "GNUPGHOME="+s.gnupgHome)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("signature verification failed: %w, stderr: %s", err, stderr.String())
	}

	log.Printf("Package signature verified successfully")
	return nil
}

// SignDirectory signs all packages in a directory.
func (s *Signer) SignDirectory(dir string) error {
	if !s.enabled {
		return nil
	}

	pattern := filepath.Join(dir, "*.tbz2")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("failed to find packages: %w", err)
	}

	for _, pkg := range matches {
		if err := s.SignPackage(pkg); err != nil {
			log.Printf("Warning: failed to sign %s: %v", pkg, err)
		}
	}

	return nil
}

// CheckGPG checks if GPG is available.
func CheckGPG() error {
	cmd := exec.Command("gpg", "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("GPG is not available: %w", err)
	}
	return nil
}

// ListKeys lists available GPG keys.
func ListKeys() error {
	cmd := exec.Command("gpg", "--list-secret-keys")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
