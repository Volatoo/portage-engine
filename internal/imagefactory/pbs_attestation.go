package imagefactory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var pbsFingerprintPattern = regexp.MustCompile(`^(?:[0-9a-f]{2}:){31}[0-9a-f]{2}$`)
var pbsOwnerPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+@[a-zA-Z0-9._-]+(?:![a-zA-Z0-9._-]+)?$`)

type PBSEvidenceDigest struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type PBSVerification struct {
	State string `json:"state"`
	UPID  string `json:"upid"`
}

type PBSImageIndex struct {
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum"`
	CryptMode string `json:"crypt_mode"`
}

type PBSTemplateContract struct {
	VMID      int    `json:"vmid"`
	Name      string `json:"name"`
	Template  int    `json:"template"`
	CIUpgrade int    `json:"ciupgrade"`
	BIOS      string `json:"bios"`
	Machine   string `json:"machine"`
	Boot      string `json:"boot"`
	Agent     string `json:"agent"`
}

type PBSRestoreGate struct {
	RestoreVMID             int               `json:"restore_vmid"`
	SmokeVMID               int               `json:"smoke_vmid"`
	CloudInitRuns           int               `json:"cloud_init_runs"`
	ImplicitEmergeSync      bool              `json:"implicit_emerge_sync"`
	TemporaryVMsDestroyed   bool              `json:"temporary_vms_destroyed"`
	OS                      string            `json:"os"`
	Kernel                  string            `json:"kernel"`
	Profile                 string            `json:"profile"`
	RootFS                  string            `json:"rootfs"`
	FirstBootEvidence       PBSEvidenceDigest `json:"first_boot_evidence"`
	RuntimeEvidence         PBSEvidenceDigest `json:"runtime_evidence"`
	SecondCloudInitEvidence PBSEvidenceDigest `json:"second_cloud_init_evidence"`
	CleanupEvidence         PBSEvidenceDigest `json:"cleanup_evidence"`
}

// PBSSourceAttestation binds one retention-protected, server-verified PBS
// snapshot to its decoded manifest, QEMU configuration and restore/boot gate.
type PBSSourceAttestation struct {
	SchemaVersion          int                 `json:"schema_version"`
	CreatedAt              time.Time           `json:"created_at"`
	PBSURL                 string              `json:"pbs_url"`
	CertificateFingerprint string              `json:"certificate_fingerprint"`
	Datastore              string              `json:"datastore"`
	BackupType             string              `json:"backup_type"`
	BackupID               string              `json:"backup_id"`
	BackupTime             int64               `json:"backup_time"`
	Snapshot               string              `json:"snapshot"`
	Owner                  string              `json:"owner"`
	Comment                string              `json:"comment"`
	Protected              bool                `json:"protected"`
	Verification           PBSVerification     `json:"verification"`
	DecodedIndex           PBSEvidenceDigest   `json:"decoded_index"`
	QEMUConfig             PBSEvidenceDigest   `json:"qemu_config"`
	ImageIndexes           []PBSImageIndex     `json:"image_indexes"`
	Template               PBSTemplateContract `json:"template"`
	RestoreGate            PBSRestoreGate      `json:"restore_gate"`
}

type PBSAttestationRequest struct {
	PBSURL, CertificateFingerprint, Datastore string
	SnapshotPath, IndexPath, QEMUConfigPath   string
	FirstBootLogPath, RuntimeLogPath          string
	SecondCloudInitLogPath, CleanupPath       string
	RestoreVMID, SmokeVMID                    int
}

type PBSSourceStampEvidence struct {
	SchemaVersion     int       `json:"schema_version"`
	StampedAt         time.Time `json:"stamped_at"`
	VMID              int       `json:"vmid"`
	Node              string    `json:"node"`
	Template          string    `json:"template"`
	Snapshot          string    `json:"snapshot"`
	AttestationDigest string    `json:"attestation_digest"`
	CIUpgrade         int       `json:"ciupgrade"`
	Verified          bool      `json:"verified"`
}

