package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/iac"
	"github.com/slchris/portage-engine/internal/imagefactory"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	commands := map[string]func([]string){
		"preflight":         runPreflight,
		"lock-materialize":  runLockMaterialize,
		"plan":              runPlan,
		"source-check":      runSourceCheck,
		"pbs-attest":        runPBSAttest,
		"pbs-stamp-source":  runPBSStampSource,
		"manifest":          runManifest,
		"catalog-assemble":  runCatalogAssemble,
		"smoke-config":      runSmokeConfig,
		"guest-ip":          runGuestIP,
		"guest-host-key":    runGuestHostKey,
		"stamp-output":      runStampOutput,
		"catalyst-plan":     runCatalystPlan,
		"catalyst-gate":     runCatalystGate,
		"catalyst-manifest": runCatalystManifest,
		"qcow2-manifest":    runQCOW2Manifest,
		"qcow2-check":       runQCOW2Check,
		"ops-keygen":        runOpsKeygen,
		"bundle-seal":       runBundleSeal,
		"bundle-verify":     runBundleVerify,
		"ops-digest":        runOpsDigest,
		"promote":           runPromote,
		"rollback":          runRollback,
		"cleanup-plan":      runCleanupPlan,
		"state-sign":        runStateSign,
		"rebuild-plan":      runRebuildPlan,
		"status-compile":    runStatusCompile,
		"package-sets":      runPackageSets,
	}
	command, ok := commands[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	command(os.Args[2:])
}

func runPackageSets(args []string) {
	fs := flag.NewFlagSet("package-sets", flag.ExitOnError)
	catalogPath := fs.String("catalog", "", "Locked package-set catalog JSON")
	setsValue := fs.String("sets", "", "Comma-separated package-set IDs")
	extrasValue := fs.String("extras", "", "Optional comma-separated target-only package atoms")
	output := fs.String("output", "", "Output newline-delimited package atoms")
	_ = fs.Parse(args)
	if *catalogPath == "" || *setsValue == "" || *output == "" {
		log.Fatal("package-sets requires -catalog, -sets, and -output")
	}
	split := func(value string) []string {
		if value == "" {
			return nil
		}
		parts := strings.Split(value, ",")
		for index := range parts {
			parts[index] = strings.TrimSpace(parts[index])
			if parts[index] == "" {
				log.Fatal("package-sets contains an empty ID or atom")
			}
		}
		return parts
	}
	catalog, err := imagefactory.LoadPackageSetCatalog(*catalogPath)
	if err != nil {
		log.Fatal(err)
	}
	packages, err := catalog.Resolve(split(*setsValue), split(*extrasValue))
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteTextAtomic(*output, strings.Join(packages, "\n")+"\n"); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("resolved %d packages from %s\n", len(packages), *setsValue)
}

func runPBSAttest(args []string) {
	fs := flag.NewFlagSet("pbs-attest", flag.ExitOnError)
	pbsURL := fs.String("pbs-url", "", "PBS HTTPS origin")
	fingerprint := fs.String("fingerprint", "", "Pinned lowercase PBS SHA-256 certificate fingerprint")
	datastore := fs.String("datastore", "", "PBS datastore")
	snapshot := fs.String("snapshot", "", "PBS snapshot API evidence JSON")
	index := fs.String("index", "", "Decoded PBS index.json")
	qemuConfig := fs.String("qemu-config", "", "Decoded qemu-server.conf")
	firstBootLog := fs.String("first-boot-log", "", "First cloud-init boot gate log")
	runtimeLog := fs.String("runtime-log", "", "Restored guest runtime gate log")
	secondCloudInitLog := fs.String("second-cloud-init-log", "", "Second cloud-init gate log")
	cleanup := fs.String("cleanup", "", "PVE temporary-VM cleanup evidence JSON")
	restoreVMID := fs.Int("restore-vmid", 0, "Disposable restored template VMID")
	smokeVMID := fs.Int("smoke-vmid", 0, "Disposable boot smoke VMID")
	output := fs.String("output", "", "Output PBS source attestation JSON")
	_ = fs.Parse(args)
	if *pbsURL == "" || *fingerprint == "" || *datastore == "" || *snapshot == "" || *index == "" || *qemuConfig == "" || *firstBootLog == "" || *runtimeLog == "" || *secondCloudInitLog == "" || *cleanup == "" || *restoreVMID == 0 || *smokeVMID == 0 || *output == "" {
		log.Fatal("pbs-attest requires PBS identity, snapshot/index/config, restore gate logs, cleanup evidence, VMIDs, and output")
	}
	attestation, err := imagefactory.CreatePBSSourceAttestation(imagefactory.PBSAttestationRequest{
		PBSURL: *pbsURL, CertificateFingerprint: *fingerprint, Datastore: *datastore,
		SnapshotPath: *snapshot, IndexPath: *index, QEMUConfigPath: *qemuConfig,
		FirstBootLogPath: *firstBootLog, RuntimeLogPath: *runtimeLog, SecondCloudInitLogPath: *secondCloudInitLog, CleanupPath: *cleanup,
		RestoreVMID: *restoreVMID, SmokeVMID: *smokeVMID,
	}, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, attestation); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("attested protected PBS snapshot %s\n", attestation.Snapshot)
}

