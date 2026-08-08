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


class OrasResolveClassificationTest(unittest.TestCase):
    """`oras resolve` cannot distinguish absent from unreachable on its own."""

    HELPER = ROOT / "scripts" / "oras-resolve.sh"

    def resolve(self, stdout: str, status: int) -> subprocess.CompletedProcess[str]:
        temp = Path(tempfile.mkdtemp(prefix="oras-stub-"))
        self.addCleanup(lambda: shutil.rmtree(temp))
        stub = temp / "oras"
        stub.write_text(
            f'#!/usr/bin/env bash\nprintf "%s\\n" {json.dumps(stdout)}\nexit {status}\n',
            encoding="utf-8",
        )
        stub.chmod(0o755)
        return subprocess.run(
            [
                "bash",
                "-c",
                f'set -euo pipefail; source "{self.HELPER}"; '
                'oras_resolve_into digest ghcr.io/example/releases:stable; '
                'printf "%s" "${digest}"',
            ],
            env={"PATH": f"{temp}:/usr/bin:/bin"},
            check=False,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )

    def test_resolved_tag_yields_its_digest(self) -> None:
        result = self.resolve(DIGEST_A, 0)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, DIGEST_A)

    def test_explicit_not_found_is_the_only_absent_answer(self) -> None:
        result = self.resolve("Error: ghcr.io/example/releases:stable: not found", 1)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "none")

    def test_transport_failure_is_never_read_as_absent(self) -> None:
        for message in (
            "Error: unexpected status code 503 Service Unavailable",
            "Error: unauthorized: authentication required",
            "Error: context deadline exceeded",
        ):
            with self.subTest(message=message):
                result = self.resolve(message, 1)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("none", result.stdout)

    def test_release_workflows_classify_every_resolve(self) -> None:
        for name in ("release-candidate.yml", "release-promote.yml", "release-rollback.yml"):
            text = (ROOT / ".github" / "workflows" / name).read_text(encoding="utf-8")
            with self.subTest(workflow=name):
                self.assertIn("source scripts/oras-resolve.sh", text)
                self.assertNotRegex(text, r"if\s+oras resolve[^\n]*2>&1")
                self.assertNotRegex(text, r"\$\(oras resolve[^\n]*2>/dev/null\)")
                self.assertNotRegex(text, r'"\$\(oras resolve[^\n]*\)"')


