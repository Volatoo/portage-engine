package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

type signerPublicMetadata struct {
	KeyID     string    `json:"key_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Server) signerPublicMetadataPath() string {
	return s.config.GPGPublicKeyPath + ".json"
}

// gpgKeyMaterial deliberately returns no secret-key bytes. Control-plane and
// builder processes can distribute the isolated signer's public key, but no
// code path in either process can export or deploy the release private key.
func (s *Server) gpgKeyMaterial() (string, []byte, []byte) {
	if !s.config.GPGEnabled || s.config.GPGPublicKeyPath == "" {
		return "", nil, nil
	}
	publicKey, err := os.ReadFile(s.config.GPGPublicKeyPath) // #nosec G304 -- operator-configured public key path.
	if err != nil || len(publicKey) == 0 {
		return "", nil, nil
	}
	var metadata signerPublicMetadata
	data, err := os.ReadFile(s.signerPublicMetadataPath()) // #nosec G304 -- adjacent signer-owned metadata.
	if err == nil {
		_ = json.Unmarshal(data, &metadata)
	}
	return metadata.KeyID, publicKey, nil
}

func (s *Server) handleGPGPubkey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_, publicKey, _ := s.gpgKeyMaterial()
	if len(publicKey) == 0 {
		http.Error(w, "isolated signer public key is not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/pgp-keys")
	w.Header().Set("Content-Disposition", `attachment; filename="portage-engine.asc"`)
	_, _ = w.Write(publicKey)
}

func (s *Server) handleGPGStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	keyID, publicKey, _ := s.gpgKeyMaterial()
	response := map[string]any{
		"enabled":          s.config.GPGEnabled,
		"ready":            len(publicKey) > 0,
		"key_id":           keyID,
		"mode":             "isolated-outbound-pull",
		"private_key_here": false,
	}
	if s.jobLedger != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		status, err := s.jobLedger.SigningRuntimeStatus(ctx)
		cancel()
		if err != nil {
			response["queue_error"] = err.Error()
		} else {
			response["queue"] = status
		}
	}
	writeJSON(w, response)
}

// Key generation is intentionally unavailable from the control plane. The
// isolated signer owns key lifecycle and writes only its public key to the
// shared read-only distribution path.
func (s *Server) handleGPGGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.Error(w, "signing keys are managed only by the isolated portage-signer", http.StatusConflict)
}