type pbsSnapshotInput struct {
	BackupType   string            `json:"backup-type"`
	BackupID     string            `json:"backup-id"`
	BackupTime   int64             `json:"backup-time"`
	Owner        string            `json:"owner"`
	Comment      string            `json:"comment"`
	Verification PBSVerification   `json:"verification"`
	Protected    bool              `json:"protected"`
	Size         int64             `json:"size"`
	Files        []pbsSnapshotFile `json:"files"`
}

type pbsSnapshotFile struct {
	Filename  string `json:"filename"`
	CryptMode string `json:"crypt-mode,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

type pbsIndexInput struct {
	BackupID    string          `json:"backup-id"`
	BackupTime  int64           `json:"backup-time"`
	BackupType  string          `json:"backup-type"`
	Files       []pbsIndexFile  `json:"files"`
	Signature   json.RawMessage `json:"signature"`
	Unprotected struct {
		ChunkUploadStats json.RawMessage `json:"chunk_upload_stats"`
		Notes            string          `json:"notes"`
		VerifyState      PBSVerification `json:"verify_state"`
	} `json:"unprotected"`
}

type pbsIndexFile struct {
	CryptMode string `json:"crypt-mode"`
	Checksum  string `json:"csum"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
}

type pveCleanupInput struct {
	TemporaryVMIDsRemaining []int `json:"temporary_vmids_remaining"`
	Source                  []struct {
		VMID     int    `json:"vmid"`
		Name     string `json:"name"`
		Status   string `json:"status"`
		Template int    `json:"template"`
		Node     string `json:"node"`
	} `json:"source"`
}

func CreatePBSSourceAttestation(request PBSAttestationRequest, now time.Time) (*PBSSourceAttestation, error) {
	if err := validatePBSAttestationRequest(request); err != nil {
		return nil, err
	}
	snapshot, vmid, err := loadPBSSnapshot(request.SnapshotPath)
	if err != nil {
		return nil, err
	}
	_, imageIndexes, err := loadPBSImageIndexes(request.IndexPath, snapshot)
	if err != nil {
		return nil, err
	}
	_, config, err := loadPBSQEMUConfig(request.QEMUConfigPath, imageIndexes)
	if err != nil {
		return nil, err
	}
	restore, err := loadPBSRestoreEvidence(request, vmid, config["name"])
	if err != nil {
		return nil, err
	}
	createdAt := now.UTC()
	if createdAt.Before(time.Unix(snapshot.BackupTime, 0).UTC()) {
		return nil, fmt.Errorf("attestation time precedes the PBS snapshot")
	}
	decodedIndex, err := digestEvidence(request.IndexPath)
	if err != nil {
		return nil, err
	}
	qemuConfig, err := digestEvidence(request.QEMUConfigPath)
	if err != nil {
		return nil, err
	}
	return &PBSSourceAttestation{
		SchemaVersion: 1, CreatedAt: createdAt, PBSURL: request.PBSURL, CertificateFingerprint: request.CertificateFingerprint, Datastore: request.Datastore,
		BackupType: snapshot.BackupType, BackupID: snapshot.BackupID, BackupTime: snapshot.BackupTime,
		Snapshot: fmt.Sprintf("%s/%s/%s", snapshot.BackupType, snapshot.BackupID, time.Unix(snapshot.BackupTime, 0).UTC().Format("2006-01-02T15:04:05Z")),
		Owner:    snapshot.Owner, Comment: snapshot.Comment, Protected: true, Verification: snapshot.Verification,
		DecodedIndex: decodedIndex, QEMUConfig: qemuConfig, ImageIndexes: imageIndexes,
		Template: PBSTemplateContract{VMID: vmid, Name: config["name"], Template: 1, CIUpgrade: 0, BIOS: config["bios"], Machine: config["machine"], Boot: config["boot"], Agent: config["agent"]},
		RestoreGate: PBSRestoreGate{RestoreVMID: request.RestoreVMID, SmokeVMID: request.SmokeVMID, CloudInitRuns: 2, ImplicitEmergeSync: false, TemporaryVMsDestroyed: true,
			OS: evidenceValue(restore.runtime.content, "OS="), Kernel: evidenceValue(restore.runtime.content, "KERNEL="), Profile: evidenceValue(restore.runtime.content, "PROFILE="), RootFS: evidenceValue(restore.runtime.content, "ROOT_FS="),
			FirstBootEvidence: restore.firstBoot.digest, RuntimeEvidence: restore.runtime.digest, SecondCloudInitEvidence: restore.secondBoot.digest, CleanupEvidence: restore.cleanupDigest},
	}, nil
}

