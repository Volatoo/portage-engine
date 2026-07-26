package imagefactory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type pbsAttestationFixture struct {
	request      PBSAttestationRequest
	snapshotPath string
	indexPath    string
}

func newPBSAttestationFixture(t *testing.T, protected bool) pbsAttestationFixture {
	t.Helper()
	dir := t.TempDir()
	backupTime := int64(1784735072)
	owner := "pve-backup@pbs!pve"
	verification := PBSVerification{State: "ok", UPID: "UPID:pbs:1:2:3:4:verify_snapshot:store-vm-9200:" + owner + ":"}
	snapshot := pbsSnapshotInput{BackupType: "vm", BackupID: "9200", BackupTime: backupTime, Owner: owner,
		Comment: "validated seed", Verification: verification, Protected: protected, Size: 1569,
		Files: []pbsSnapshotFile{{Filename: "qemu-server.conf.blob", CryptMode: "none", Size: 545}, {Filename: "drive-scsi0.img.fidx", CryptMode: "none", Size: 1024}, {Filename: "index.json.blob", CryptMode: "none", Size: 500}, {Filename: "client.log.blob"}}}
	index := pbsIndexInput{BackupID: "9200", BackupTime: backupTime, BackupType: "vm", Signature: json.RawMessage("null"),
		Files: []pbsIndexFile{{CryptMode: "none", Checksum: strings.Repeat("a", 64), Filename: "qemu-server.conf.blob", Size: 545}, {CryptMode: "none", Checksum: strings.Repeat("b", 64), Filename: "drive-scsi0.img.fidx", Size: 1024}}}
	index.Unprotected.ChunkUploadStats = json.RawMessage(`{}`)
	index.Unprotected.Notes = snapshot.Comment
	index.Unprotected.VerifyState = verification
	snapshotPath := writeTestJSON(t, dir, "snapshot.json", snapshot)
	indexPath := writeTestJSON(t, dir, "index.json", index)
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	config := "agent: enabled=1\nbios: ovmf\nboot: order=scsi0\nciupgrade: 0\nmachine: q35\nname: gentoo-cloudinit-template\nscsi0: ceph:base-9200,size=30G\ntemplate: 1\n#qmdump#map:scsi0:drive-scsi0:ceph:raw:\n"
	first := "CLOUD_INIT_GATE=PASS\n{\n  \"status\": \"done\",\n  \"errors\": []\n}\nIMPLICIT_EMERGE_SYNC=ABSENT\n"
	runtime := "CLOUD_INIT_LOCAL=active\nCLOUD_INIT_NETWORK=active\nCLOUD_CONFIG=active\nCLOUD_FINAL=active\nQEMU_GUEST_AGENT=active\nOS=Gentoo Linux\nKERNEL=6.18.38\nPROFILE=default/linux/amd64/23.0/no-multilib/systemd\nROOT_FS=xfs /dev/sda2\nPortage 3.0.81.2\n/usr/bin/cloud-init\n/usr/bin/qemu-ga\n/usr/bin/emerge\n/usr/bin/python3\n"
	second := "SECOND_CLOUD_INIT_GATE=PASS\n{\n  \"status\": \"done\",\n  \"errors\": []\n}\nactive\nSECOND_RUN_GATE=PASS\n"
	cleanup := pveCleanupInput{TemporaryVMIDsRemaining: []int{}, Source: []struct {
		VMID     int    `json:"vmid"`
		Name     string `json:"name"`
		Status   string `json:"status"`
		Template int    `json:"template"`
		Node     string `json:"node"`
	}{{VMID: 9200, Name: "gentoo-cloudinit-template", Status: "stopped", Template: 1, Node: "pve01"}}}
	cleanupPath := writeTestJSON(t, dir, "cleanup.json", cleanup)
	return pbsAttestationFixture{snapshotPath: snapshotPath, indexPath: indexPath, request: PBSAttestationRequest{
		PBSURL: "https://pbs.internal:8007", CertificateFingerprint: strings.Repeat("aa:", 31) + "aa", Datastore: "portage-engine",
		SnapshotPath: snapshotPath, IndexPath: indexPath, QEMUConfigPath: write("qemu-server.conf", config),
		FirstBootLogPath: write("first.log", first), RuntimeLogPath: write("runtime.log", runtime), SecondCloudInitLogPath: write("second.log", second), CleanupPath: cleanupPath,
		RestoreVMID: 9300, SmokeVMID: 9401,
	}}
}

