package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/slchris/portage-engine/internal/iac"
	"github.com/slchris/portage-engine/pkg/config"
)

// handlers_settings.go exposes the dashboard-managed runtime configuration:
// cloud provisioning settings can be viewed and edited over the API instead of
// hand-editing server.conf. In PostgreSQL authority mode, non-secret settings
// are versioned and audited in runtime_settings while credential values remain
// process-injected. Standalone compatibility mode retains the local JSON file.

// cloudSettingsResponse is a CloudSettings with secrets redacted; booleans
// tell the UI whether a secret is stored (an empty secret on PUT means "keep
// the stored one").
type cloudSettingsResponse struct {
	config.CloudSettings
	HasPVETokenSecret             bool `json:"has_pve_token_secret"`
	HasPVEPassword                bool `json:"has_pve_password"`
	HasAWSSecretKey               bool `json:"has_aws_secret_key"`
	HasUploadPassword             bool `json:"has_upload_password"`
	SecretValuesManagedExternally bool `json:"secret_values_managed_externally"`
}

func redactedCloudSettings(cs *config.CloudSettings) cloudSettingsResponse {
	resp := cloudSettingsResponse{
		CloudSettings:     *cs.Clone(),
		HasPVETokenSecret: cs.PVETokenSecret != "",
		HasPVEPassword:    cs.PVEPassword != "",
		HasAWSSecretKey:   cs.AWSSecretKey != "",
		HasUploadPassword: cs.UploadPassword != "",
	}
	resp.PVETokenSecret = ""
	resp.PVEPassword = ""
	resp.AWSSecretKey = ""
	resp.UploadPassword = ""
	return resp
}

var cloudSettingSecretRefs = map[string]string{
	"pve_token_secret": "env:CLOUD_PVE_TOKEN_SECRET",
	"pve_password":     "env:CLOUD_PVE_PASSWORD",
	"aws_secret_key":   "env:CLOUD_AWS_SECRET_KEY",
	"upload_password":  "env:PORTAGE_UPLOAD_PASSWORD",
}

func stripCloudSettingSecrets(cs *config.CloudSettings) *config.CloudSettings {
	safe := cs.Clone()
	safe.PVETokenSecret = ""
	safe.PVEPassword = ""
	safe.AWSSecretKey = ""
	safe.UploadPassword = ""
	return safe
}

func applyCloudSettingSecrets(dst, source *config.CloudSettings) {
	dst.PVETokenSecret = source.PVETokenSecret
	dst.PVEPassword = source.PVEPassword
	dst.AWSSecretKey = source.AWSSecretKey
	dst.UploadPassword = source.UploadPassword
}

func containsCloudSettingSecrets(cs *config.CloudSettings) bool {
	return cs.PVETokenSecret != "" || cs.PVEPassword != "" ||
		cs.AWSSecretKey != "" || cs.UploadPassword != ""
}

// cloudSettingsPath is where dashboard-edited settings persist across
// restarts.
func (s *Server) cloudSettingsPath() string {
	dataDir := s.config.DataDir
	if dataDir == "" {
		dataDir = "/var/lib/portage-engine/server"
	}
	return filepath.Join(dataDir, "cloud-settings.json")
}