func validatePBSAttestationRequest(request PBSAttestationRequest) error {
	if request.RestoreVMID < 100 || request.SmokeVMID < 100 || request.RestoreVMID == request.SmokeVMID {
		return fmt.Errorf("restore and smoke VMIDs must be distinct PVE VMIDs")
	}
	parsedURL, err := url.Parse(request.PBSURL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" || parsedURL.Path != "" {
		return fmt.Errorf("pbs_url must be an HTTPS origin without path, credentials, query, or fragment")
	}
	if !pbsFingerprintPattern.MatchString(request.CertificateFingerprint) || !repoComponentPattern.MatchString(request.Datastore) {
		return fmt.Errorf("invalid PBS fingerprint or datastore")
	}
	return nil
}

func loadPBSSnapshot(path string) (*pbsSnapshotInput, int, error) {
	var snapshot pbsSnapshotInput
	if err := decodeStrictFile(path, &snapshot); err != nil {
		return nil, 0, fmt.Errorf("load PBS snapshot evidence: %w", err)
	}
	if snapshot.BackupType != "vm" || snapshot.BackupTime <= 0 || !pbsOwnerPattern.MatchString(snapshot.Owner) || snapshot.Comment == "" || snapshot.Size <= 0 {
		return nil, 0, fmt.Errorf("PBS snapshot identity is incomplete")
	}
	vmid, err := strconv.Atoi(snapshot.BackupID)
	if err != nil || vmid < 100 {
		return nil, 0, fmt.Errorf("PBS snapshot backup-id is not a PVE VMID")
	}
	if !snapshot.Protected {
		return nil, 0, fmt.Errorf("PBS source snapshot must be protected from prune before attestation")
	}
	if snapshot.Verification.State != "ok" || !validVerifyUPID(snapshot.Verification.UPID, snapshot.Owner) {
		return nil, 0, fmt.Errorf("PBS snapshot lacks a successful server-side verification")
	}
	return &snapshot, vmid, nil
}

