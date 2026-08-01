package distcc

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"time"
)

var sha256Pattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// GateManifest is produced by real local-only or distcc test runs. This
// package compares evidence; it never synthesizes build, install, or GUI
// results when distccd/PVE is unavailable.
type GateManifest struct {
	SchemaVersion    int                 `json:"schema_version"`
	Mode             string              `json:"mode"`
	BuildInputDigest string              `json:"build_input_digest"`
	ArtifactDigest   string              `json:"artifact_digest"`
	ABIDigest        string              `json:"abi_digest"`
	Install          InstallEvidence     `json:"install"`
	GUI              []GUIEvidence       `json:"gui"`
	DistCC           *DistCCGateEvidence `json:"distcc,omitempty"`
}

type InstallEvidence struct {
	Passed         bool   `json:"passed"`
	EvidenceDigest string `json:"evidence_digest"`
}

type GUIEvidence struct {
	ScenarioID     string `json:"scenario_id"`
	Passed         bool   `json:"passed"`
	EvidenceDigest string `json:"evidence_digest"`
}

type DistCCGateEvidence struct {
	PoolID       string `json:"pool_id"`
	PumpMode     bool   `json:"pump_mode"`
	OutputFenced bool   `json:"output_fenced"`
}

type GateReceipt struct {
	SchemaVersion    int       `json:"schema_version"`
	State            string    `json:"state"`
	BuildInputDigest string    `json:"build_input_digest"`
	ArtifactDigest   string    `json:"artifact_digest"`
	ABIDigest        string    `json:"abi_digest"`
	GUIScenarios     []string  `json:"gui_scenarios"`
	ComparedAt       time.Time `json:"compared_at"`
}

func (m GateManifest) Validate() error {
	if m.SchemaVersion != 1 || (m.Mode != "local-only" && m.Mode != "distcc") {
		return fmt.Errorf("unsupported distcc gate manifest")
	}
	for name, digest := range map[string]string{
		"build input": m.BuildInputDigest, "artifact": m.ArtifactDigest,
		"ABI": m.ABIDigest, "install evidence": m.Install.EvidenceDigest,
	} {
		if !sha256Pattern.MatchString(digest) {
			return fmt.Errorf("invalid %s digest", name)
		}
	}
	if !m.Install.Passed || len(m.GUI) == 0 {
		return fmt.Errorf("install and at least one GUI gate must have passed")
	}
	seen := make(map[string]struct{}, len(m.GUI))
	for _, gui := range m.GUI {
		if gui.ScenarioID == "" || !gui.Passed || !sha256Pattern.MatchString(gui.EvidenceDigest) {
			return fmt.Errorf("invalid or failed GUI evidence")
		}
		if _, exists := seen[gui.ScenarioID]; exists {
			return fmt.Errorf("duplicate GUI scenario %q", gui.ScenarioID)
		}
		seen[gui.ScenarioID] = struct{}{}
	}
	if m.Mode == "local-only" && m.DistCC != nil {
		return fmt.Errorf("local-only evidence contains a distcc claim")
	}
	if m.Mode == "distcc" && (m.DistCC == nil || m.DistCC.PoolID == "" ||
		m.DistCC.PumpMode || !m.DistCC.OutputFenced) {
		return fmt.Errorf("distcc evidence must identify a fenced pool with pump disabled")
	}
	return nil
}

// CompareGate requires reproducible artifact/ABI identity plus equivalent
// installation and GUI evidence for the same immutable build input.
func CompareGate(local, remote GateManifest, now time.Time) (*GateReceipt, error) {
	if err := local.Validate(); err != nil {
		return nil, fmt.Errorf("local-only evidence: %w", err)
	}
	if err := remote.Validate(); err != nil {
		return nil, fmt.Errorf("distcc evidence: %w", err)
	}
	if local.Mode != "local-only" || remote.Mode != "distcc" {
		return nil, fmt.Errorf("gate requires local-only then distcc evidence")
	}
	if local.BuildInputDigest != remote.BuildInputDigest {
		return nil, fmt.Errorf("build input digest mismatch")
	}
	if local.ArtifactDigest != remote.ArtifactDigest {
		return nil, fmt.Errorf("artifact digest mismatch")
	}
	if local.ABIDigest != remote.ABIDigest {
		return nil, fmt.Errorf("ABI digest mismatch")
	}
	localGUI := make(map[string]string, len(local.GUI))
	for _, gui := range local.GUI {
		localGUI[gui.ScenarioID] = gui.EvidenceDigest
	}
	scenarios := make([]string, 0, len(remote.GUI))
	for _, gui := range remote.GUI {
		if localGUI[gui.ScenarioID] != gui.EvidenceDigest {
			return nil, fmt.Errorf("GUI evidence mismatch for scenario %q", gui.ScenarioID)
		}
		delete(localGUI, gui.ScenarioID)
		scenarios = append(scenarios, gui.ScenarioID)
	}
	if len(localGUI) != 0 {
		return nil, fmt.Errorf("distcc evidence omitted a local-only GUI scenario")
	}
	if local.Install.EvidenceDigest != remote.Install.EvidenceDigest {
		return nil, fmt.Errorf("install evidence mismatch")
	}
	sort.Strings(scenarios)
	return &GateReceipt{
		SchemaVersion: 1, State: "passed",
		BuildInputDigest: local.BuildInputDigest,
		ArtifactDigest:   local.ArtifactDigest, ABIDigest: local.ABIDigest,
		GUIScenarios: scenarios, ComparedAt: now.UTC(),
	}, nil
}

func ReadGateManifest(path string) (GateManifest, error) {
	file, err := os.Open(path) // #nosec G304 -- operator-selected local evidence path.
	if err != nil {
		return GateManifest{}, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var manifest GateManifest
	if err := decoder.Decode(&manifest); err != nil {
		return GateManifest{}, fmt.Errorf("decode gate manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return GateManifest{}, fmt.Errorf("decode gate manifest: trailing content")
	}
	return manifest, manifest.Validate()
}