class StableRevisionSigningOrderTest(unittest.TestCase):
    """Signing and tag order decide whether a lost cutover is recoverable.

    Both stable workflows open by resolving `:stable` and verifying the
    signature on whatever it binds, so a `:stable` that points at an unsigned
    revision locks out every workflow that could replace it -- including the
    re-run that would repair it. So the revision is signed before any tag
    moves, and every write-once `stable-${release_id}` tag -- in the release
    repository and in every role repository alike -- is written only after the
    alias is bound and read back, which leaves an abandoned revision inert:
    signed, but reachable only by its content-addressed digest tag and safe to
    re-promote under the same release ID.
    """

    # The line that makes a revision authoritative in each workflow. It writes
    # the `:stable` alias and nothing else: `oras tag` applies a tag list one
    # tag at a time, so a call that moves the alias and records the release ID
    # together has an interior a dying run can stop in.
    CUTOVER = {
        "release-promote.yml": 'oras tag "${RELEASE_REPOSITORY}@${stable_oci_digest}" stable',
        "release-rollback.yml": 'oras tag "${RELEASE_REPOSITORY}@${rollback_oci_digest}" stable',
    }
    RELEASE_ID_TAG_WRITE = re.compile(r'^[ \t]*oras tag .*"stable-\$\{release_id\}"', re.MULTILINE)
    IMMUTABLE_RECORD_WRITE = re.compile(r'^[ \t]*oras tag \S+ "stable-\$\{release_id\}"\s*$')

    def workflow(self, name: str) -> str:
        return (ROOT / ".github" / "workflows" / name).read_text(encoding="utf-8")

    def test_revision_is_signed_before_any_tag_moves(self) -> None:
        for name in self.CUTOVER:
            text = self.workflow(name)
            with self.subTest(workflow=name):
                signature = text.index("cosign sign --yes")
                first_tag_move = text.index("oras tag ")
                self.assertLess(
                    signature,
                    first_tag_move,
                    "a tag moves before cosign signs the revision it points at",
                )

    def test_nothing_that_can_fail_runs_after_the_cutover(self) -> None:
        # Once the alias binds the new revision the promotion has won and its
        # inputs will never be accepted again, so the only work left may be
        # writing the write-once records and reading tags back. Anything that
        # re-derives, re-signs or re-fetches the revision there fails a release
        # that already happened, and no re-run can repair it.
        for name, cutover in self.CUTOVER.items():
            text = self.workflow(name)
            with self.subTest(workflow=name):
                self.assertTrue(
                    cutover in text,
                    f"{name} must reach stable through an oras tag call that writes the alias alone",
                )
                tail = text[text.index(cutover) + len(cutover) :]
                for command in ("cosign ", "python3 ", "oras push", "oras pull"):
                    self.assertNotIn(
                        command,
                        tail,
                        f"{command.strip()} can fail after the cutover has already landed",
                    )
                for line in tail.splitlines():
                    if "oras tag " in line:
                        self.assertRegex(
                            line,
                            self.IMMUTABLE_RECORD_WRITE,
                            "only write-once release ID records may be tagged after the cutover",
                        )

    def test_every_release_id_tag_is_written_only_once_the_cutover_is_won(self) -> None:
        # `stable-${release_id}` is the write-once record the two existence
        # guards read, and neither guard can tell a record left by a promotion
        # that died from one left by a promotion that finished. So a single such
        # tag written before the alias is bound -- by the role loop as much as
        # by the release repository call -- makes that release ID exit 1 forever
        # while `:stable` still names the old revision.
        text = self.workflow("release-promote.yml")
        cutover = self.CUTOVER["release-promote.yml"]
        self.assertEqual(
            text.count('"stable-${release_id}" stable'),
            0,
            "one oras tag call writes the release ID record and moves the alias",
        )
        read_back = text.index("stable alias read-back did not bind the signed manifest")
        for write in self.RELEASE_ID_TAG_WRITE.finditer(text):
            with self.subTest(write=write.group().strip()):
                self.assertGreater(
                    write.start(),
                    read_back,
                    "a release ID tag is written before the cutover is won and read back",
                )
        # Both records still have to be written; ordering them correctly is not
        # the same as dropping them.
        self.assertIn('oras tag "${RELEASE_REPOSITORY}@${stable_oci_digest}" "stable-${release_id}"', text)
        self.assertIn('oras tag "${reference}" "stable-${release_id}"', text)
        self.assertLess(
            text.index("stable alias changed before the final cutover"),
            text.index(cutover),
        )
        self.assertLess(
            text.index("stable release ID tag already exists; refusing to overwrite it"),
            text.index("oras push --artifact-type"),
        )

    def test_verification_uses_the_config_carried_by_the_signed_bundle(self) -> None:
        verify = (ROOT / "scripts" / "verify-release.sh").read_text(encoding="utf-8")
        self.assertIn('--config "$verify_tmp/release-config.json"', verify)
        self.assertIn('"$script_dir/release_manifest.py"', verify)
        self.assertNotIn("python3 scripts/release_manifest.py", verify)
        candidate = (ROOT / ".github" / "workflows" / "release-candidate.yml").read_text(encoding="utf-8")
        self.assertIn("cp release/release-config.json bundle/release-config.json", candidate)


