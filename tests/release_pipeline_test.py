import hashlib
import json
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "release_manifest.py"
CONFIG_PATH = ROOT / "release" / "release-config.json"
DIGEST_A = "sha256:" + "a" * 64
DIGEST_B = "sha256:" + "b" * 64


def digest(path: Path) -> str:
    return "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()


def write_json(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


class ReleaseManifestContractTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = Path(tempfile.mkdtemp(prefix="release-contract-"))
        self.addCleanup(lambda: shutil.rmtree(self.temp))
        self.config = json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
        (self.temp / "binaries").mkdir()
        (self.temp / "evidence").mkdir()

        for command in self.config["commands"]:
            for platform in self.config["binary_platforms"]:
                name = f"portage-{command}-{platform['os']}-{platform['arch']}"
                (self.temp / "binaries" / name).write_bytes(name.encode("ascii"))
        binary_lines = []
        for path in sorted((self.temp / "binaries").iterdir()):
            binary_lines.append(f"{digest(path).removeprefix('sha256:')}  binaries/{path.name}")
        (self.temp / "SHA256SUMS").write_text("\n".join(binary_lines) + "\n", encoding="utf-8")

        namespace = "ghcr.io/example/portage-engine"
        images = []
        evidence_lines = []
        for index, role in enumerate(self.config["roles"]):
            image_digest = "sha256:" + f"{index + 1:064x}"
            descriptors = {}
            for kind, suffix in (("sbom", "sbom.spdx.json"), ("provenance", "provenance.slsa.json")):
                relative = f"evidence/{role['name']}.{suffix}"
                path = self.temp / relative
                write_json(path, {"kind": kind, "role": role["name"]})
                descriptors[kind] = {"path": relative, "sha256": digest(path)}
                evidence_lines.append(f"{digest(path).removeprefix('sha256:')}  {relative}")
            repository = f"{namespace}/{role['name']}"
            images.append(
                {
                    "role": role["name"],
                    "target": role["target"],
                    "repository": repository,
                    "digest": image_digest,
                    "reference": repository + "@" + image_digest,
                    "platforms": self.config["platforms"],
                    "sbom": descriptors["sbom"],
                    "provenance": descriptors["provenance"],
                }
            )
        evidence_lines.sort(key=lambda line: line.split("  ", 1)[1])
        (self.temp / "EVIDENCE.SHA256SUMS").write_text("\n".join(evidence_lines) + "\n", encoding="utf-8")
        write_json(self.temp / "images.json", images)
        self.run_cli("index-binaries", "--root", str(self.temp), "--output", str(self.temp / "binaries.json"))
        checksums = {
            "binaries": {"path": "SHA256SUMS", "sha256": digest(self.temp / "SHA256SUMS")},
            "evidence": {"path": "EVIDENCE.SHA256SUMS", "sha256": digest(self.temp / "EVIDENCE.SHA256SUMS")},
        }
        write_json(self.temp / "checksums.json", checksums)
        self.candidate = self.temp / "release-manifest.json"
        self.run_cli(
            "create-candidate",
            "--release-id",
            "v1.2.3",
            "--source-repository",
            "example/portage-engine",
            "--source-commit",
            "1" * 40,
            "--source-ref",
            "refs/tags/v1.2.3",
            "--created-at",
            "2026-08-01T00:00:00Z",
            "--images",
            str(self.temp / "images.json"),
            "--binaries",
            str(self.temp / "binaries.json"),
            "--checksums",
            str(self.temp / "checksums.json"),
            "--root",
            str(self.temp),
            "--output",
            str(self.candidate),
        )

    def run_cli(self, *args: str, expect_ok: bool = True) -> subprocess.CompletedProcess[str]:
        result = subprocess.run(
            ["python3", str(SCRIPT), "--config", str(CONFIG_PATH), *args],
            cwd=ROOT,
            check=False,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        if expect_ok and result.returncode != 0:
            self.fail(f"command failed: {result.stderr}")
        if not expect_ok and result.returncode == 0:
            self.fail("command unexpectedly succeeded")
        return result

    def test_candidate_is_canonical_and_digest_bound(self) -> None:
        result = self.run_cli("validate", "--manifest", str(self.candidate), "--root", str(self.temp))
        self.assertEqual(result.stdout.strip(), digest(self.candidate))
        binary = next((self.temp / "binaries").iterdir())
        binary.write_bytes(b"tampered")
        failure = self.run_cli("validate", "--manifest", str(self.candidate), "--root", str(self.temp), expect_ok=False)
        self.assertIn("does not bind", failure.stderr)

    def test_checksum_order_is_validated_by_path_not_digest(self) -> None:
        checksum_path = self.temp / "EVIDENCE.SHA256SUMS"
        lines = checksum_path.read_text(encoding="utf-8").splitlines()
        self.assertEqual([line.split("  ", 1)[1] for line in lines], sorted(line.split("  ", 1)[1] for line in lines))
        checksum_path.write_text("\n".join(reversed(lines)) + "\n", encoding="utf-8")
        manifest = json.loads(self.candidate.read_text(encoding="utf-8"))
        manifest["checksums"]["evidence"]["sha256"] = digest(checksum_path)
        self.candidate.write_text(json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
        failure = self.run_cli("validate", "--manifest", str(self.candidate), "--root", str(self.temp), expect_ok=False)
        self.assertIn("sorted by path", failure.stderr)

    def test_unknown_fields_mutable_references_and_path_escape_fail(self) -> None:
        manifest = json.loads(self.candidate.read_text(encoding="utf-8"))
        manifest["unknown"] = True
        self.candidate.write_text(json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
        self.assertNotEqual(self.run_cli("validate", "--manifest", str(self.candidate), "--root", str(self.temp), expect_ok=False).returncode, 0)

        manifest.pop("unknown")
        manifest["images"][0]["reference"] = manifest["images"][0]["repository"] + ":stable"
        self.candidate.write_text(json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
        self.assertIn("digest-bound", self.run_cli("validate", "--manifest", str(self.candidate), "--root", str(self.temp), expect_ok=False).stderr)

        manifest["images"][0]["reference"] = manifest["images"][0]["repository"] + "@" + manifest["images"][0]["digest"]
        manifest["images"][0]["sbom"]["path"] = "../outside.json"
        self.candidate.write_text(json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
        self.assertIn("confined", self.run_cli("validate", "--manifest", str(self.candidate), "--root", str(self.temp), expect_ok=False).stderr)

    def test_promotion_compare_and_swap_and_rollback_inputs(self) -> None:
        first = self.temp / "stable-first.json"
        self.run_cli(
            "promote",
            "--candidate",
            str(self.candidate),
            "--candidate-root",
            str(self.temp),
            "--candidate-oci-digest",
            DIGEST_A,
            "--expected-previous-manifest-digest",
            "none",
            "--expected-previous-oci-digest",
            "none",
            "--promoted-at",
            "2026-08-01T01:00:00Z",
            "--output-root",
            str(self.temp),
            "--output",
            str(first),
        )
        first_digest = digest(first)
        second = self.temp / "stable-second.json"
        bad = self.run_cli(
            "promote",
            "--candidate",
            str(self.candidate),
            "--candidate-root",
            str(self.temp),
            "--candidate-oci-digest",
            DIGEST_A,
            "--expected-previous-manifest-digest",
            DIGEST_B,
            "--expected-previous-oci-digest",
            DIGEST_B,
            "--current-stable",
            str(first),
            "--current-root",
            str(self.temp),
            "--promoted-at",
            "2026-08-01T02:00:00Z",
            "--output-root",
            str(self.temp),
            "--output",
            str(second),
            expect_ok=False,
        )
        self.assertIn("CAS digest", bad.stderr)
        self.run_cli(
            "promote",
            "--candidate",
            str(self.candidate),
            "--candidate-root",
            str(self.temp),
            "--candidate-oci-digest",
            DIGEST_A,
            "--expected-previous-manifest-digest",
            first_digest,
            "--expected-previous-oci-digest",
            DIGEST_B,
            "--current-stable",
            str(first),
            "--current-root",
            str(self.temp),
            "--promoted-at",
            "2026-08-01T02:00:00Z",
            "--output-root",
            str(self.temp),
            "--output",
            str(second),
        )
        rolled_back = self.temp / "stable-rollback.json"
        self.run_cli(
            "rollback",
            "--current",
            str(second),
            "--current-root",
            str(self.temp),
            "--target",
            str(first),
            "--target-root",
            str(self.temp),
            "--target-oci-digest",
            DIGEST_A,
            "--expected-current-manifest-digest",
            digest(second),
            "--expected-current-oci-digest",
            DIGEST_B,
            "--reason",
            "regression in v1.2.3",
            "--rolled-back-at",
            "2026-08-01T03:00:00Z",
            "--output-root",
            str(self.temp),
            "--output",
            str(rolled_back),
        )
        rollback = json.loads(rolled_back.read_text(encoding="utf-8"))
        self.assertEqual(rollback["transition"]["target_manifest_digest"], first_digest)
        self.assertEqual(rollback["transition"]["expected_previous_stable_manifest_digest"], digest(second))
        self.assertEqual(rollback["transition"]["expected_previous_stable_oci_digest"], DIGEST_B)
        self.assertEqual(rollback["images"], json.loads(first.read_text(encoding="utf-8"))["images"])

        self.assertIn(
            "rollback CAS digest",
            self.run_cli(
                "rollback",
                "--current",
                str(second),
                "--current-root",
                str(self.temp),
                "--target",
                str(first),
                "--target-root",
                str(self.temp),
                "--target-oci-digest",
                DIGEST_A,
                "--expected-current-manifest-digest",
                DIGEST_B,
                "--expected-current-oci-digest",
                DIGEST_B,
                "--reason",
                "wrong expectation",
                "--rolled-back-at",
                "2026-08-01T03:00:00Z",
                "--output-root",
                str(self.temp),
                "--output",
                str(rolled_back),
                expect_ok=False,
            ).stderr,
        )


class ReleasePipelineStaticTest(unittest.TestCase):
    def test_release_config_covers_every_production_runtime_target(self) -> None:
        config = json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
        dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")
        targets = set(re.findall(r"^FROM\s+\S+(?:\s+AS\s+)([a-z0-9-]+)\s*$", dockerfile, re.MULTILINE | re.IGNORECASE))
        production_targets = {target for target in targets if target.endswith("-runtime")} - {"trusted-runtime"}
        self.assertEqual(production_targets, {role["target"] for role in config["roles"]})
        self.assertNotIn("trusted-runtime", {role["target"] for role in config["roles"]})

    def test_every_external_action_is_full_sha_pinned(self) -> None:
        workflow_dir = ROOT / ".github" / "workflows"
        uses = []
        for workflow in workflow_dir.glob("*.yml"):
            for line_number, line in enumerate(workflow.read_text(encoding="utf-8").splitlines(), 1):
                match = re.search(r"\buses:\s*([^\s#]+)", line)
                if match and not match.group(1).startswith("./"):
                    uses.append((workflow.name, line_number, match.group(1)))
        unpinned = [entry for entry in uses if not re.fullmatch(r"[^@]+@[0-9a-f]{40}", entry[2])]
        self.assertEqual(unpinned, [], f"unpinned Actions: {unpinned}")

    def test_pull_request_workflows_have_no_registry_or_oidc_write(self) -> None:
        for workflow in (ROOT / ".github" / "workflows").glob("*.yml"):
            text = workflow.read_text(encoding="utf-8")
            if re.search(r"^\s*pull_request:", text, re.MULTILINE):
                self.assertNotRegex(text, r"packages:\s*write")
                self.assertNotRegex(text, r"id-token:\s*write")
                self.assertNotIn("secrets:", text)

    def test_release_workflows_are_manual_or_tag_gated(self) -> None:
        candidate = (ROOT / ".github" / "workflows" / "release-candidate.yml").read_text(encoding="utf-8")
        promote = (ROOT / ".github" / "workflows" / "release-promote.yml").read_text(encoding="utf-8")
        rollback = (ROOT / ".github" / "workflows" / "release-rollback.yml").read_text(encoding="utf-8")
        self.assertIn("workflow_dispatch:", candidate)
        self.assertIn("tags:", candidate)
        self.assertNotIn("pull_request:", candidate)
        self.assertIn("release-candidate", candidate)
        for text in (promote, rollback):
            self.assertIn("workflow_dispatch:", text)
            self.assertNotIn("pull_request:", text)
            self.assertIn("release-stable", text)
            self.assertIn("oras resolve", text)
        self.assertIn("expected_previous_manifest_digest", promote)
        self.assertIn("expected_current_manifest_digest", rollback)


if __name__ == "__main__":
    unittest.main()
