package distcc

import (
	"strings"
	"testing"
	"time"
)

func gateDigest(char string) string { return "sha256:" + strings.Repeat(char, 64) }

func validGateManifests() (GateManifest, GateManifest) {
	local := GateManifest{
		SchemaVersion: 1, Mode: "local-only", BuildInputDigest: gateDigest("a"),
		ArtifactDigest: gateDigest("b"), ABIDigest: gateDigest("c"),
		Install: InstallEvidence{Passed: true, EvidenceDigest: gateDigest("d")},
		GUI:     []GUIEvidence{{ScenarioID: "desktop/app-smoke", Passed: true, EvidenceDigest: gateDigest("e")}},
	}
	remote := local
	remote.GUI = append([]GUIEvidence(nil), local.GUI...)
	remote.Mode = "distcc"
	remote.DistCC = &DistCCGateEvidence{PoolID: "distcc-abcd", OutputFenced: true}
	return local, remote
}

func TestCompareGateRequiresExactArtifactABIInstallAndGUI(t *testing.T) {
	local, remote := validGateManifests()
	receipt, err := CompareGate(local, remote, time.Unix(123, 0))
	if err != nil || receipt.State != "passed" || len(receipt.GUIScenarios) != 1 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	checks := []func(*GateManifest){
		func(m *GateManifest) { m.ArtifactDigest = gateDigest("f") },
		func(m *GateManifest) { m.ABIDigest = gateDigest("f") },
		func(m *GateManifest) { m.Install.EvidenceDigest = gateDigest("f") },
		func(m *GateManifest) { m.GUI[0].EvidenceDigest = gateDigest("f") },
		func(m *GateManifest) { m.DistCC.PumpMode = true },
		func(m *GateManifest) { m.DistCC.OutputFenced = false },
	}
	for index, mutate := range checks {
		_, candidate := validGateManifests()
		mutate(&candidate)
		if _, err := CompareGate(local, candidate, time.Now()); err == nil {
			t.Fatalf("mismatch %d passed", index)
		}
	}
}