// loadCloudSettingsOverride applies the shared PostgreSQL setting when the
// durable control plane is enabled. Secret references are resolved by the
// process configuration and never read from PostgreSQL.
func (s *Server) loadCloudSettingsOverride() {
	if s.jobLedger != nil {
		var stored config.CloudSettings
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		version, _, found, err := s.jobLedger.LoadRuntimeSetting(
			ctx, "control-plane", "cloud", &stored,
		)
		cancel()
		if err != nil {
			log.Printf("Warning: cannot read PostgreSQL cloud settings: %v", err)
			return
		}
		if found {
			applyCloudSettingSecrets(&stored, s.builder.CloudSettings())
			s.builder.UpdateCloudSettings(&stored)
			log.Printf("Applied PostgreSQL cloud settings version %d (credential values resolved from process environment)", version)
			return
		}
		injected := s.builder.CloudSettings()
		safe := stripCloudSettingSecrets(injected)
		actor := "control-plane-bootstrap"
		// A pre-DB-4 installation may still have a local override. Import only
		// its non-secret fields; credential values must be rotated into env or
		// the deployment secret provider.
		data, readErr := os.ReadFile(s.cloudSettingsPath())
		if readErr == nil {
			var legacy config.CloudSettings
			if err := json.Unmarshal(data, &legacy); err != nil {
				log.Printf("Warning: ignoring corrupt legacy cloud settings override %s: %v", s.cloudSettingsPath(), err)
				return
			}
			if containsCloudSettingSecrets(&legacy) {
				log.Printf("Warning: legacy cloud-settings.json contains credential values; they were not imported. Rotate them into environment-backed secrets and remove the legacy file after verification")
			}
			safe = stripCloudSettingSecrets(&legacy)
			actor = "db4-legacy-import"
		} else if !os.IsNotExist(readErr) {
			log.Printf("Warning: cannot read legacy cloud settings override: %v", readErr)
			return
		}
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		created, err := s.jobLedger.EnsureRuntimeSetting(
			ctx, "control-plane", "cloud", safe, cloudSettingSecretRefs, actor,
		)
		cancel()
		if err != nil {
			log.Printf("Warning: initial cloud settings were not stored in PostgreSQL: %v", err)
			return
		}
		if !created {
			ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			_, _, found, err = s.jobLedger.LoadRuntimeSetting(
				ctx, "control-plane", "cloud", &stored,
			)
			cancel()
			if err != nil || !found {
				log.Printf("Warning: concurrently initialized cloud settings could not be reloaded: %v", err)
				return
			}
			safe = &stored
		}
		applyCloudSettingSecrets(safe, injected)
		s.builder.UpdateCloudSettings(safe)
		log.Printf("Initialized PostgreSQL cloud settings (created=%t; credential values resolved from process environment)", created)
		return
	}

	data, err := os.ReadFile(s.cloudSettingsPath())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Warning: cannot read cloud settings override: %v", err)
		}
		return
	}
	var cs config.CloudSettings
	if err := json.Unmarshal(data, &cs); err != nil {
		log.Printf("Warning: ignoring corrupt cloud settings override %s: %v", s.cloudSettingsPath(), err)
		return
	}
	s.builder.UpdateCloudSettings(&cs)
	log.Printf("Applied dashboard-managed cloud settings from %s (overrides server.conf)", s.cloudSettingsPath())
}