func loadPBSImageIndexes(path string, snapshot *pbsSnapshotInput) (*pbsIndexInput, []PBSImageIndex, error) {
	var index pbsIndexInput
	if err := decodeStrictFile(path, &index); err != nil {
		return nil, nil, fmt.Errorf("load decoded PBS index: %w", err)
	}
	if index.BackupType != snapshot.BackupType || index.BackupID != snapshot.BackupID || index.BackupTime != snapshot.BackupTime || index.Unprotected.Notes != snapshot.Comment || index.Unprotected.VerifyState != snapshot.Verification {
		return nil, nil, fmt.Errorf("decoded PBS index does not match snapshot identity and verification")
	}
	snapshotFiles := make(map[string]pbsSnapshotFile, len(snapshot.Files))
	for _, file := range snapshot.Files {
		if file.Filename == "" {
			return nil, nil, fmt.Errorf("PBS snapshot contains an unnamed file")
		}
		if _, duplicate := snapshotFiles[file.Filename]; duplicate {
			return nil, nil, fmt.Errorf("PBS snapshot contains duplicate file %q", file.Filename)
		}
		snapshotFiles[file.Filename] = file
	}
	imageIndexes := make([]PBSImageIndex, 0, len(index.Files))
	qemuConfigFound, diskIndexFound := false, false
	seenIndexFiles := map[string]struct{}{}
	for _, file := range index.Files {
		if file.Filename == "" || file.Size <= 0 || !validSHA256(file.Checksum) || file.CryptMode != "none" {
			return nil, nil, fmt.Errorf("decoded PBS index contains invalid or encrypted file %q", file.Filename)
		}
		if _, duplicate := seenIndexFiles[file.Filename]; duplicate {
			return nil, nil, fmt.Errorf("decoded PBS index contains duplicate file %q", file.Filename)
		}
		seenIndexFiles[file.Filename] = struct{}{}
		snapshotFile, exists := snapshotFiles[file.Filename]
		if !exists || snapshotFile.Size != file.Size || snapshotFile.CryptMode != file.CryptMode {
			return nil, nil, fmt.Errorf("PBS snapshot metadata does not match index file %q", file.Filename)
		}
		if file.Filename == "qemu-server.conf.blob" {
			qemuConfigFound = true
			continue
		}
		if strings.HasSuffix(file.Filename, ".img.fidx") {
			diskIndexFound = true
			imageIndexes = append(imageIndexes, PBSImageIndex{Filename: file.Filename, Size: file.Size, Checksum: "sha256:" + file.Checksum, CryptMode: file.CryptMode})
		}
	}
	if !qemuConfigFound || !diskIndexFound {
		return nil, nil, fmt.Errorf("decoded PBS index lacks QEMU config or disk image indexes")
	}
	sort.Slice(imageIndexes, func(i, j int) bool { return imageIndexes[i].Filename < imageIndexes[j].Filename })
	return &index, imageIndexes, nil
}

func loadPBSQEMUConfig(path string, imageIndexes []PBSImageIndex) ([]byte, map[string]string, error) {
	configBytes, err := os.ReadFile(path) // #nosec G304 -- operator-selected evidence path.
	if err != nil {
		return nil, nil, err
	}
	config, err := parsePVEQEMUConfig(string(configBytes))
	if err != nil {
		return nil, nil, err
	}
	if config["template"] != "1" || config["ciupgrade"] != "0" || config["name"] == "" || config["bios"] != "ovmf" || config["machine"] != "q35" || config["boot"] != "order=scsi0" || config["agent"] != "enabled=1" {
		return nil, nil, fmt.Errorf("restored QEMU config violates the Gentoo seed contract")
	}
	for _, image := range imageIndexes {
		drive := strings.TrimSuffix(strings.TrimPrefix(image.Filename, "drive-"), ".img.fidx")
		if !strings.Contains(string(configBytes), "#qmdump#map:"+drive+":") {
			return nil, nil, fmt.Errorf("QEMU config lacks qmdump mapping for %q", image.Filename)
		}
	}
	return configBytes, config, nil
}

type pbsRestoreEvidence struct {
	firstBoot     *evidenceContent
	runtime       *evidenceContent
	secondBoot    *evidenceContent
	cleanupDigest PBSEvidenceDigest
}