class AbandonedCutoverRerunTest(unittest.TestCase):
    """A stable workflow that dies must leave a registry its re-run can finish.

    Reading the order of the tag writes proves they are ordered; it does not
    prove the order is survivable. A registry applies one tag at a time and a
    run can stop between any two of them -- failed step, cancelled workflow,
    reclaimed runner -- so this drives the workflows' own step scripts against a
    stub registry, stops them after every prefix of their tag writes, and then
    replays them from a clean checkout against whatever the dead run left
    behind. A run that never reached the cutover must be able to complete on
    the second attempt; a run that reached it must be refused rather than
    silently promoting a second time.
    """

    RELEASE_REPOSITORY = "ghcr.io/example/portage-engine/releases"
    RELEASE_ID = "v1.2.3"
    ROLES = ("server", "dashboard", "signer")
    OLD_BUNDLE = "sha256:" + "1" * 64
    NEW_BUNDLE = "sha256:" + "2" * 64
    MANIFEST_DIGEST = "sha256:" + "3" * 64

    STUBS = {
        # A tag store plus a write budget: exhausting the budget aborts the
        # process mid-call, which is how a run dies between the tags of one
        # `oras tag` invocation.
        "oras": """#!/bin/bash
registry="${STUB_REGISTRY}"
counter="${registry}/writes"
digest=""

store_path() {
  local sanitized="${1//\\//_}"
  sanitized="${sanitized//:/_}"
  store="${registry}/${sanitized//@/_}"
}

resolve_reference() {
  digest=""
  case "$1" in
    *@sha256:*) digest="${1#*@}" ;;
    *)
      store_path "$1"
      if [ -f "${store}" ]; then read -r digest < "${store}"; fi
      ;;
  esac
}

take_write() {
  local count
  read -r count < "${counter}"
  if [ -n "${STUB_FAIL_AFTER_WRITES:-}" ] && [ "${count}" -ge "${STUB_FAIL_AFTER_WRITES}" ]; then
    echo "stub: the runner stopped after ${count} registry writes" >&2
    exit 1
  fi
  printf '%s\\n' "$((count + 1))" > "${counter}"
}

subcommand="$1"
shift
case "${subcommand}" in
  resolve)
    resolve_reference "$1"
    if [ -z "${digest}" ]; then
      echo "Error: $1: not found" >&2
      exit 1
    fi
    printf '%s\\n' "${digest}"
    ;;
  tag)
    reference="$1"
    shift
    resolve_reference "${reference}"
    if [ -z "${digest}" ]; then
      echo "Error: ${reference}: not found" >&2
      exit 1
    fi
    case "${reference}" in
      *@*) repository="${reference%%@*}" ;;
      *) repository="${reference%:*}" ;;
    esac
    for tag in "$@"; do
      take_write
      store_path "${repository}:${tag}"
      printf '%s\\n' "${digest}" > "${store}"
    done
    ;;
  push)
    shift 2
    take_write
    store_path "$1"
    printf '%s\\n' "${STUB_BUNDLE_DIGEST}" > "${store}"
    ;;
  *)
    echo "unstubbed oras subcommand: ${subcommand}" >&2
    exit 2
    ;;
esac
""",
        "python3": """#!/bin/sh
mode=""
output=""
previous=""
for argument in "$@"; do
  if [ "${previous}" = "--output" ]; then output="${argument}"; fi
  case "${argument}" in
    promote|rollback|validate)
      if [ -z "${mode}" ]; then mode="${argument}"; fi
      ;;
  esac
  previous="${argument}"
done
case "${mode}" in
  promote|rollback) cp "${STUB_MANIFEST}" "${output}" ;;
  validate) printf '%s\\n' "${STUB_MANIFEST_DIGEST}" ;;
  *)
    echo "unstubbed release_manifest.py invocation: $*" >&2
    exit 2
    ;;
esac
""",
        "jq": """#!/bin/sh
case "$2" in
  '.release.id') printf '%s\\n' "${STUB_RELEASE_ID}" ;;
  '.images[] | [.repository, .reference] | @tsv') cut -f1,2 "${STUB_IMAGES_TSV}" ;;
  '.images[] | [.repository, .reference, .digest] | @tsv') cat "${STUB_IMAGES_TSV}" ;;
  *)
    echo "unstubbed jq filter: $2" >&2
    exit 2
    ;;
esac
""",
        "cosign": """#!/bin/sh
previous=""
for argument in "$@"; do
  if [ "${previous}" = "--bundle" ]; then : > "${argument}"; fi
  previous="${argument}"
done
""",
        "find": """#!/bin/sh
/usr/bin/find . -type f | sed 's|^\\./||'
""",
        "date": """#!/bin/sh
printf '%s\\n' "${STUB_TIMESTAMP}"
""",
    }

    @classmethod
    def setUpClass(cls) -> None:
        cls.bash = None
        candidates = ["/opt/homebrew/bin/bash", "/usr/local/bin/bash", shutil.which("bash"), "/bin/bash"]
        for candidate in candidates:
            # The step scripts use mapfile, which bash 3.2 -- still /bin/bash on
            # macOS -- does not have.
            if not candidate or not Path(candidate).exists():
                continue
            probe = subprocess.run(
                [candidate, "-c", 'mapfile -t lines < <(printf "ok\\n"); printf "%s" "${lines[0]}"'],
                check=False,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
            )
            if probe.returncode == 0 and probe.stdout == "ok":
                cls.bash = candidate
                break

    def setUp(self) -> None:
        if self.bash is None:
            self.skipTest("no bash with mapfile available to run the workflow step scripts")
        self.temp = Path(tempfile.mkdtemp(prefix="release-cutover-"))
        self.addCleanup(lambda: shutil.rmtree(self.temp))
        self.new_image = {role: "sha256:" + f"{index + 10:064x}" for index, role in enumerate(self.ROLES)}
        self.old_image = {role: "sha256:" + f"{index + 90:064x}" for index, role in enumerate(self.ROLES)}
        self.stub_dir = self.temp / "bin"
        self.stub_dir.mkdir()
        for name, body in self.STUBS.items():
            stub = self.stub_dir / name
            stub.write_text(body, encoding="utf-8")
            stub.chmod(0o755)
        self.images = self.temp / "images.tsv"
        self.images.write_text(
            "".join(
                f"{self.repository(role)}\t{self.repository(role)}@{self.new_image[role]}\t{self.new_image[role]}\n"
                for role in self.ROLES
            ),
            encoding="utf-8",
        )
        self.manifest = self.temp / "release-manifest.json"
        write_json(self.manifest, {"release": {"id": self.RELEASE_ID}})
        self.runs = 0

    def repository(self, role: str) -> str:
        return f"ghcr.io/example/portage-engine/{role}"

    def script(self, workflow: str, step: str) -> str:
        """The literal shell body of one workflow step, dedented as bash sees it."""
        lines = (ROOT / ".github" / "workflows" / workflow).read_text(encoding="utf-8").splitlines()
        start = lines.index(f"      - name: {step}")
        run = lines.index("        run: |", start) + 1
        body = []
        for line in lines[run:]:
            if line.strip() and not line.startswith(" " * 10):
                break
            body.append(line[10:])
        return "\n".join(body) + "\n"

    def registry(self) -> Path:
        """A registry holding the stable release and images this run replaces."""
        directory = self.temp / f"registry-{len(list(self.temp.glob('registry-*')))}"
        directory.mkdir()
        self.tag(directory, f"{self.RELEASE_REPOSITORY}:stable", self.OLD_BUNDLE)
        for role in self.ROLES:
            self.tag(directory, f"{self.repository(role)}:stable", self.old_image[role])
        (directory / "writes").write_text("0\n", encoding="utf-8")
        return directory

    @staticmethod
    def key(reference: str) -> str:
        return re.sub(r"[/:@]", "_", reference)

    def tag(self, registry: Path, reference: str, digest: str) -> None:
        (registry / self.key(reference)).write_text(digest + "\n", encoding="utf-8")

    def resolve(self, registry: Path, reference: str) -> str:
        stored = registry / self.key(reference)
        return stored.read_text(encoding="utf-8").strip() if stored.exists() else "none"

    def writes(self, registry: Path) -> int:
        return int((registry / "writes").read_text(encoding="utf-8").strip())

    def execute(self, script: str, registry: Path, environment, fail_after=None):
        """Run the step script the way a fresh runner would: new checkout, one registry."""
        self.runs += 1
        work = self.temp / f"run-{self.runs}"
        (work / "scripts").mkdir(parents=True)
        shutil.copy(ROOT / "scripts" / "oras-resolve.sh", work / "scripts" / "oras-resolve.sh")
        for name in ("candidate", "current", "target"):
            (work / name).mkdir()
            (work / name / "release-manifest.json").write_text("{}\n", encoding="utf-8")
            (work / name / "release-config.json").write_text("{}\n", encoding="utf-8")
        env = {
            "PATH": f"{self.stub_dir}:/usr/bin:/bin",
            "STUB_REGISTRY": str(registry),
            "STUB_BUNDLE_DIGEST": self.NEW_BUNDLE,
            "STUB_MANIFEST": str(self.manifest),
            "STUB_MANIFEST_DIGEST": self.MANIFEST_DIGEST,
            "STUB_IMAGES_TSV": str(self.images),
            "STUB_RELEASE_ID": self.RELEASE_ID,
            "STUB_TIMESTAMP": "2026-08-01T00:00:00Z",
            "GITHUB_REPOSITORY": "example/portage-engine",
            "GITHUB_REF": "refs/heads/main",
            "GITHUB_STEP_SUMMARY": str(work / "summary.md"),
            "RELEASE_REPOSITORY": self.RELEASE_REPOSITORY,
            "CURRENT_OCI_DIGEST": self.OLD_BUNDLE,
            **environment,
        }
        if fail_after is not None:
            env["STUB_FAIL_AFTER_WRITES"] = str(fail_after)
        return subprocess.run(
            [self.bash, "-c", script],
            cwd=work,
            env=env,
            check=False,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )

    def assert_stable_is_fully_new(self, registry: Path) -> None:
        self.assertEqual(self.resolve(registry, f"{self.RELEASE_REPOSITORY}:stable"), self.NEW_BUNDLE)
        for role in self.ROLES:
            self.assertEqual(
                self.resolve(registry, f"{self.repository(role)}:stable"),
                self.new_image[role],
                f"{role}:stable still names the replaced revision while the release manifest is authoritative",
            )

    def assert_records_are_written(self, registry: Path) -> None:
        self.assertEqual(
            self.resolve(registry, f"{self.RELEASE_REPOSITORY}:stable-{self.RELEASE_ID}"),
            self.NEW_BUNDLE,
        )
        for role in self.ROLES:
            self.assertEqual(
                self.resolve(registry, f"{self.repository(role)}:stable-{self.RELEASE_ID}"),
                self.new_image[role],
            )

    def test_a_promotion_that_dies_before_the_cutover_can_be_re_run(self) -> None:
        script = self.script("release-promote.yml", "Create and sign the stable release manifest")
        environment = {
            "EXPECTED_PREVIOUS": "sha256:" + "9" * 64,
            "CANDIDATE_OCI_DIGEST": "sha256:" + "8" * 64,
            "STABLE_IDENTITY_REGEX": "^https://github.com/example/portage-engine/.*$",
        }
        complete = self.registry()
        result = self.execute(script, complete, environment)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_stable_is_fully_new(complete)
        self.assert_records_are_written(complete)

        for stopped_after in range(self.writes(complete)):
            with self.subTest(writes_before_failure=stopped_after):
                registry = self.registry()
                first = self.execute(script, registry, environment, fail_after=stopped_after)
                self.assertNotEqual(first.returncode, 0, "the injected failure did not stop the run")
                if self.resolve(registry, f"{self.RELEASE_REPOSITORY}:stable") == self.OLD_BUNDLE:
                    second = self.execute(script, registry, environment)
                    self.assertEqual(
                        second.returncode,
                        0,
                        f"the abandoned promotion blocks its own re-run: {second.stderr}",
                    )
                    self.assert_stable_is_fully_new(registry)
                    self.assert_records_are_written(registry)
                else:
                    # The alias moved, so the promotion won and the images it
                    # names must already be reachable under `:stable`.
                    self.assert_stable_is_fully_new(registry)
                    second = self.execute(script, registry, environment)
                    self.assertNotEqual(
                        second.returncode,
                        0,
                        "a promotion that already won the compare-and-swap ran a second time",
                    )

    def test_a_rollback_that_dies_before_the_cutover_can_be_re_run(self) -> None:
        script = self.script("release-rollback.yml", "Create a signed rollback revision and update convenience tags")
        environment = {
            "EXPECTED_CURRENT": "sha256:" + "9" * 64,
            "TARGET_OCI_DIGEST": "sha256:" + "8" * 64,
            "ROLLBACK_REASON": "regression in v1.2.3",
        }
        complete = self.registry()
        result = self.execute(script, complete, environment)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_stable_is_fully_new(complete)

        for stopped_after in range(self.writes(complete)):
            with self.subTest(writes_before_failure=stopped_after):
                registry = self.registry()
                first = self.execute(script, registry, environment, fail_after=stopped_after)
                self.assertNotEqual(first.returncode, 0, "the injected failure did not stop the run")
                if self.resolve(registry, f"{self.RELEASE_REPOSITORY}:stable") == self.OLD_BUNDLE:
                    second = self.execute(script, registry, environment)
                    self.assertEqual(
                        second.returncode,
                        0,
                        f"the abandoned rollback blocks its own re-run: {second.stderr}",
                    )
                    self.assert_stable_is_fully_new(registry)
                else:
                    self.assert_stable_is_fully_new(registry)
                    second = self.execute(script, registry, environment)
                    self.assertNotEqual(
                        second.returncode,
                        0,
                        "a rollback that already won the compare-and-swap ran a second time",
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

    def test_every_external_base_image_is_digest_pinned(self) -> None:
        # A tag alone is a mutable input to a build whose output gets signed, and
        # `:latest@sha256:...` is pinned despite how the tag reads — the digest
        # is what the daemon resolves. Stage names introduced by `AS` are local.
        for dockerfile in sorted(ROOT.glob("Dockerfile*")):
            text = dockerfile.read_text(encoding="utf-8")
            stages = set(re.findall(r"^FROM\s+\S+\s+AS\s+(\S+)\s*$", text, re.MULTILINE | re.IGNORECASE))
            for line_number, line in enumerate(text.splitlines(), 1):
                match = re.match(r"^FROM\s+(\S+)", line, re.IGNORECASE)
                if not match or match.group(1) in stages:
                    continue
                with self.subTest(dockerfile=dockerfile.name, line=line_number):
                    self.assertRegex(match.group(1), r"@sha256:[0-9a-f]{64}$")

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
            self.assertIn("oras_resolve_into", text)
        self.assertIn("expected_previous_manifest_digest", promote)
        self.assertIn("expected_current_manifest_digest", rollback)

    def test_release_binaries_are_built_after_the_console_bundle(self) -> None:
        # portage-dashboard embeds the console and serves it as the catch-all at
        # `/`. A Go build that runs first still succeeds -- //go:embed all:bundle
        # matches the committed bundle/.gitkeep -- and produces a binary that
        # answers 503 on every console path. SHA256SUMS and the cosign signature
        # both describe that binary accurately, so nothing downstream catches it.
        text = (ROOT / ".github" / "workflows" / "release-candidate.yml").read_text(encoding="utf-8")
        install = text.index("npm ci")
        build_console = text.index("npm run build")
        build_go = text.index("Build the reviewed command and platform matrix")
        self.assertLess(install, build_console)
        self.assertLess(build_console, build_go)
        self.assertIn("internal/dashboard/webassets/bundle/dist/index.html", text)

    def test_every_job_that_needs_the_console_bundle_builds_it(self) -> None:
        # The release job was the third place this ordering constraint had to be
        # restated by hand, and pr-build.yml was the fourth -- its Test Check ran
        # the dashboard tests against an unbuilt bundle and its Build Verification
        # uploaded binaries that answer 503 on every console route. Restating a
        # constraint per workflow is what let it be missed, so this derives the
        # set of jobs that need it instead of naming them.
        #
        # A job needs the bundle if it links a portage-dashboard binary anyone
        # keeps, or runs the whole test suite -- four dashboard cases serve the
        # console and 503 without it. `go build ./...` is deliberately not on
        # this list: //go:embed all:bundle matches the committed bundle/.gitkeep,
        # so compiling succeeds either way and only the artifact is wrong.
        needs_bundle = ("cmd/dashboard/main.go", "go test -v -race", "go test ./...")
        missing = []
        for workflow in sorted((ROOT / ".github" / "workflows").glob("*.yml")):
            text = workflow.read_text(encoding="utf-8")
            for job_name, job in self._workflow_jobs(text):
                trigger = next(
                    (marker for marker in needs_bundle if marker in job), None
                )
                if trigger is None:
                    continue
                if "npm ci" not in job or "npm run build" not in job:
                    missing.append(f"{workflow.name}:{job_name} runs {trigger!r}")
                    continue
                if job.index("npm run build") > job.index(trigger):
                    missing.append(
                        f"{workflow.name}:{job_name} builds the console after {trigger!r}"
                    )
        self.assertEqual(missing, [])

    @staticmethod
    def _workflow_jobs(text: str) -> "list[tuple[str, str]]":
        """Split a workflow into (job name, job body) without a YAML parser.

        Jobs are the four-space-indented keys under `jobs:`; anything more
        indented belongs to the job above it.
        """
        lines = text.split("\n")
        try:
            start = lines.index("jobs:") + 1
        except ValueError:
            return []
        jobs: "list[tuple[str, list[str]]]" = []
        for line in lines[start:]:
            if line and not line.startswith(" "):
                break
            stripped = line.strip()
            if (
                line.startswith("  ")
                and not line.startswith("   ")
                and stripped.endswith(":")
                and not stripped.startswith("#")
            ):
                jobs.append((stripped[:-1], []))
            elif jobs:
                jobs[-1][1].append(line)
        return [(name, "\n".join(body)) for name, body in jobs]

    def test_every_golangci_lint_pin_can_load_this_module(self) -> None:
        # golangci-lint refuses to load any config whose module targets a Go
        # language version above the one it was built with, and exits 3 before
        # linting. The pin therefore has a floor as well as a ceiling, and the
        # floor moves with go.mod. Kept as one shared version so the CI lint job,
        # the gosec job, the PR job and `make lint` cannot disagree about which
        # findings are real.
        go_mod = (ROOT / "go.mod").read_text(encoding="utf-8")
        module_go = re.search(r"^go\s+(\d+)\.(\d+)", go_mod, re.MULTILINE)
        assert module_go is not None
        required = (int(module_go.group(1)), int(module_go.group(2)))

        # (golangci-lint release, the Go minor it is built with). Extend when the
        # pin moves; an unlisted version fails here rather than in CI.
        built_with = {(2, 12, 2): (1, 26)}

        # Read the `version:` that belongs to the golangci-lint step, not every
        # `version:` in the file: these workflows pin other tools the same way.
        pins = set()
        for workflow in sorted((ROOT / ".github" / "workflows").glob("*.yml")):
            lines = workflow.read_text(encoding="utf-8").splitlines()
            for index, line in enumerate(lines):
                if "golangci-lint-action@" not in line:
                    continue
                for follower in lines[index + 1 : index + 6]:
                    pin = re.match(r"^\s*version:\s*v(\d+\.\d+\.\d+)\s*$", follower)
                    if pin:
                        pins.add(pin.group(1))
                        break
                else:
                    self.fail(f"{workflow.name}: golangci-lint step has no version pin")
        makefile = (ROOT / "Makefile").read_text(encoding="utf-8")
        makefile_pin = re.search(r"^GOLANGCI_LINT_VERSION\s*\?=\s*v(\d+\.\d+\.\d+)", makefile, re.MULTILINE)
        assert makefile_pin is not None
        pins.add(makefile_pin.group(1))

        self.assertEqual(len(pins), 1, f"golangci-lint pins disagree: {sorted(pins)}")
        pin = tuple(int(part) for part in pins.pop().split("."))
        self.assertIn(pin, built_with, "record which Go minor this golangci-lint release was built with")
        self.assertGreaterEqual(built_with[pin], required)


if __name__ == "__main__":
    unittest.main()