// handleCloudSettings serves GET (redacted view) and PUT (update + persist).
func (s *Server) handleCloudSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp := redactedCloudSettings(s.builder.CloudSettings())
		resp.SecretValuesManagedExternally = s.jobLedger != nil
		writeJSON(w, resp)
	case http.MethodPut:
		s.updateCloudSettings(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) updateCloudSettings(w http.ResponseWriter, r *http.Request) {
	var in config.CloudSettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	switch in.Provider {
	case "", "pve", "gcp", "aws":
	default:
		http.Error(w, fmt.Sprintf("unsupported provider %q", in.Provider), http.StatusBadRequest)
		return
	}
	if in.InstanceTTLMinutes < 0 {
		http.Error(w, "instance_ttl_minutes must be >= 0", http.StatusBadRequest)
		return
	}
	if in.SkipVerifyInstall {
		http.Error(w, "skip_verify_install is no longer supported: verification is mandatory before publication", http.StatusBadRequest)
		return
	}
	if in.BuildMode != "" && in.BuildMode != "native-gentoo" {
		http.Error(w, fmt.Sprintf("build_mode %q is no longer supported; only native-gentoo is available", in.BuildMode), http.StatusBadRequest)
		return
	}
	in.BuildMode = "native-gentoo"

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	if s.jobLedger != nil && containsCloudSettingSecrets(&in) {
		http.Error(w, "credential values cannot be persisted through the shared settings API; inject CLOUD_PVE_TOKEN_SECRET, CLOUD_PVE_PASSWORD, CLOUD_AWS_SECRET_KEY, or PORTAGE_UPLOAD_PASSWORD via the deployment secret provider", http.StatusBadRequest)
		return
	}

	// An empty secret means "keep the stored one" so the UI never has to
	// round-trip (or even see) the real secrets.
	cur := s.builder.CloudSettings()
	if in.PVETokenSecret == "" {
		in.PVETokenSecret = cur.PVETokenSecret
	}
	if in.PVEPassword == "" {
		in.PVEPassword = cur.PVEPassword
	}
	if in.AWSSecretKey == "" {
		in.AWSSecretKey = cur.AWSSecretKey
	}
	if in.UploadPassword == "" {
		in.UploadPassword = cur.UploadPassword
	}

	if s.jobLedger != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		version, err := s.jobLedger.SaveRuntimeSetting(
			ctx, "control-plane", "cloud", stripCloudSettingSecrets(&in),
			cloudSettingSecretRefs, "settings-api", r.Header.Get("X-Request-ID"),
		)
		cancel()
		if err != nil {
			http.Error(w, "settings were not applied: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		s.builder.UpdateCloudSettings(&in)
		log.Printf("Cloud settings version %d updated via API (provider=%s, pve_endpoint=%s)", version, in.Provider, in.PVEEndpoint)
		resp := redactedCloudSettings(&in)
		resp.SecretValuesManagedExternally = true
		writeJSON(w, resp)
		return
	}

	s.builder.UpdateCloudSettings(&in)

	// Persist (with the secret — mode 0600) so the settings survive restarts.
	path := s.cloudSettingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		http.Error(w, "settings applied but not persisted: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data, err := json.MarshalIndent(&in, "", "  ")
	if err != nil {
		http.Error(w, "settings applied but not persisted: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		http.Error(w, "settings applied but not persisted: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Cloud settings updated via API (provider=%s, pve_endpoint=%s)", in.Provider, in.PVEEndpoint)
	writeJSON(w, redactedCloudSettings(&in))
}

// handleCloudSettingsTest validates PVE connectivity for the posted settings
// (falling back to stored values for omitted fields, notably the secret) and
// returns the cluster's nodes so the UI can show what the scheduler sees.
func (s *Server) handleCloudSettingsTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var in config.CloudSettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	cur := s.builder.CloudSettings()
	if in.PVEEndpoint == "" {
		in.PVEEndpoint = cur.PVEEndpoint
	}
	if in.PVETokenID == "" {
		in.PVETokenID = cur.PVETokenID
	}
	if in.PVETokenSecret == "" {
		in.PVETokenSecret = cur.PVETokenSecret
	}
	if in.PVEUsername == "" {
		in.PVEUsername = cur.PVEUsername
	}
	if in.PVEPassword == "" {
		in.PVEPassword = cur.PVEPassword
	}
	if in.PVETemplate == "" {
		in.PVETemplate = cur.PVETemplate
	}

	type testResponse struct {
		OK    bool              `json:"ok"`
		Error string            `json:"error,omitempty"`
		Nodes []iac.PVENodeInfo `json:"nodes,omitempty"`
	}

	auth := iac.PVEAuth{
		TokenID:     in.PVETokenID,
		TokenSecret: in.PVETokenSecret,
		Username:    in.PVEUsername,
		Password:    in.PVEPassword,
		Insecure:    in.PVEInsecure,
	}
	nodes, err := iac.PVEClusterNodes(in.PVEEndpoint, auth, in.PVETemplate)
	if err != nil {
		writeJSON(w, testResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, testResponse{OK: true, Nodes: nodes})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