func runPBSStampSource(args []string) {
	fs := flag.NewFlagSet("pbs-stamp-source", flag.ExitOnError)
	commonPath := fs.String("common", "", "Site-local common config JSON")
	attestationPath := fs.String("attestation", "", "Validated PBS source attestation JSON")
	output := fs.String("output", "", "PVE source stamp evidence JSON")
	_ = fs.Parse(args)
	if *commonPath == "" || *attestationPath == "" || *output == "" {
		log.Fatal("pbs-stamp-source requires -common, -attestation, and -output")
	}
	common, err := imagefactory.LoadCommonConfig(*commonPath)
	if err != nil {
		log.Fatal(err)
	}
	evidence, err := imagefactory.StampPVESourceAttestation(context.Background(), common, *attestationPath,
		os.Getenv("PKR_VAR_proxmox_username"), os.Getenv("PKR_VAR_proxmox_token"), time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, evidence); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stamped PVE source VMID %d with %s\n", evidence.VMID, evidence.AttestationDigest)
}

func runCatalystPlan(args []string) {
	fs := flag.NewFlagSet("catalyst-plan", flag.ExitOnError)
	planPath := fs.String("plan", "", "Reviewed CatalystPlan JSON")
	lockPath := fs.String("lock", "", "Offline input lock JSON")
	offlineRoot := fs.String("root", "", "Verified offline root")
	workRoot := fs.String("work", "", "Fresh Catalyst work directory")
	output := fs.String("output", "", "Prepared runner/evidence JSON")
	_ = fs.Parse(args)
	if *planPath == "" || *lockPath == "" || *offlineRoot == "" || *workRoot == "" || *output == "" {
		log.Fatal("catalyst-plan requires -plan, -lock, -root, -work, and -output")
	}
	prepared, err := imagefactory.PrepareCatalystPlan(*planPath, *lockPath, *offlineRoot, *workRoot)
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, prepared); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("prepared locked Catalyst build %s\n", prepared.PlanDigest)
}

func runCatalystGate(args []string) {
	fs := flag.NewFlagSet("catalyst-gate", flag.ExitOnError)
	preparedPath := fs.String("prepared", "", "Prepared Catalyst evidence JSON")
	output := fs.String("output", "", "Execution gate evidence JSON")
	_ = fs.Parse(args)
	if *preparedPath == "" || *output == "" {
		log.Fatal("catalyst-gate requires -prepared and -output")
	}
	var prepared imagefactory.CatalystPrepared
	file, err := os.Open(*preparedPath) // #nosec G304 -- operator-selected evidence file.
	if err != nil {
		log.Fatal(err)
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&prepared); err != nil {
		_ = file.Close()
		log.Fatal(err)
	}
	if err := file.Close(); err != nil {
		log.Fatal(err)
	}
	if prepared.SchemaVersion != 1 || prepared.Target == "" || prepared.PlanDigest == "" || prepared.InputLockDigest == "" {
		log.Fatal("prepared Catalyst evidence is incomplete")
	}
	if err := imagefactory.WriteJSONAtomic(*output, imagefactory.NewCatalystGateEvidence(&prepared, time.Now())); err != nil {
		log.Fatal(err)
	}
	fmt.Println("recorded Catalyst signature and network-deny gate")
}

func runCatalystManifest(args []string) {
	fs := flag.NewFlagSet("catalyst-manifest", flag.ExitOnError)
	planPath := fs.String("plan", "", "Reviewed CatalystPlan JSON")
	lockPath := fs.String("lock", "", "Offline input lock JSON")
	preparedPath := fs.String("prepared", "", "Prepared Catalyst evidence JSON")
	gatePath := fs.String("gate", "", "Execution gate evidence JSON")
	rootfsPath := fs.String("rootfs", "", "Exact Catalyst rootfs output")
	output := fs.String("output", "", "Rootfs manifest JSON")
	_ = fs.Parse(args)
	if *planPath == "" || *lockPath == "" || *preparedPath == "" || *gatePath == "" || *rootfsPath == "" || *output == "" {
		log.Fatal("catalyst-manifest requires -plan, -lock, -prepared, -gate, -rootfs, and -output")
	}
	manifest, err := imagefactory.GenerateCatalystRootfsManifest(*planPath, *lockPath, *preparedPath, *gatePath, *rootfsPath, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, manifest); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote rootfs manifest %s\n", manifest.RootfsDigest)
}

