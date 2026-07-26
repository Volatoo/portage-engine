package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxPVEConfigBytes int64 = 1 << 20

const (
	maxGuestEvidenceBytes = 32 << 20
	guestEvidenceChunk    = 192 << 10
)

var (
	pveNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sha256Pattern    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	absoluteGuestBin = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

// PVEConfig contains only non-secret, reviewed Desktop E2E policy. PVE API
// credentials are supplied separately through environment variables.
type PVEConfig struct {
	SchemaVersion          int    `json:"schema_version"`
	Endpoint               string `json:"endpoint"`
	InsecureSkipTLSVerify  bool   `json:"insecure_skip_tls_verify"`
	CABundlePath           string `json:"ca_bundle_path,omitempty"`
	Node                   string `json:"node"`
	VMID                   int    `json:"vmid"`
	AllowedSnapshot        string `json:"allowed_snapshot"`
	StagingBinhost         string `json:"staging_binhost,omitempty"`
	StagingDigest          string `json:"staging_digest,omitempty"`
	GuestAgentPath         string `json:"guest_agent_path"`
	ArtifactDirectory      string `json:"artifact_directory"`
	LifecycleTimeoutSecond int    `json:"lifecycle_timeout_seconds"`
}

// LoadPVEConfig loads and validates a PVE desktop-runner configuration.
func LoadPVEConfig(path string) (*PVEConfig, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-selected policy file.
	if err != nil {
		return nil, fmt.Errorf("open desktop PVE config: %w", err)
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(io.LimitReader(f, maxPVEConfigBytes+1))
	dec.DisallowUnknownFields()
	var config PVEConfig
	if err := dec.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode desktop PVE config: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode desktop PVE config: multiple JSON values")
		}
		return nil, fmt.Errorf("decode desktop PVE config trailer: %w", err)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

// Validate checks the PVE desktop-runner configuration contract.
func (c *PVEConfig) Validate() error {
	if c.SchemaVersion != 1 || !pveNamePattern.MatchString(c.Node) || c.VMID < 100 || c.VMID > 999999999 || !pveNamePattern.MatchString(c.AllowedSnapshot) {
		return fmt.Errorf("desktop PVE config requires schema_version 1, valid node/snapshot and VMID")
	}
	endpoint, err := url.Parse(strings.TrimRight(strings.TrimSpace(c.Endpoint), "/"))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/api2/json") {
		return fmt.Errorf("desktop PVE endpoint must be an HTTPS origin or /api2/json URL without credentials, query, or fragment")
	}
	if c.CABundlePath != "" && (!filepath.IsAbs(c.CABundlePath) || c.InsecureSkipTLSVerify) {
		return fmt.Errorf("desktop PVE CA bundle must be absolute and cannot be combined with insecure TLS")
	}
	stagingConfigured := strings.TrimSpace(c.StagingBinhost) != "" || strings.TrimSpace(c.StagingDigest) != ""
	if stagingConfigured {
		binhost, err := url.Parse(strings.TrimSpace(c.StagingBinhost))
		if err != nil || (binhost.Scheme != "http" && binhost.Scheme != "https") || binhost.Hostname() == "" || binhost.User != nil || binhost.RawQuery != "" || binhost.Fragment != "" {
			return fmt.Errorf("desktop staging binhost must use HTTP(S) without credentials, query, or fragment")
		}
		if !sha256Pattern.MatchString(c.StagingDigest) {
			return fmt.Errorf("desktop staging digest must be a full sha256 digest")
		}
	}
	if !absoluteGuestBin.MatchString(c.GuestAgentPath) || !filepath.IsAbs(c.ArtifactDirectory) {
		return fmt.Errorf("desktop guest agent and artifact directory must be absolute safe paths")
	}
	if c.LifecycleTimeoutSecond < 30 || c.LifecycleTimeoutSecond > 1800 {
		return fmt.Errorf("desktop lifecycle timeout must be 30..1800 seconds")
	}
	return nil
}

// PVEQGADriver executes desktop actions through the PVE QEMU guest agent.
type PVEQGADriver struct {
	config  PVEConfig
	tokenID string
	secret  string
	client  *http.Client
	baseURL string
}

// NewPVEQGADriver creates a QGA driver using an operator-provided PVE token.
func NewPVEQGADriver(config *PVEConfig, tokenID, secret string) (*PVEQGADriver, error) {
	if config == nil {
		return nil, fmt.Errorf("desktop PVE config is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(tokenID) == "" || strings.TrimSpace(secret) == "" || strings.ContainsAny(tokenID+secret, "\r\n") {
		return nil, fmt.Errorf("desktop PVE token ID and secret are required")
	}
	endpoint := strings.TrimRight(config.Endpoint, "/")
	if !strings.HasSuffix(endpoint, "/api2/json") {
		endpoint += "/api2/json"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: config.InsecureSkipTLSVerify} // #nosec G402 -- explicit private-CA operator policy.
	if config.CABundlePath != "" {
		pem, err := os.ReadFile(config.CABundlePath) // #nosec G304 -- operator-selected absolute CA policy path.
		if err != nil {
			return nil, fmt.Errorf("read desktop PVE CA bundle: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("desktop PVE CA bundle contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	transport.TLSClientConfig = tlsConfig
	return &PVEQGADriver{
		config:  *config,
		tokenID: tokenID,
		secret:  secret,
		client:  &http.Client{Transport: transport, Timeout: 310 * time.Second},
		baseURL: endpoint,
	}, nil
}

// Do executes one bounded action and returns its observation.
func (d *PVEQGADriver) Do(ctx context.Context, action ActionRequest) (Observation, error) {
	switch action.Action {
	case "restore":
		if action.Input["snapshot"] != d.config.AllowedSnapshot {
			return Observation{}, fmt.Errorf("snapshot is not allowed by desktop PVE policy")
		}
		if err := d.ensureStopped(ctx); err != nil {
			return Observation{}, err
		}
		if err := d.taskPOST(ctx, d.vmPath()+"/snapshot/"+url.PathEscape(d.config.AllowedSnapshot)+"/rollback", url.Values{}); err != nil {
			return Observation{}, fmt.Errorf("restore reviewed desktop snapshot: %w", err)
		}
		return Observation{State: "passed", Message: "reviewed snapshot restored"}, nil
	case "start":
		if err := d.ensureStarted(ctx); err != nil {
			return Observation{}, err
		}
		if err := d.ensureDesktopReady(ctx); err != nil {
			return Observation{}, err
		}
		if d.config.StagingBinhost != "" {
			if _, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "prepare", d.config.StagingBinhost, d.config.StagingDigest}); err != nil {
				return Observation{}, fmt.Errorf("prepare locked staging binhost: %w", err)
			}
		}
		return Observation{State: "passed", Message: "VM and guest agent ready"}, nil
	case "stop":
		if err := d.ensureStopped(ctx); err != nil {
			return Observation{}, err
		}
		return Observation{State: "passed", Message: "VM stopped"}, nil
	case "install":
		if d.config.StagingBinhost == "" {
			return Observation{}, fmt.Errorf("install action requires a configured staging binhost")
		}
		if action.Input["staging_digest"] != d.config.StagingDigest {
			return Observation{}, fmt.Errorf("scenario staging digest does not match desktop PVE policy")
		}
		_, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "install", action.Input["atom"], action.Input["staging_digest"]})
		if err != nil {
			return Observation{}, err
		}
		return Observation{State: "passed", Message: "digest-bound binpkg installed"}, nil
	case "launch":
		_, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "launch", action.Input["application_id"]})
		if err != nil {
			return Observation{}, err
		}
		return Observation{State: "passed"}, nil
	case "wait_accessible":
		_, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "wait-accessible", action.Input["role"], action.Input["name"], action.Input["state"]})
		if err != nil {
			return Observation{}, err
		}
		return Observation{State: "passed"}, nil
	case "key":
		_, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "key", action.Input["keys"]})
		if err != nil {
			return Observation{}, err
		}
		return Observation{State: "passed"}, nil
	case "type":
		_, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "type", action.Input["text"]})
		if err != nil {
			return Observation{}, err
		}
		return Observation{State: "passed"}, nil
	case "click":
		if action.Input["x"] == "" || action.Input["y"] == "" || action.Input["accessibility_id"] != "" || action.Input["needle"] != "" {
			return Observation{}, fmt.Errorf("direct PVE adapter currently requires reviewed x/y click coordinates")
		}
		_, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "click", action.Input["x"], action.Input["y"]})
		if err != nil {
			return Observation{}, err
		}
		return Observation{State: "passed"}, nil
	case "screenshot":
		return d.collectScreenshot(ctx, action)
	case "collect_accessibility":
		return d.collectAccessibility(ctx, action)
	case "collect_logs":
		return d.collectLogs(ctx, action)
	case "assert_needle":
		return Observation{}, fmt.Errorf("needle comparison requires a reviewed openQA/needle adapter; direct PVE mode fails closed")
	default:
		return Observation{}, fmt.Errorf("unsupported direct PVE desktop action %q", action.Action)
	}
}