func loadPBSRestoreEvidence(request PBSAttestationRequest, vmid int, templateName string) (*pbsRestoreEvidence, error) {
	firstBoot, err := readRequiredEvidence(request.FirstBootLogPath, []string{"CLOUD_INIT_GATE=PASS", "IMPLICIT_EMERGE_SYNC=ABSENT", `"status": "done"`, `"errors": []`})
	if err != nil {
		return nil, fmt.Errorf("first boot gate: %w", err)
	}
	runtimeLog, err := readRequiredEvidence(request.RuntimeLogPath, []string{"CLOUD_INIT_LOCAL=active", "CLOUD_INIT_NETWORK=active", "CLOUD_CONFIG=active", "CLOUD_FINAL=active", "QEMU_GUEST_AGENT=active", "OS=Gentoo Linux", "KERNEL=", "PROFILE=", "ROOT_FS=", "Portage ", "/usr/bin/cloud-init", "/usr/bin/qemu-ga", "/usr/bin/emerge", "/usr/bin/python3"})
	if err != nil {
		return nil, fmt.Errorf("runtime gate: %w", err)
	}
	secondBoot, err := readRequiredEvidence(request.SecondCloudInitLogPath, []string{"SECOND_CLOUD_INIT_GATE=PASS", "SECOND_RUN_GATE=PASS", `"status": "done"`, `"errors": []`, "\nactive\n"})
	if err != nil {
		return nil, fmt.Errorf("second cloud-init gate: %w", err)
	}
	if strings.Contains(firstBoot.content, "IMPLICIT_EMERGE_SYNC=FOUND") || strings.Contains(firstBoot.content, "emerge --quiet --sync") || strings.Contains(secondBoot.content, "IMPLICIT_EMERGE_SYNC=FOUND") || strings.Contains(secondBoot.content, "emerge --quiet --sync") {
		return nil, fmt.Errorf("cloud-init evidence contains an implicit emerge sync")
	}
	var cleanup pveCleanupInput
	if err := decodeStrictFile(request.CleanupPath, &cleanup); err != nil {
		return nil, fmt.Errorf("load PVE cleanup evidence: %w", err)
	}
	if len(cleanup.TemporaryVMIDsRemaining) != 0 || len(cleanup.Source) != 1 || cleanup.Source[0].VMID != vmid || cleanup.Source[0].Name != templateName || cleanup.Source[0].Status != "stopped" || cleanup.Source[0].Template != 1 {
		return nil, fmt.Errorf("PVE cleanup evidence does not preserve only the stopped source template")
	}
	cleanupDigest, err := digestEvidence(request.CleanupPath)
	if err != nil {
		return nil, err
	}
	return &pbsRestoreEvidence{
		firstBoot: firstBoot, runtime: runtimeLog, secondBoot: secondBoot, cleanupDigest: cleanupDigest,
	}, nil
}

func LoadPBSSourceAttestation(path string) (*PBSSourceAttestation, error) {
	var attestation PBSSourceAttestation
	if err := decodeStrictFile(path, &attestation); err != nil {
		return nil, err
	}
	if err := attestation.Validate(); err != nil {
		return nil, err
	}
	return &attestation, nil
}