func runQCOW2Manifest(args []string) {
	fs := flag.NewFlagSet("qcow2-manifest", flag.ExitOnError)
	rootfsManifest := fs.String("rootfs-manifest", "", "Catalyst rootfs manifest JSON")
	qcow2Path := fs.String("qcow2", "", "Assembled QCOW2")
	assemblerPath := fs.String("assembler", "", "Disk assembler script")
	output := fs.String("output", "", "QCOW2 manifest JSON")
	_ = fs.Parse(args)
	if *rootfsManifest == "" || *qcow2Path == "" || *assemblerPath == "" || *output == "" {
		log.Fatal("qcow2-manifest requires -rootfs-manifest, -qcow2, -assembler, and -output")
	}
	manifest, err := imagefactory.GenerateQCOW2Manifest(*rootfsManifest, *qcow2Path, *assemblerPath, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, manifest); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote QCOW2 manifest %s\n", manifest.QCOW2Digest)
}

func runQCOW2Check(args []string) {
	fs := flag.NewFlagSet("qcow2-check", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "QCOW2 manifest JSON")
	qcow2Path := fs.String("qcow2", "", "QCOW2 artifact")
	_ = fs.Parse(args)
	if *manifestPath == "" || *qcow2Path == "" {
		log.Fatal("qcow2-check requires -manifest and -qcow2")
	}
	digest, err := imagefactory.VerifyQCOW2Artifact(*manifestPath, *qcow2Path)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("manifest_digest=%s\n", digest)
}

func runOpsKeygen(args []string) {
	fs := flag.NewFlagSet("ops-keygen", flag.ExitOnError)
	privatePath := fs.String("private", "", "New owner-only operations private key JSON")
	publicPath := fs.String("public", "", "New operations public key JSON")
	_ = fs.Parse(args)
	if *privatePath == "" || *publicPath == "" || *privatePath == *publicPath {
		log.Fatal("ops-keygen requires distinct -private and -public paths")
	}
	for _, path := range []string{*privatePath, *publicPath} {
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			log.Fatalf("refusing to overwrite key path %s", path)
		}
	}
	privateKey, publicKey, err := imagefactory.NewOperationsKeyPair()
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*privatePath, privateKey); err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*publicPath, publicKey); err != nil {
		_ = os.Remove(*privatePath)
		log.Fatal(err)
	}
	if err := os.Chmod(*privatePath, 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(*publicPath, 0o644); err != nil { // #nosec G302 -- this command explicitly exports a public signing key.
		log.Fatal(err)
	}
	fmt.Printf("generated operations key %s\n", publicKey.KeyID)
}

func runBundleSeal(args []string) {
	fs := flag.NewFlagSet("bundle-seal", flag.ExitOnError)
	lockPath := fs.String("lock", "", "Input lock JSON")
	root := fs.String("root", "", "Offline bundle root")
	privateKey := fs.String("private-key", "", "Sync-zone private signing key")
	freshHours := fs.Int("fresh-hours", 336, "Freshness window in hours")
	manifestOutput := fs.String("manifest-output", "", "Bundle manifest output")
	signatureOutput := fs.String("signature-output", "", "Detached signature output")
	_ = fs.Parse(args)
	if *lockPath == "" || *root == "" || *privateKey == "" || *manifestOutput == "" || *signatureOutput == "" || *manifestOutput == *signatureOutput {
		log.Fatal("bundle-seal requires -lock, -root, -private-key, -manifest-output, and -signature-output")
	}
	manifest, signature, err := imagefactory.SealBundle(*lockPath, *root, *privateKey, time.Now(), time.Duration(*freshHours)*time.Hour)
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*manifestOutput, manifest); err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*signatureOutput, signature); err != nil {
		log.Fatal(err)
	}
	digest, _ := imagefactory.CanonicalDigest(manifest)
	fmt.Printf("sealed bundle %s manifest_digest=%s\n", manifest.BundleID, digest)
}