func TestCreatePBSSourceAttestation(t *testing.T) {
	fixture := newPBSAttestationFixture(t, true)
	attestation, err := CreatePBSSourceAttestation(fixture.request, time.Unix(1784736000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := attestation.Validate(); err != nil {
		t.Fatal(err)
	}
	if attestation.Snapshot != "vm/9200/2026-07-22T15:44:32Z" || attestation.Template.CIUpgrade != 0 || len(attestation.ImageIndexes) != 1 || attestation.ImageIndexes[0].Checksum != "sha256:"+strings.Repeat("b", 64) || attestation.RestoreGate.Profile == "" {
		t.Fatalf("unexpected attestation: %+v", attestation)
	}
	plan := testBuildPlan()
	plan.RootfsSource = "approved-pbs-snapshot"
	plan.SourceVMID = 9200
	plan.SourceTemplate = "gentoo-cloudinit-template"
	plan.SourceProvenanceObjectID = "pbs-source/9200"
	plan.ProfilePath = "default/linux/amd64/23.0/no-multilib/systemd"
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if kind := expectedSourceObjectKind(&plan); kind != "pbs-source-attestation" {
		t.Fatalf("unexpected PBS source object kind %q", kind)
	}
	if err := attestation.ValidateForPlan(&plan); err != nil {
		t.Fatal(err)
	}
	plan.ProfilePath = "default/linux/amd64/23.0/systemd"
	if err := attestation.ValidateForPlan(&plan); err == nil {
		t.Fatal("PBS source profile drift was accepted")
	}
}

func TestCreatePBSSourceAttestationRejectsUnsafeEvidence(t *testing.T) {
	t.Run("unprotected snapshot", func(t *testing.T) {
		fixture := newPBSAttestationFixture(t, false)
		if _, err := CreatePBSSourceAttestation(fixture.request, time.Unix(1784736000, 0)); err == nil || !strings.Contains(err.Error(), "protected") {
			t.Fatalf("accepted unprotected snapshot: %v", err)
		}
	})
	t.Run("implicit emerge sync", func(t *testing.T) {
		fixture := newPBSAttestationFixture(t, true)
		file, err := os.OpenFile(fixture.request.FirstBootLogPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.WriteString("emerge --quiet --sync\n")
		_ = file.Close()
		if _, err := CreatePBSSourceAttestation(fixture.request, time.Unix(1784736000, 0)); err == nil || !strings.Contains(err.Error(), "emerge sync") {
			t.Fatalf("accepted implicit emerge sync: %v", err)
		}
	})
	t.Run("index identity drift", func(t *testing.T) {
		fixture := newPBSAttestationFixture(t, true)
		var index pbsIndexInput
		if err := decodeStrictFile(fixture.indexPath, &index); err != nil {
			t.Fatal(err)
		}
		index.BackupTime++
		_ = writeTestJSON(t, filepath.Dir(fixture.indexPath), filepath.Base(fixture.indexPath), index)
		if _, err := CreatePBSSourceAttestation(fixture.request, time.Unix(1784736000, 0)); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("accepted drifted PBS index: %v", err)
		}
	})
}

func TestPreflightRejectsSemanticallyInvalidPBSSourceAttestation(t *testing.T) {
	fixture := newPBSAttestationFixture(t, true)
	attestation, err := CreatePBSSourceAttestation(fixture.request, time.Unix(1784736000, 0))
	if err != nil {
		t.Fatal(err)
	}
	attestation.Protected = false
	dir := t.TempDir()
	path := writeTestJSON(t, dir, "source.json", attestation)
	object := objectForFile(t, "pbs-source/test", "pbs-source-attestation", path, []string{"base-systemd"})
	lock := testLock(object)
	report, err := Preflight(dir, lock, "base-systemd")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 || !strings.Contains(report.Missing[0].Reason, "invalid PBS source attestation") {
		t.Fatalf("semantic attestation drift was not reported: %+v", report.Missing)
	}
}

func TestStampPVESourceAttestationWritesAndReadsBackDigest(t *testing.T) {
	fixture := newPBSAttestationFixture(t, true)
	attestation, err := CreatePBSSourceAttestation(fixture.request, time.Unix(1784736000, 0))
	if err != nil {
		t.Fatal(err)
	}
	attestationPath := writeTestJSON(t, t.TempDir(), "attestation.json", attestation)
	stampedDescription := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=packer@pve!factory=secret" {
			t.Errorf("unexpected authorization header")
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status/current"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = fmt.Fprintf(w, `{"data":{"name":"gentoo-cloudinit-template","template":1,"ciupgrade":0,"description":%q}}`, stampedDescription)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/config"):
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("ciupgrade") != "0" {
				t.Error("source stamp did not preserve ciupgrade=0")
			}
			stampedDescription = r.Form.Get("description")
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	common := &CommonConfig{ProxmoxURL: server.URL + "/api2/json", ProxmoxNode: "pve01", ProxmoxInsecure: true}
	evidence, err := StampPVESourceAttestation(context.Background(), common, attestationPath, "packer@pve!factory", "secret", time.Unix(1784737000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.Verified || evidence.VMID != 9200 || !prefixedSHA256Pattern.MatchString(evidence.AttestationDigest) || !strings.Contains(stampedDescription, evidence.AttestationDigest) {
		t.Fatalf("unexpected source stamp evidence: %+v", evidence)
	}
}

func TestStampPVESourceAttestationRejectsRunningSource(t *testing.T) {
	fixture := newPBSAttestationFixture(t, true)
	attestation, err := CreatePBSSourceAttestation(fixture.request, time.Unix(1784736000, 0))
	if err != nil {
		t.Fatal(err)
	}
	attestationPath := writeTestJSON(t, t.TempDir(), "attestation.json", attestation)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
	}))
	defer server.Close()
	common := &CommonConfig{ProxmoxURL: server.URL + "/api2/json", ProxmoxNode: "pve01", ProxmoxInsecure: true}
	if _, err := StampPVESourceAttestation(context.Background(), common, attestationPath, "packer@pve!factory", "secret", time.Now()); err == nil || !strings.Contains(err.Error(), "must be stopped") {
		t.Fatalf("accepted running source template: %v", err)
	}
}