// StampPVESourceAttestation binds a stopped source template to a validated PBS
// attestation digest. Credentials are used only for the live API calls.
func StampPVESourceAttestation(ctx context.Context, common *CommonConfig, attestationPath, username, token string, now time.Time) (*PBSSourceStampEvidence, error) {
	if common == nil || username == "" || token == "" {
		return nil, fmt.Errorf("PVE common config and token credentials are required")
	}
	attestation, err := LoadPBSSourceAttestation(attestationPath)
	if err != nil {
		return nil, fmt.Errorf("load PBS source attestation: %w", err)
	}
	digest, err := digestFile(attestationPath)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(common.ProxmoxURL, "/")
	vmid := attestation.Template.VMID
	statusURL := fmt.Sprintf("%s/nodes/%s/qemu/%d/status/current", base, url.PathEscape(common.ProxmoxNode), vmid)
	var statusPayload struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := getPVEJSON(ctx, common, statusURL, username, token, &statusPayload); err != nil {
		return nil, fmt.Errorf("query PVE source status: %w", err)
	}
	if statusPayload.Data.Status != "stopped" {
		return nil, fmt.Errorf("PVE source VMID %d must be stopped before stamping", vmid)
	}
	configURL := fmt.Sprintf("%s/nodes/%s/qemu/%d/config", base, url.PathEscape(common.ProxmoxNode), vmid)
	type configPayload struct {
		Data struct {
			Name        string      `json:"name"`
			Template    json.Number `json:"template"`
			CIUpgrade   json.Number `json:"ciupgrade"`
			Description string      `json:"description"`
		} `json:"data"`
	}
	var before configPayload
	if err := getPVEJSON(ctx, common, configURL, username, token, &before); err != nil {
		return nil, fmt.Errorf("query PVE source config: %w", err)
	}
	if before.Data.Name != attestation.Template.Name || before.Data.Template.String() != "1" || before.Data.CIUpgrade.String() != "0" {
		return nil, fmt.Errorf("PVE source VMID %d does not match the attested template contract", vmid)
	}
	markerDigest := "sha256:" + digest
	description := fmt.Sprintf("Portage Engine PBS source %s | portage-engine-provenance=%s", attestation.Snapshot, markerDigest)
	form := url.Values{"description": []string{description}, "ciupgrade": []string{"0"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, configURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+username+"="+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := pveClient(common).Do(req)
	if err != nil {
		return nil, fmt.Errorf("stamp PVE PBS source: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("stamp PVE PBS source: HTTP %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	var after configPayload
	if err := getPVEJSON(ctx, common, configURL, username, token, &after); err != nil {
		return nil, fmt.Errorf("verify PVE PBS source stamp: %w", err)
	}
	if after.Data.Name != attestation.Template.Name || after.Data.Template.String() != "1" || after.Data.CIUpgrade.String() != "0" || after.Data.Description != description {
		return nil, fmt.Errorf("PVE PBS source stamp read-back mismatch")
	}
	return &PBSSourceStampEvidence{SchemaVersion: 1, StampedAt: now.UTC(), VMID: vmid, Node: common.ProxmoxNode, Template: attestation.Template.Name,
		Snapshot: attestation.Snapshot, AttestationDigest: markerDigest, CIUpgrade: 0, Verified: true}, nil
}

func getPVEJSON(ctx context.Context, common *CommonConfig, endpoint, username, token string, destination any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+username+"="+token)
	resp, err := pveClient(common).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return nil
}

func (a *PBSSourceAttestation) Validate() error {
	if a == nil {
		return fmt.Errorf("invalid PBS source attestation identity")
	}
	if err := a.validateIdentity(); err != nil {
		return err
	}
	vmid, err := a.validateTemplateContract()
	if err != nil {
		return err
	}
	if err := a.validateEvidenceContract(vmid); err != nil {
		return err
	}
	return a.validateImageIndexes()
}

func (a *PBSSourceAttestation) validateIdentity() error {
	parsedURL, urlErr := url.Parse(a.PBSURL)
	if a.SchemaVersion != 1 || a.CreatedAt.IsZero() || urlErr != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" || parsedURL.Path != "" || !pbsFingerprintPattern.MatchString(a.CertificateFingerprint) || !repoComponentPattern.MatchString(a.Datastore) || a.BackupType != "vm" || a.BackupTime <= 0 || a.Snapshot == "" || !pbsOwnerPattern.MatchString(a.Owner) || a.Comment == "" || !a.Protected || a.Verification.State != "ok" || !validVerifyUPID(a.Verification.UPID, a.Owner) {
		return fmt.Errorf("invalid PBS source attestation identity")
	}
	return nil
}

func (a *PBSSourceAttestation) validateTemplateContract() (int, error) {
	vmid, err := strconv.Atoi(a.BackupID)
	if err != nil || vmid != a.Template.VMID || a.Template.Name == "" || a.Template.Template != 1 || a.Template.CIUpgrade != 0 || a.Template.BIOS != "ovmf" || a.Template.Machine != "q35" || a.Template.Boot != "order=scsi0" || a.Template.Agent != "enabled=1" {
		return 0, fmt.Errorf("invalid PBS source template contract")
	}
	if a.Snapshot != fmt.Sprintf("vm/%s/%s", a.BackupID, time.Unix(a.BackupTime, 0).UTC().Format("2006-01-02T15:04:05Z")) || a.CreatedAt.Before(time.Unix(a.BackupTime, 0).UTC()) {
		return 0, fmt.Errorf("PBS snapshot identity is inconsistent")
	}
	return vmid, nil
}

func (a *PBSSourceAttestation) validateEvidenceContract(vmid int) error {
	for _, evidence := range []PBSEvidenceDigest{a.DecodedIndex, a.QEMUConfig, a.RestoreGate.FirstBootEvidence, a.RestoreGate.RuntimeEvidence, a.RestoreGate.SecondCloudInitEvidence, a.RestoreGate.CleanupEvidence} {
		if evidence.Size <= 0 || !prefixedSHA256Pattern.MatchString(evidence.SHA256) {
			return fmt.Errorf("PBS attestation contains invalid evidence digest")
		}
	}
	if len(a.ImageIndexes) == 0 || a.RestoreGate.RestoreVMID < 100 || a.RestoreGate.SmokeVMID < 100 || a.RestoreGate.RestoreVMID == a.RestoreGate.SmokeVMID || a.RestoreGate.RestoreVMID == vmid || a.RestoreGate.SmokeVMID == vmid || a.RestoreGate.CloudInitRuns != 2 || a.RestoreGate.ImplicitEmergeSync || !a.RestoreGate.TemporaryVMsDestroyed || a.RestoreGate.OS != "Gentoo Linux" || a.RestoreGate.Kernel == "" || a.RestoreGate.Profile == "" || a.RestoreGate.RootFS == "" {
		return fmt.Errorf("PBS restore gate is incomplete")
	}
	return nil
}

func (a *PBSSourceAttestation) validateImageIndexes() error {
	seen := map[string]struct{}{}
	for _, image := range a.ImageIndexes {
		if !strings.HasSuffix(image.Filename, ".img.fidx") || image.Size <= 0 || !prefixedSHA256Pattern.MatchString(image.Checksum) || image.CryptMode != "none" {
			return fmt.Errorf("invalid PBS image index %q", image.Filename)
		}
		if _, duplicate := seen[image.Filename]; duplicate {
			return fmt.Errorf("duplicate PBS image index %q", image.Filename)
		}
		seen[image.Filename] = struct{}{}
	}
	return nil
}

func (a *PBSSourceAttestation) ValidateForPlan(plan *BuildPlan) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if plan == nil {
		return fmt.Errorf("PBS source attestation does not match BuildPlan source")
	}
	expectedProfile := plan.ProfileRepository + ":" + plan.ProfilePath
	profileMatches := a.RestoreGate.Profile == expectedProfile || (plan.ProfileRepository == "gentoo" && a.RestoreGate.Profile == plan.ProfilePath)
	if plan.RootfsSource != "approved-pbs-snapshot" || plan.SourceVMID != a.Template.VMID || plan.SourceTemplate != a.Template.Name || !profileMatches {
		return fmt.Errorf("PBS source attestation does not match BuildPlan source")
	}
	return nil
}

type evidenceContent struct {
	content string
	digest  PBSEvidenceDigest
}

func readRequiredEvidence(path string, markers []string) (*evidenceContent, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-selected evidence path.
	if err != nil {
		return nil, err
	}
	content := strings.ReplaceAll(string(data), "\r", "")
	for _, marker := range markers {
		if !strings.Contains(content, marker) {
			return nil, fmt.Errorf("missing marker %q", marker)
		}
	}
	digest, err := digestEvidence(path)
	if err != nil {
		return nil, err
	}
	return &evidenceContent{content: content, digest: digest}, nil
}

func digestEvidence(path string) (PBSEvidenceDigest, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return PBSEvidenceDigest{}, fmt.Errorf("evidence path %q is not a non-empty regular file", path)
	}
	digest, err := digestFile(path)
	if err != nil {
		return PBSEvidenceDigest{}, err
	}
	return PBSEvidenceDigest{SHA256: "sha256:" + digest, Size: info.Size()}, nil
}

func parsePVEQEMUConfig(content string) (map[string]string, error) {
	values := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 || parts[0] == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("invalid QEMU config line %q", line)
		}
		if _, duplicate := values[parts[0]]; duplicate {
			return nil, fmt.Errorf("duplicate QEMU config key %q", parts[0])
		}
		values[parts[0]] = strings.TrimSpace(parts[1])
	}
	return values, nil
}

func validVerifyUPID(upid, owner string) bool {
	return pbsOwnerPattern.MatchString(owner) && strings.HasPrefix(upid, "UPID:") && strings.Contains(upid, ":verify_snapshot:") && strings.HasSuffix(upid, ":"+owner+":")
}

func validSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func evidenceValue(content, prefix string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}