func (d *PVEQGADriver) vmPath() string {
	return fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(d.config.Node), d.config.VMID)
}

func (d *PVEQGADriver) ensureStarted(ctx context.Context) error {
	status, err := d.vmStatus(ctx)
	if err != nil {
		return err
	}
	if status != "running" {
		if err := d.taskPOST(ctx, d.vmPath()+"/status/start", url.Values{}); err != nil {
			return fmt.Errorf("start desktop VM: %w", err)
		}
	}
	deadline := time.Now().Add(time.Duration(d.config.LifecycleTimeoutSecond) * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := d.guestExecOnce(ctx, []string{"/usr/bin/true"}); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("desktop VM guest agent did not become ready")
}

func (d *PVEQGADriver) ensureStopped(ctx context.Context) error {
	status, err := d.vmStatus(ctx)
	if err != nil {
		return err
	}
	if status == "stopped" {
		return nil
	}
	if err := d.taskPOST(ctx, d.vmPath()+"/status/stop", url.Values{}); err != nil {
		return fmt.Errorf("stop desktop VM: %w", err)
	}
	deadline := time.Now().Add(time.Duration(d.config.LifecycleTimeoutSecond) * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		status, err = d.vmStatus(ctx)
		if err == nil && status == "stopped" {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("desktop VM did not stop before deadline")
}

func (d *PVEQGADriver) ensureDesktopReady(ctx context.Context) error {
	deadline := time.Now().Add(time.Duration(d.config.LifecycleTimeoutSecond) * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := d.guestExecOnce(ctx, []string{d.config.GuestAgentPath, "desktop-ready"}); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("desktop session did not become ready")
}

func (d *PVEQGADriver) vmStatus(ctx context.Context) (string, error) {
	var response struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := d.api(ctx, http.MethodGet, d.vmPath()+"/status/current", nil, &response); err != nil {
		return "", fmt.Errorf("read desktop VM status: %w", err)
	}
	if response.Data.Status != "running" && response.Data.Status != "stopped" {
		return "", fmt.Errorf("unexpected desktop VM status %q", response.Data.Status)
	}
	return response.Data.Status, nil
}

func (d *PVEQGADriver) taskPOST(ctx context.Context, path string, values url.Values) error {
	var response struct {
		Data string `json:"data"`
	}
	if err := d.api(ctx, http.MethodPost, path, values, &response); err != nil {
		return err
	}
	if response.Data == "" {
		return nil
	}
	deadline := time.Now().Add(time.Duration(d.config.LifecycleTimeoutSecond) * time.Second)
	for time.Now().Before(deadline) {
		var status struct {
			Data struct {
				Status     string `json:"status"`
				ExitStatus string `json:"exitstatus"`
			} `json:"data"`
		}
		path := fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(d.config.Node), url.PathEscape(response.Data))
		if err := d.api(ctx, http.MethodGet, path, nil, &status); err != nil {
			return err
		}
		if status.Data.Status == "stopped" {
			if status.Data.ExitStatus != "OK" {
				return fmt.Errorf("PVE task failed with %q", status.Data.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("PVE task did not finish before deadline")
}

func (d *PVEQGADriver) guestExec(ctx context.Context, command []string) (string, error) {
	// Named guest actions may be stateful (install, launch, input). Never retry
	// an ambiguous failure here; lifecycle readiness has its own read-only
	// /usr/bin/true polling loop in ensureStarted.
	return d.guestExecOnce(ctx, command)
}

func (d *PVEQGADriver) guestExecOnce(ctx context.Context, command []string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("empty guest command")
	}
	values := url.Values{}
	for _, arg := range command {
		if strings.ContainsRune(arg, '\x00') || len(arg) > 4096 {
			return "", fmt.Errorf("invalid guest command argument")
		}
		values.Add("command", arg)
	}
	var started struct {
		Data struct {
			PID int `json:"pid"`
		} `json:"data"`
	}
	if err := d.api(ctx, http.MethodPost, d.vmPath()+"/agent/exec", values, &started); err != nil {
		return "", err
	}
	if started.Data.PID < 1 {
		return "", fmt.Errorf("guest exec returned invalid PID")
	}
	for {
		var status struct {
			Data struct {
				Exited       int    `json:"exited"`
				ExitCode     int    `json:"exitcode"`
				OutData      string `json:"out-data"`
				ErrData      string `json:"err-data"`
				OutTruncated int    `json:"out-truncated"`
				ErrTruncated int    `json:"err-truncated"`
			} `json:"data"`
		}
		query := url.Values{"pid": []string{strconv.Itoa(started.Data.PID)}}
		if err := d.api(ctx, http.MethodGet, d.vmPath()+"/agent/exec-status", query, &status); err != nil {
			return "", err
		}
		if status.Data.Exited == 1 {
			if status.Data.ExitCode != 0 {
				message := strings.TrimSpace(status.Data.ErrData)
				if message == "" {
					message = strings.TrimSpace(status.Data.OutData)
				}
				return "", fmt.Errorf("guest helper exited %d: %s", status.Data.ExitCode, message)
			}
			if status.Data.OutTruncated != 0 || status.Data.ErrTruncated != 0 {
				return "", fmt.Errorf("guest helper output was truncated")
			}
			return status.Data.OutData, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (d *PVEQGADriver) collectScreenshot(ctx context.Context, action ActionRequest) (Observation, error) {
	base := safeArtifactBase(action.ScenarioID, action.StepID)
	guestPath := "/run/portage-engine/desktop-evidence/" + base + ".png.b64"
	if _, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "screenshot", guestPath}); err != nil {
		return Observation{}, err
	}
	content, err := d.guestFileRead(ctx, guestPath)
	if err != nil {
		return Observation{}, err
	}
	png, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(content)))
	if err != nil || len(png) < 8 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		return Observation{}, fmt.Errorf("guest screenshot is not valid base64 PNG")
	}
	path, digest, err := d.writeArtifact(base+".png", png)
	if err != nil {
		return Observation{}, err
	}
	return Observation{State: "passed", Message: digest, Artifacts: []string{path}}, nil
}

func (d *PVEQGADriver) collectLogs(ctx context.Context, action ActionRequest) (Observation, error) {
	base := safeArtifactBase(action.ScenarioID, action.StepID)
	guestPath := "/run/portage-engine/desktop-evidence/" + base + ".log"
	if _, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "collect-logs", action.Input["scope"], action.Input["application_id"], guestPath}); err != nil {
		return Observation{}, err
	}
	content, err := d.guestFileRead(ctx, guestPath)
	if err != nil {
		return Observation{}, err
	}
	path, digest, err := d.writeArtifact(base+".log", content)
	if err != nil {
		return Observation{}, err
	}
	return Observation{State: "passed", Message: digest, Artifacts: []string{path}}, nil
}

func (d *PVEQGADriver) collectAccessibility(ctx context.Context, action ActionRequest) (Observation, error) {
	base := safeArtifactBase(action.ScenarioID, action.StepID)
	guestPath := "/run/portage-engine/desktop-evidence/" + base + ".a11y.json"
	if _, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "dump-accessibility", guestPath}); err != nil {
		return Observation{}, err
	}
	content, err := d.guestFileRead(ctx, guestPath)
	if err != nil {
		return Observation{}, err
	}
	var tree struct {
		SchemaVersion int `json:"schema_version"`
		Nodes         []struct {
			Index  int      `json:"index"`
			Parent int      `json:"parent"`
			Role   string   `json:"role"`
			Name   string   `json:"name"`
			States []string `json:"states"`
		} `json:"nodes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tree); err != nil || tree.SchemaVersion != 1 || len(tree.Nodes) == 0 || len(tree.Nodes) > 4096 {
		return Observation{}, fmt.Errorf("guest accessibility evidence is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Observation{}, fmt.Errorf("guest accessibility evidence has trailing data")
	}
	for index, node := range tree.Nodes {
		if node.Index != index || node.Parent < -1 || node.Parent >= index || len(node.Role) > 128 || len(node.Name) > 512 || len(node.States) > 4 {
			return Observation{}, fmt.Errorf("guest accessibility node %d is invalid", index)
		}
	}
	path, digest, err := d.writeArtifact(base+".a11y.json", content)
	if err != nil {
		return Observation{}, err
	}
	return Observation{State: "passed", Message: digest, Artifacts: []string{path}}, nil
}

func (d *PVEQGADriver) guestFileRead(ctx context.Context, path string) ([]byte, error) {
	metadataOutput, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "evidence-info", path})
	if err != nil {
		return nil, fmt.Errorf("read guest evidence metadata: %w", err)
	}
	var metadata struct {
		Size   int    `json:"size"`
		SHA256 string `json:"sha256"`
	}
	decoder := json.NewDecoder(strings.NewReader(metadataOutput))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("decode guest evidence metadata: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode guest evidence metadata trailer")
	}
	if metadata.Size < 0 || metadata.Size > maxGuestEvidenceBytes || !sha256Pattern.MatchString(metadata.SHA256) {
		return nil, fmt.Errorf("guest evidence metadata is outside reviewed limits")
	}
	var result bytes.Buffer
	result.Grow(metadata.Size)
	for offset := 0; offset < metadata.Size; offset += guestEvidenceChunk {
		want := min(guestEvidenceChunk, metadata.Size-offset)
		encoded, err := d.guestExec(ctx, []string{d.config.GuestAgentPath, "read-evidence", path, strconv.Itoa(offset), strconv.Itoa(want)})
		if err != nil {
			return nil, fmt.Errorf("read guest evidence at offset %d: %w", offset, err)
		}
		chunk, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil || len(chunk) != want {
			return nil, fmt.Errorf("guest evidence chunk at offset %d is invalid", offset)
		}
		_, _ = result.Write(chunk)
	}
	data := result.Bytes()
	digest := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(digest[:]) != metadata.SHA256 {
		return nil, fmt.Errorf("guest evidence digest mismatch")
	}
	return data, nil
}

func (d *PVEQGADriver) writeArtifact(name string, data []byte) (string, string, error) {
	if err := os.MkdirAll(d.config.ArtifactDirectory, 0o750); err != nil {
		return "", "", err
	}
	path := filepath.Join(d.config.ArtifactDirectory, name)
	temporary, err := os.CreateTemp(d.config.ArtifactDirectory, ".desktop-evidence-*")
	if err != nil {
		return "", "", err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return "", "", err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", "", err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", "", err
	}
	if err := temporary.Close(); err != nil {
		return "", "", err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	return path, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func safeArtifactBase(scenarioID, stepID string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", "..", "_")
	return replacer.Replace(scenarioID + "--" + stepID)
}

func (d *PVEQGADriver) api(ctx context.Context, method, path string, values url.Values, output any) error {
	var body io.Reader
	requestURL := d.baseURL + path
	if method == http.MethodGet && len(values) > 0 {
		requestURL += "?" + values.Encode()
	} else if len(values) > 0 {
		body = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return err
	}
	if method != http.MethodGet && len(values) > 0 {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Authorization", "PVEAPIToken="+d.tokenID+"="+d.secret)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("PVE API %s %s returned HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(message)))
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode PVE API response: %w", err)
	}
	return nil
}