func runBundleVerify(args []string) {
	fs := flag.NewFlagSet("bundle-verify", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "Signed bundle manifest")
	signaturePath := fs.String("signature", "", "Detached signature")
	publicKey := fs.String("public-key", "", "Trusted sync-zone public key")
	lockPath := fs.String("lock", "", "Input lock JSON")
	root := fs.String("root", "", "Offline bundle root")
	_ = fs.Parse(args)
	if *manifestPath == "" || *signaturePath == "" || *publicKey == "" || *lockPath == "" || *root == "" {
		log.Fatal("bundle-verify requires -manifest, -signature, -public-key, -lock, and -root")
	}
	manifest, err := imagefactory.VerifyBundle(*manifestPath, *signaturePath, *publicKey, *lockPath, *root, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("verified bundle %s fresh_until=%s\n", manifest.BundleID, manifest.FreshUntil.Format(time.RFC3339))
}

func runOpsDigest(args []string) {
	fs := flag.NewFlagSet("ops-digest", flag.ExitOnError)
	kind := fs.String("kind", "", "catalog or bundle")
	input := fs.String("input", "", "Strict JSON input")
	_ = fs.Parse(args)
	if *input == "" {
		log.Fatal("ops-digest requires -input")
	}
	var value any
	var err error
	switch *kind {
	case "catalog":
		value, err = catalog.Load(*input)
	case "bundle":
		value, err = imagefactory.LoadBundleManifest(*input)
	default:
		log.Fatal("ops-digest -kind must be catalog or bundle")
	}
	if err != nil {
		log.Fatal(err)
	}
	digest, err := imagefactory.CanonicalDigest(value)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(digest)
}

func runPromote(args []string) {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	candidateCatalog := fs.String("catalog", "", "Candidate catalog JSON")
	bundleManifest := fs.String("bundle-manifest", "", "Signed bundle manifest")
	bundleSignature := fs.String("bundle-signature", "", "Bundle signature")
	bundlePublicKey := fs.String("bundle-public-key", "", "Trusted sync-zone public key")
	lockPath := fs.String("lock", "", "Input lock JSON")
	offlineRoot := fs.String("offline-root", "", "Offline bundle root")
	evidenceRoot := fs.String("evidence-root", "", "Root containing promotion evidence")
	planPath := fs.String("plan", "", "Promotion plan JSON")
	releasePrivateKey := fs.String("release-private-key", "", "Independent release signer private key")
	releasePublicKey := fs.String("release-public-key", "", "Independent release signer public key")
	currentAlias := fs.String("current-alias", "", "Existing signed alias envelope, if any")
	outputRoot := fs.String("output-root", "", "Immutable releases directory")
	aliasOutput := fs.String("alias-output", "", "Mutable signed alias envelope")
	_ = fs.Parse(args)
	if *candidateCatalog == "" || *bundlePublicKey == "" || *evidenceRoot == "" || *planPath == "" || *releasePrivateKey == "" || *releasePublicKey == "" || *outputRoot == "" || *aliasOutput == "" {
		log.Fatal("promote requires catalog, trusted sync public key, evidence, plan, release-key, output-root, and alias arguments; legacy single-bundle plans also require bundle and lock arguments")
	}
	result, err := imagefactory.PromoteRelease(*candidateCatalog, *bundleManifest, *bundleSignature, *bundlePublicKey, *lockPath, *offlineRoot,
		*evidenceRoot, *planPath, *releasePrivateKey, *releasePublicKey, *currentAlias, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	releaseDir := filepath.Join(*outputRoot, result.Receipt.ReleaseID)
	aliasPath, err := filepath.Abs(*aliasOutput)
	if err != nil {
		log.Fatal(err)
	}
	releasePath, err := filepath.Abs(releaseDir)
	if err != nil {
		log.Fatal(err)
	}
	relativeAlias, err := filepath.Rel(releasePath, aliasPath)
	if err != nil {
		log.Fatal(err)
	}
	if relativeAlias == "." || (relativeAlias != ".." && !strings.HasPrefix(relativeAlias, ".."+string(filepath.Separator))) {
		log.Fatal("alias output must be outside the immutable release directory")
	}
	if _, err := os.Lstat(releaseDir); err == nil || !os.IsNotExist(err) {
		log.Fatalf("refusing to overwrite release directory %s", releaseDir)
	}
	if err := os.MkdirAll(filepath.Dir(releaseDir), 0o750); err != nil {
		log.Fatal(err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(releaseDir), ".release-staging-")
	if err != nil {
		log.Fatal(err)
	}
	for name, value := range map[string]any{"catalog.json": result.Catalog, "promotion-receipt.json": result.Receipt, "promotion-receipt.sig.json": result.ReceiptSignature} {
		if err := imagefactory.WriteJSONAtomic(filepath.Join(staging, name), value); err != nil {
			_ = os.RemoveAll(staging)
			log.Fatal(err)
		}
	}
	if err := os.Rename(staging, releaseDir); err != nil {
		_ = os.RemoveAll(staging)
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*aliasOutput, result.AliasEnvelope); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("promoted %s and atomically updated alias %s revision %d\n", result.Receipt.ReleaseID, result.Alias.Alias, result.Alias.Revision)
}

func runRollback(args []string) {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	currentAlias := fs.String("current-alias", "", "Current signed alias envelope")
	targetReceipt := fs.String("target-receipt", "", "Target release promotion receipt")
	targetReceiptSignature := fs.String("target-receipt-signature", "", "Target receipt signature")
	targetCatalog := fs.String("target-catalog", "", "Target stable catalog")
	releasePrivateKey := fs.String("release-private-key", "", "Release signer private key")
	releasePublicKey := fs.String("release-public-key", "", "Release signer public key")
	reason := fs.String("reason", "", "Auditable rollback reason")
	aliasOutput := fs.String("alias-output", "", "Signed alias envelope output")
	_ = fs.Parse(args)
	if *currentAlias == "" || *targetReceipt == "" || *targetReceiptSignature == "" || *targetCatalog == "" || *releasePrivateKey == "" || *releasePublicKey == "" || *reason == "" || *aliasOutput == "" {
		log.Fatal("rollback requires current alias, target receipt/catalog, release keys, reason, and alias output")
	}
	envelope, err := imagefactory.RollbackRelease(*currentAlias, *targetReceipt, *targetReceiptSignature, *targetCatalog, *releasePrivateKey, *releasePublicKey, *reason, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*aliasOutput, envelope); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("rolled alias %s back to %s revision %d\n", envelope.Alias.Alias, envelope.Alias.ReleaseID, envelope.Alias.Revision)
}

func runCleanupPlan(args []string) {
	fs := flag.NewFlagSet("cleanup-plan", flag.ExitOnError)
	statePath := fs.String("signed-state", "", "Signed operations generation/lease state envelope")
	publicKey := fs.String("public-key", "", "Trusted state authority public key")
	output := fs.String("output", "", "Non-destructive cleanup plan output")
	_ = fs.Parse(args)
	if *statePath == "" || *publicKey == "" || *output == "" {
		log.Fatal("cleanup-plan requires -signed-state, -public-key, and -output")
	}
	state, err := imagefactory.LoadSignedOperationsState(*statePath, *publicKey)
	if err != nil {
		log.Fatal(err)
	}
	plan, err := imagefactory.PlanGenerationCleanup(state, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, plan); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote cleanup plan with %d generation decisions; no resources were deleted\n", len(plan.Decisions))
}

func runStateSign(args []string) {
	fs := flag.NewFlagSet("state-sign", flag.ExitOnError)
	statePath := fs.String("state", "", "Authoritative operations state JSON")
	privateKey := fs.String("private-key", "", "Lease/state authority private key")
	output := fs.String("output", "", "Signed operations state envelope")
	_ = fs.Parse(args)
	if *statePath == "" || *privateKey == "" || *output == "" {
		log.Fatal("state-sign requires -state, -private-key, and -output")
	}
	state, err := imagefactory.LoadOperationsState(*statePath)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := imagefactory.PlanGenerationCleanup(state, time.Now()); err != nil {
		log.Fatal(err)
	}
	signature, err := imagefactory.SignOperationsPayload("operations-state", state, *privateKey)
	if err != nil {
		log.Fatal(err)
	}
	envelope := imagefactory.SignedOperationsState{SchemaVersion: 1, State: *state, Signature: *signature}
	if err := imagefactory.WriteJSONAtomic(*output, &envelope); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("signed operations state with %d generations and %d leases\n", len(state.Generations), len(state.Leases))
}

func runRebuildPlan(args []string) {
	fs := flag.NewFlagSet("rebuild-plan", flag.ExitOnError)
	policyPath := fs.String("policy", "", "Periodic rebuild policy JSON")
	bundlePath := fs.String("bundle-manifest", "", "Verified bundle manifest JSON")
	output := fs.String("output", "", "Rebuild decisions output")
	_ = fs.Parse(args)
	if *policyPath == "" || *bundlePath == "" || *output == "" {
		log.Fatal("rebuild-plan requires -policy, -bundle-manifest, and -output")
	}
	policy, err := imagefactory.LoadRebuildPolicy(*policyPath)
	if err != nil {
		log.Fatal(err)
	}
	bundle, err := imagefactory.LoadBundleManifest(*bundlePath)
	if err != nil {
		log.Fatal(err)
	}
	plan, err := imagefactory.PlanRebuild(policy, bundle, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, plan); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote rebuild plan for %d profiles\n", len(plan.Decisions))
}

func runStampOutput(args []string) {
	fs := flag.NewFlagSet("stamp-output", flag.ExitOnError)
	commonPath := fs.String("common", "", "Site-local common config JSON")
	planPath := fs.String("plan", "", "Reviewed BuildPlan JSON")
	lockPath := fs.String("lock", "", "Offline input lock JSON")
	manifestPath := fs.String("manifest", "", "Smoke-tested candidate image manifest")
	evidenceOutput := fs.String("evidence-output", "", "Output stamp evidence JSON")
	_ = fs.Parse(args)
	if *commonPath == "" || *planPath == "" || *lockPath == "" || *manifestPath == "" || *evidenceOutput == "" {
		log.Fatal("stamp-output requires -common, -plan, -lock, -manifest, and -evidence-output")
	}
	common, err := imagefactory.LoadCommonConfig(*commonPath)
	if err != nil {
		log.Fatal(err)
	}
	manifest, err := imagefactory.LoadImageManifest(*manifestPath)
	if err != nil {
		log.Fatal(err)
	}
	plan, err := imagefactory.LoadBuildPlan(*planPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := manifest.ValidateForPlan(plan); err != nil {
		log.Fatal(err)
	}
	if err := manifest.ValidateEvidenceFiles(*commonPath, *planPath, *lockPath); err != nil {
		log.Fatal(err)
	}
	evidence, err := imagefactory.StampPVEOutput(context.Background(), common, manifest, *manifestPath,
		os.Getenv("PKR_VAR_proxmox_username"), os.Getenv("PKR_VAR_proxmox_token"))
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*evidenceOutput, evidence); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stamped PVE template %s with %s\n", evidence.Template, evidence.ManifestDigest)
}

func runSmokeConfig(args []string) {
	fs := flag.NewFlagSet("smoke-config", flag.ExitOnError)
	commonPath := fs.String("common", "", "Site-local common config JSON")
	planPath := fs.String("plan", "", "Reviewed BuildPlan JSON")
	manifestPath := fs.String("manifest", "", "Candidate image manifest JSON")
	lockPath := fs.String("lock", "", "Offline input lock JSON")
	instanceName := fs.String("name", "", "Disposable smoke VM name")
	output := fs.String("output", "", "Generated Terraform main.tf")
	_ = fs.Parse(args)
	if *commonPath == "" || *planPath == "" || *manifestPath == "" || *lockPath == "" || *instanceName == "" || *output == "" {
		log.Fatal("smoke-config requires -common, -plan, -manifest, -lock, -name, and -output")
	}
	if len(*instanceName) > 63 || strings.Trim(*instanceName, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
		log.Fatal("invalid smoke VM name")
	}
	common, err := imagefactory.LoadCommonConfig(*commonPath)
	if err != nil {
		log.Fatal(err)
	}
	plan, err := imagefactory.LoadBuildPlan(*planPath)
	if err != nil {
		log.Fatal(err)
	}
	manifest, err := imagefactory.LoadImageManifest(*manifestPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := manifest.ValidateForPlan(plan); err != nil {
		log.Fatal(err)
	}
	if err := manifest.ValidateEvidenceFiles(*commonPath, *planPath, *lockPath); err != nil {
		log.Fatal(err)
	}
	endpoint := strings.TrimSuffix(strings.TrimSuffix(common.ProxmoxURL, "/"), "/api2/json")
	config := &iac.PVEConfig{Endpoint: endpoint, Node: common.ProxmoxNode,
		TokenID: os.Getenv("PKR_VAR_proxmox_username"), TokenSecret: os.Getenv("PKR_VAR_proxmox_token"),
		Insecure: common.ProxmoxInsecure, StateDir: "./", SSHKeyPath: common.SSHPrivateKeyFile,
		SSHUser: common.SSHUsername, Storage: common.ProxmoxStorage, Network: common.ProxmoxBridge,
		Template: plan.Template, Bios: "ovmf", Machine: "q35"}
	if config.TokenID == "" || config.TokenSecret == "" {
		log.Fatal("PVE token credentials are required")
	}
	provisioner, err := iac.NewPVEProvisioner(config)
	if err != nil {
		log.Fatal(err)
	}
	spec := iac.DefaultPVEInstanceSpec()
	spec.Node, spec.Cores, spec.MemoryMB, spec.DiskSizeGB = common.ProxmoxNode, 2, 4096, 50
	spec.Storage, spec.Template, spec.Network, spec.Pool = common.ProxmoxStorage, plan.Template, common.ProxmoxBridge, common.ProxmoxPool
	spec.Bios, spec.Machine, spec.Agent, spec.CloudInit, spec.StartOnBoot = "ovmf", "q35", true, true, false
	if err := imagefactory.WriteTextAtomic(*output, provisioner.GenerateMainTF(spec, *instanceName)); err != nil {
		log.Fatal(err)
	}
}

func runGuestIP(args []string) {
	fs := flag.NewFlagSet("guest-ip", flag.ExitOnError)
	commonPath := fs.String("common", "", "Site-local common config JSON")
	node := fs.String("node", "", "PVE node")
	vmid := fs.String("vmid", "", "PVE VMID")
	timeoutSeconds := fs.Int("timeout", 600, "Timeout in seconds")
	_ = fs.Parse(args)
	if *commonPath == "" || *node == "" || *vmid == "" || *timeoutSeconds < 1 {
		log.Fatal("guest-ip requires -common, -node, -vmid, and a positive -timeout")
	}
	if _, err := strconv.Atoi(*vmid); err != nil {
		log.Fatal("invalid VMID")
	}
	common, err := imagefactory.LoadCommonConfig(*commonPath)
	if err != nil {
		log.Fatal(err)
	}
	endpoint := strings.TrimSuffix(strings.TrimSuffix(common.ProxmoxURL, "/"), "/api2/json")
	auth := iac.PVEAuth{TokenID: os.Getenv("PKR_VAR_proxmox_username"), TokenSecret: os.Getenv("PKR_VAR_proxmox_token"), Insecure: common.ProxmoxInsecure}
	ip, err := iac.WaitForPVEGuestIP(endpoint, auth, *node, *vmid, time.Duration(*timeoutSeconds)*time.Second, nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(ip)
}

func runGuestHostKey(args []string) {
	fs := flag.NewFlagSet("guest-host-key", flag.ExitOnError)
	commonPath := fs.String("common", "", "Site-local common config JSON")
	node := fs.String("node", "", "PVE node")
	vmid := fs.String("vmid", "", "PVE VMID")
	timeoutSeconds := fs.Int("timeout", 120, "Timeout in seconds")
	_ = fs.Parse(args)
	if *commonPath == "" || *node == "" || *vmid == "" || *timeoutSeconds < 1 {
		log.Fatal("guest-host-key requires -common, -node, -vmid, and a positive -timeout")
	}
	if _, err := strconv.Atoi(*vmid); err != nil {
		log.Fatal("invalid VMID")
	}
	common, err := imagefactory.LoadCommonConfig(*commonPath)
	if err != nil {
		log.Fatal(err)
	}
	endpoint := strings.TrimSuffix(strings.TrimSuffix(common.ProxmoxURL, "/"), "/api2/json")
	auth := iac.PVEAuth{TokenID: os.Getenv("PKR_VAR_proxmox_username"), TokenSecret: os.Getenv("PKR_VAR_proxmox_token"), Insecure: common.ProxmoxInsecure}
	key, err := iac.WaitForPVEGuestHostKey(endpoint, auth, *node, *vmid, time.Duration(*timeoutSeconds)*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(key)
}

func runPlan(args []string) {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	commonPath := fs.String("common", "", "Site-local common config JSON")
	planPath := fs.String("plan", "", "Reviewed BuildPlan JSON")
	lockPath := fs.String("lock", "", "Offline input lock JSON")
	offlineRoot := fs.String("root", "", "Verified offline root")
	target := fs.String("target", "", "Expected image target")
	packerManifest := fs.String("packer-manifest", "", "Future Packer manifest output path")
	varsOutput := fs.String("vars-output", "", "Generated Packer vars JSON")
	evidenceOutput := fs.String("evidence-output", "", "Validated plan evidence JSON")
	_ = fs.Parse(args)
	if *commonPath == "" || *planPath == "" || *lockPath == "" || *offlineRoot == "" || *target == "" || *packerManifest == "" || *varsOutput == "" || *evidenceOutput == "" {
		log.Fatal("plan requires -common, -plan, -lock, -root, -target, -packer-manifest, -vars-output, and -evidence-output")
	}
	plan, err := imagefactory.LoadBuildPlan(*planPath)
	if err != nil {
		log.Fatal(err)
	}
	if plan.Target != *target {
		log.Fatalf("BuildPlan target %q does not match requested target %q", plan.Target, *target)
	}
	vars, evidence, err := imagefactory.PreparePlan(*commonPath, *planPath, *lockPath, *offlineRoot, *packerManifest)
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*varsOutput, vars); err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*evidenceOutput, evidence); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("validated BuildPlan %s\n", evidence.PlanDigest)
}

func runSourceCheck(args []string) {
	fs := flag.NewFlagSet("source-check", flag.ExitOnError)
	commonPath := fs.String("common", "", "Site-local common config JSON")
	planPath := fs.String("plan", "", "Reviewed BuildPlan JSON")
	lockPath := fs.String("lock", "", "Offline input lock JSON")
	offlineRoot := fs.String("root", "", "Verified offline root")
	packerManifest := fs.String("packer-manifest", "", "Future Packer manifest output path")
	evidenceOutput := fs.String("evidence-output", "", "PVE source-check evidence JSON")
	_ = fs.Parse(args)
	if *commonPath == "" || *planPath == "" || *lockPath == "" || *offlineRoot == "" || *packerManifest == "" || *evidenceOutput == "" {
		log.Fatal("source-check requires -common, -plan, -lock, -root, -packer-manifest, and -evidence-output")
	}
	common, err := imagefactory.LoadCommonConfig(*commonPath)
	if err != nil {
		log.Fatal(err)
	}
	plan, err := imagefactory.LoadBuildPlan(*planPath)
	if err != nil {
		log.Fatal(err)
	}
	_, evidence, err := imagefactory.PreparePlan(*commonPath, *planPath, *lockPath, *offlineRoot, *packerManifest)
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.CheckPVESource(context.Background(), common, plan, evidence, os.Getenv("PKR_VAR_proxmox_username"), os.Getenv("PKR_VAR_proxmox_token")); err != nil {
		log.Fatal(err)
	}
	sourceEvidence := imagefactory.SourceCheckEvidence{SchemaVersion: 1, CheckedAt: time.Now().UTC(), SourceVMID: plan.SourceVMID,
		SourceTemplate: plan.SourceTemplate, SourceProvenanceObjectID: plan.SourceProvenanceObjectID,
		SourceProvenanceDigest: evidence.SourceProvenanceDigest, Verified: true}
	if err := imagefactory.WriteJSONAtomic(*evidenceOutput, sourceEvidence); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("verified PVE source VMID %d (%s)\n", plan.SourceVMID, evidence.SourceProvenanceDigest)
}

func runPreflight(args []string) {
	fs := flag.NewFlagSet("preflight", flag.ExitOnError)
	lockPath := fs.String("lock", "", "Offline input lock JSON")
	root := fs.String("root", "", "Offline mirror root")
	target := fs.String("target", "", "Image target ID")
	reportPath := fs.String("report", "", "Optional report output path")
	_ = fs.Parse(args)
	if *lockPath == "" || *root == "" || *target == "" {
		log.Fatal("preflight requires -lock, -root, and -target")
	}
	lock, err := imagefactory.LoadInputLock(*lockPath)
	if err != nil {
		log.Fatal(err)
	}
	report, err := imagefactory.Preflight(*root, lock, *target)
	if err != nil {
		log.Fatal(err)
	}
	if *reportPath != "" {
		if err := imagefactory.WriteJSONAtomic(*reportPath, report); err != nil {
			log.Fatal(err)
		}
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(data))
	if len(report.Missing) > 0 {
		os.Exit(1)
	}
}

func runLockMaterialize(args []string) {
	fs := flag.NewFlagSet("lock-materialize", flag.ExitOnError)
	draft := fs.String("draft", "", "Reviewed input-lock draft JSON")
	root := fs.String("root", "", "Offline root containing every drafted object")
	output := fs.String("output", "", "Atomic materialized input lock output")
	_ = fs.Parse(args)
	if *draft == "" || *root == "" || *output == "" {
		log.Fatal("lock-materialize requires -draft, -root, and -output")
	}
	lock, err := imagefactory.MaterializeInputLock(*draft, *root)
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, lock); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("materialized %d locked objects in %s\n", len(lock.Objects), *output)
}

func runManifest(args []string) {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	commonPath := fs.String("common", "", "Site-local common config JSON")
	planPath := fs.String("plan", "", "Reviewed BuildPlan JSON")
	lockPath := fs.String("lock", "", "Offline input lock JSON")
	packerPath := fs.String("packer-manifest", "", "Packer manifest JSON")
	output := fs.String("output", "", "Output image manifest JSON")
	catalogOutput := fs.String("catalog-output", "", "Output catalog candidate fragment JSON")
	_ = fs.Parse(args)
	if *commonPath == "" || *planPath == "" || *lockPath == "" || *packerPath == "" || *output == "" || *catalogOutput == "" {
		log.Fatal("manifest requires -common, -plan, -lock, -packer-manifest, -output, and -catalog-output")
	}
	manifest, err := imagefactory.GenerateManifest(*commonPath, *planPath, *lockPath, *packerPath, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, manifest); err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*catalogOutput, manifest.CatalogFragment()); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s and %s\n", *output, *catalogOutput)
}

func runCatalogAssemble(args []string) {
	fs := flag.NewFlagSet("catalog-assemble", flag.ExitOnError)
	specPath := fs.String("spec", "", "Strict candidate release assembly JSON")
	output := fs.String("output", "", "Validated candidate catalog output")
	_ = fs.Parse(args)
	if *specPath == "" || *output == "" {
		log.Fatal("catalog-assemble requires -spec and -output")
	}
	candidate, err := imagefactory.AssembleCandidateCatalog(*specPath, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, candidate); err != nil {
		log.Fatal(err)
	}
	digest, err := imagefactory.CanonicalDigest(candidate)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("assembled candidate catalog with %d images: %s\n", len(candidate.Images), digest)
}

func runStatusCompile(args []string) {
	fs := flag.NewFlagSet("status-compile", flag.ExitOnError)
	input := fs.String("input", "", "Strict image-factory status source JSON")
	output := fs.String("output", "", "Validated atomic status output for the server/WebUI")
	_ = fs.Parse(args)
	if *input == "" || *output == "" {
		log.Fatal("status-compile requires -input and -output")
	}
	status, err := imagefactory.LoadFactoryStatus(*input)
	if err != nil {
		log.Fatal(err)
	}
	if err := imagefactory.WriteJSONAtomic(*output, status); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("validated image-factory status %s (%s)\n", *output, status.OverallState)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: portage-image-factory <preflight|lock-materialize|plan|source-check|pbs-attest|pbs-stamp-source|manifest|catalog-assemble|smoke-config|guest-ip|guest-host-key|stamp-output|catalyst-plan|catalyst-gate|catalyst-manifest|qcow2-manifest|qcow2-check|ops-keygen|bundle-seal|bundle-verify|ops-digest|promote|rollback|state-sign|cleanup-plan|rebuild-plan|status-compile|package-sets> [flags]")
}
