#!/usr/bin/env python3
"""Create and validate digest-authoritative Portage Engine release manifests."""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import re
import sys
from datetime import datetime
from pathlib import Path, PurePosixPath
from typing import Any


SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
RELEASE_RE = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$")
NAME_RE = re.compile(r"^[a-z0-9]+(?:[.-][a-z0-9]+)*$")
REPOSITORY_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
TIME_RE = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")
REF_RE = re.compile(r"^refs/(?:heads|tags)/[0-9A-Za-z][0-9A-Za-z._/-]*$")


class ManifestError(ValueError):
    pass


def reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ManifestError(f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=reject_duplicate_keys)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ManifestError(f"cannot load {path}: {exc}") from exc


def canonical_bytes(value: Any) -> bytes:
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")


def write_canonical(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(canonical_bytes(value))


def file_digest(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return "sha256:" + digest.hexdigest()


def exact_keys(value: Any, required: set[str], context: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ManifestError(f"{context} must be an object")
    actual = set(value)
    if actual != required:
        missing = sorted(required - actual)
        unknown = sorted(actual - required)
        raise ManifestError(f"{context} keys differ (missing={missing}, unknown={unknown})")
    return value


def require_string(value: Any, pattern: re.Pattern[str], context: str) -> str:
    if not isinstance(value, str) or not pattern.fullmatch(value):
        raise ManifestError(f"{context} is invalid")
    return value


def require_digest(value: Any, context: str) -> str:
    digest = require_string(value, SHA256_RE, context)
    if digest == "sha256:" + "0" * 64:
        raise ManifestError(f"{context} may not be a placeholder digest")
    return digest


def require_timestamp(value: Any, context: str) -> str:
    timestamp = require_string(value, TIME_RE, context)
    try:
        datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%SZ")
    except ValueError as exc:
        raise ManifestError(f"{context} is not a real UTC timestamp") from exc
    return timestamp


def parsed_timestamp(value: str) -> datetime:
    return datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ")


def safe_artifact(root: Path, relative: Any, expected_digest: Any, context: str) -> str:
    if not isinstance(relative, str) or not relative or "\\" in relative:
        raise ManifestError(f"{context}.path is invalid")
    pure = PurePosixPath(relative)
    if pure.is_absolute() or any(part in ("", ".", "..") for part in pure.parts):
        raise ManifestError(f"{context}.path is not a confined relative path")
    root_resolved = root.resolve(strict=True)
    unresolved = root
    for part in pure.parts:
        unresolved /= part
        if unresolved.is_symlink():
            raise ManifestError(f"{context}.path contains a symlink")
    path = unresolved.resolve(strict=True)
    try:
        path.relative_to(root_resolved)
    except ValueError as exc:
        raise ManifestError(f"{context}.path escapes the evidence root") from exc
    if not path.is_file():
        raise ManifestError(f"{context}.path is not a regular non-symlink file")
    digest = require_digest(expected_digest, f"{context}.sha256")
    if file_digest(path) != digest:
        raise ManifestError(f"{context}.sha256 does not bind {relative}")
    return relative


def load_config(path: Path) -> dict[str, Any]:
    config = exact_keys(
        load_json(path),
        {"schema_version", "registry", "manifest_repository_suffix", "platforms", "binary_platforms", "commands", "roles"},
        "release config",
    )
    if config["schema_version"] != 1 or config["registry"] != "ghcr.io":
        raise ManifestError("release config schema or registry is unsupported")
    require_string(config["manifest_repository_suffix"], NAME_RE, "manifest repository suffix")
    if config["platforms"] != sorted(set(config["platforms"])) or not config["platforms"]:
        raise ManifestError("release image platforms must be non-empty, unique, and sorted")
    for platform in config["platforms"]:
        if platform not in ("linux/amd64", "linux/arm64"):
            raise ManifestError(f"unsupported release image platform {platform!r}")
    if not isinstance(config["binary_platforms"], list) or not config["binary_platforms"]:
        raise ManifestError("binary platforms must be non-empty")
    binary_platforms: list[tuple[str, str]] = []
    for index, platform in enumerate(config["binary_platforms"]):
        item = exact_keys(platform, {"os", "arch"}, f"binary_platforms[{index}]")
        if item["os"] not in ("darwin", "linux") or item["arch"] not in ("amd64", "arm64"):
            raise ManifestError("unsupported binary platform")
        binary_platforms.append((item["os"], item["arch"]))
    if binary_platforms != sorted(set(binary_platforms)):
        raise ManifestError("binary platforms must be unique and sorted")
    commands = config["commands"]
    if not isinstance(commands, list) or commands != sorted(set(commands)) or not commands:
        raise ManifestError("commands must be non-empty, unique, and sorted")
    for command in commands:
        require_string(command, NAME_RE, "command")
    roles = config["roles"]
    if not isinstance(roles, list) or not roles:
        raise ManifestError("roles must be non-empty")
    role_names: list[str] = []
    targets: set[str] = set()
    for index, role in enumerate(roles):
        item = exact_keys(role, {"name", "target", "binary"}, f"roles[{index}]")
        role_names.append(require_string(item["name"], NAME_RE, f"roles[{index}].name"))
        require_string(item["target"], NAME_RE, f"roles[{index}].target")
        require_string(item["binary"], NAME_RE, f"roles[{index}].binary")
        if item["target"] in targets:
            raise ManifestError("Docker targets must be unique")
        targets.add(item["target"])
    if role_names != sorted(set(role_names)):
        raise ManifestError("roles must be unique and sorted")
    return config


def validate_descriptor(value: Any, root: Path, context: str) -> dict[str, Any]:
    descriptor = exact_keys(value, {"path", "sha256"}, context)
    safe_artifact(root, descriptor["path"], descriptor["sha256"], context)
    return descriptor


def parse_checksum_file(path: Path, root: Path, context: str) -> dict[str, str]:
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError) as exc:
        raise ManifestError(f"cannot read {context}: {exc}") from exc
    if not lines:
        raise ManifestError(f"{context} must not be empty")
    result: dict[str, str] = {}
    paths: list[str] = []
    for line in lines:
        match = re.fullmatch(r"([0-9a-f]{64})  ([^\s]+)", line)
        if not match:
            raise ManifestError(f"{context} contains a malformed checksum line")
        relative = match.group(2)
        if relative in result:
            raise ManifestError(f"{context} contains duplicate path {relative!r}")
        safe_artifact(root, relative, "sha256:" + match.group(1), context)
        result[relative] = "sha256:" + match.group(1)
        paths.append(relative)
    if paths != sorted(paths):
        raise ManifestError(f"{context} must be sorted by path")
    return result


def validate_manifest(manifest: Any, root: Path, config: dict[str, Any]) -> dict[str, Any]:
    value = exact_keys(manifest, {"schema_version", "release", "registry", "images", "binaries", "checksums", "transition"}, "manifest")
    if value["schema_version"] != 1:
        raise ManifestError("unsupported manifest schema_version")

    release = exact_keys(value["release"], {"id", "channel", "operation", "source_repository", "source_commit", "source_ref", "created_at"}, "release")
    require_string(release["id"], RELEASE_RE, "release.id")
    if len(release["id"]) > 64:
        raise ManifestError("release.id exceeds 64 characters")
    require_string(release["source_repository"], REPOSITORY_RE, "release.source_repository")
    require_string(release["source_commit"], COMMIT_RE, "release.source_commit")
    require_string(release["source_ref"], REF_RE, "release.source_ref")
    require_timestamp(release["created_at"], "release.created_at")
    if ".." in release["source_ref"] or "//" in release["source_ref"] or "@{" in release["source_ref"]:
        raise ManifestError("release.source_ref contains a forbidden Git ref sequence")
    if release["channel"] not in ("candidate", "stable") or release["operation"] not in ("build", "promotion", "rollback"):
        raise ManifestError("release channel or operation is invalid")

    registry = exact_keys(value["registry"], {"host", "namespace", "release_repository"}, "registry")
    expected_namespace = config["registry"] + "/" + release["source_repository"].lower()
    expected_release_repository = expected_namespace + "/" + config["manifest_repository_suffix"]
    if registry != {"host": config["registry"], "namespace": expected_namespace, "release_repository": expected_release_repository}:
        raise ManifestError("registry naming does not match the configured GHCR convention")

    roles = {role["name"]: role for role in config["roles"]}
    images = value["images"]
    if not isinstance(images, list) or [item.get("role") if isinstance(item, dict) else None for item in images] != sorted(roles):
        raise ManifestError("images must contain every configured role exactly once in sorted order")
    evidence: dict[str, str] = {}
    for index, image in enumerate(images):
        item = exact_keys(image, {"role", "target", "repository", "digest", "reference", "platforms", "sbom", "provenance"}, f"images[{index}]")
        role = roles.get(item["role"])
        if role is None or item["target"] != role["target"]:
            raise ManifestError(f"images[{index}] does not match the configured Docker role")
        expected_repository = expected_namespace + "/" + item["role"]
        digest = require_digest(item["digest"], f"images[{index}].digest")
        if item["repository"] != expected_repository or item["reference"] != expected_repository + "@" + digest:
            raise ManifestError(f"images[{index}] repository/reference is not digest-bound")
        if item["platforms"] != config["platforms"]:
            raise ManifestError(f"images[{index}] does not contain the complete multi-arch platform set")
        for kind in ("sbom", "provenance"):
            descriptor = validate_descriptor(item[kind], root, f"images[{index}].{kind}")
            if descriptor["path"] in evidence:
                raise ManifestError("evidence paths must be unique")
            evidence[descriptor["path"]] = descriptor["sha256"]

    binaries = value["binaries"]
    expected_binary_keys = sorted(
        ("portage-" + command, platform["os"], platform["arch"])
        for command in config["commands"]
        for platform in config["binary_platforms"]
    )
    actual_binary_keys: list[tuple[str, str, str]] = []
    binary_digests: dict[str, str] = {}
    if not isinstance(binaries, list):
        raise ManifestError("binaries must be an array")
    for index, binary in enumerate(binaries):
        item = exact_keys(binary, {"name", "os", "arch", "path", "sha256"}, f"binaries[{index}]")
        key = (item["name"], item["os"], item["arch"])
        actual_binary_keys.append(key)
        expected_path = f"binaries/{item['name']}-{item['os']}-{item['arch']}"
        if item["path"] != expected_path:
            raise ManifestError(f"binaries[{index}] path does not match its identity")
        safe_artifact(root, item["path"], item["sha256"], f"binaries[{index}]")
        binary_digests[item["path"]] = item["sha256"]
    if actual_binary_keys != expected_binary_keys:
        raise ManifestError("binaries do not contain the exact configured command/platform matrix in sorted order")

    checksums = exact_keys(value["checksums"], {"binaries", "evidence"}, "checksums")
    binary_checksum = validate_descriptor(checksums["binaries"], root, "checksums.binaries")
    evidence_checksum = validate_descriptor(checksums["evidence"], root, "checksums.evidence")
    parsed_binaries = parse_checksum_file(root / binary_checksum["path"], root, "binary checksums")
    parsed_evidence = parse_checksum_file(root / evidence_checksum["path"], root, "evidence checksums")
    if parsed_binaries != binary_digests:
        raise ManifestError("binary checksum file and manifest binary inventory differ")
    if parsed_evidence != evidence:
        raise ManifestError("evidence checksum file and manifest evidence inventory differ")

    transition = value["transition"]
    transition_type = transition.get("type") if isinstance(transition, dict) else None
    if release["channel"] == "candidate" or release["operation"] == "build":
        if release["channel"] != "candidate" or release["operation"] != "build" or transition != {"type": "build"}:
            raise ManifestError("candidate manifests must have the build transition")
    elif release["operation"] == "promotion":
        item = exact_keys(transition, {"type", "candidate_manifest_digest", "candidate_oci_digest", "expected_previous_stable_manifest_digest", "expected_previous_stable_oci_digest"}, "transition")
        if transition_type != "promotion":
            raise ManifestError("promotion transition type is invalid")
        require_digest(item["candidate_manifest_digest"], "transition.candidate_manifest_digest")
        require_digest(item["candidate_oci_digest"], "transition.candidate_oci_digest")
        previous = item["expected_previous_stable_manifest_digest"]
        if previous != "none":
            require_digest(previous, "transition.expected_previous_stable_manifest_digest")
        previous_oci = item["expected_previous_stable_oci_digest"]
        if previous_oci != "none":
            require_digest(previous_oci, "transition.expected_previous_stable_oci_digest")
        if (previous == "none") != (previous_oci == "none"):
            raise ManifestError("promotion previous canonical and OCI digests must both be none or both be set")
    else:
        item = exact_keys(transition, {"type", "target_manifest_digest", "target_oci_digest", "expected_previous_stable_manifest_digest", "expected_previous_stable_oci_digest", "reason"}, "transition")
        if transition_type != "rollback":
            raise ManifestError("rollback transition type is invalid")
        require_digest(item["target_manifest_digest"], "transition.target_manifest_digest")
        require_digest(item["target_oci_digest"], "transition.target_oci_digest")
        require_digest(item["expected_previous_stable_manifest_digest"], "transition.expected_previous_stable_manifest_digest")
        require_digest(item["expected_previous_stable_oci_digest"], "transition.expected_previous_stable_oci_digest")
        if not isinstance(item["reason"], str) or not 1 <= len(item["reason"]) <= 512 or any(ord(char) < 32 for char in item["reason"]):
            raise ManifestError("rollback reason must be printable and between 1 and 512 characters")
    return value


def load_canonical_manifest(path: Path, root: Path, config: dict[str, Any]) -> dict[str, Any]:
    manifest = validate_manifest(load_json(path), root, config)
    if path.read_bytes() != canonical_bytes(manifest):
        raise ManifestError(f"{path} is not in canonical sorted compact JSON form")
    return manifest


def reference_config(override: str | None, config: dict[str, Any]) -> dict[str, Any]:
    # A signed bundle carries the release config it was built against and is
    # validated against that copy, never the caller's working tree. The role set
    # and the command/platform matrix are exact-equality assertions, so without
    # this the first merge that adds a cmd/ target would strand every manifest
    # signed before it: un-promotable, and unverifiable by its own operators.
    if not override:
        return config
    return load_config(Path(override))


def load_inventory(path: Path, context: str) -> list[dict[str, Any]]:
    value = load_json(path)
    if not isinstance(value, list):
        raise ManifestError(f"{context} must be an array")
    return value


def cmd_index_binaries(args: argparse.Namespace, config: dict[str, Any]) -> None:
    root = Path(args.root)
    result: list[dict[str, Any]] = []
    for command in config["commands"]:
        name = "portage-" + command
        for platform in config["binary_platforms"]:
            relative = f"binaries/{name}-{platform['os']}-{platform['arch']}"
            path = root / relative
            if not path.is_file():
                raise ManifestError(f"missing release binary {relative}")
            result.append({"name": name, "os": platform["os"], "arch": platform["arch"], "path": relative, "sha256": file_digest(path)})
    result.sort(key=lambda item: (item["name"], item["os"], item["arch"]))
    write_canonical(Path(args.output), result)


def cmd_create_candidate(args: argparse.Namespace, config: dict[str, Any]) -> None:
    repository = require_string(args.source_repository, REPOSITORY_RE, "source repository")
    namespace = config["registry"] + "/" + repository.lower()
    manifest = {
        "schema_version": 1,
        "release": {
            "id": args.release_id,
            "channel": "candidate",
            "operation": "build",
            "source_repository": repository,
            "source_commit": args.source_commit,
            "source_ref": args.source_ref,
            "created_at": args.created_at,
        },
        "registry": {
            "host": config["registry"],
            "namespace": namespace,
            "release_repository": namespace + "/" + config["manifest_repository_suffix"],
        },
        "images": load_inventory(Path(args.images), "image inventory"),
        "binaries": load_inventory(Path(args.binaries), "binary inventory"),
        "checksums": load_json(Path(args.checksums)),
        "transition": {"type": "build"},
    }
    validate_manifest(manifest, Path(args.root), config)
    write_canonical(Path(args.output), manifest)


def cmd_validate(args: argparse.Namespace, config: dict[str, Any]) -> None:
    path = Path(args.manifest)
    load_canonical_manifest(path, Path(args.root), config)
    print(file_digest(path))


def cmd_promote(args: argparse.Namespace, config: dict[str, Any]) -> None:
    candidate_path = Path(args.candidate)
    candidate = load_canonical_manifest(candidate_path, Path(args.candidate_root), config)
    if candidate["release"]["channel"] != "candidate":
        raise ManifestError("promotion input must be a candidate manifest")
    expected = args.expected_previous_manifest_digest
    expected_oci = args.expected_previous_oci_digest
    current_path = Path(args.current_stable) if args.current_stable else None
    current: dict[str, Any] | None = None
    if expected == "none":
        if expected_oci != "none" or current_path is not None:
            raise ManifestError("first promotion may not supply a current stable manifest")
    else:
        require_digest(expected, "expected previous stable manifest digest")
        require_digest(expected_oci, "expected previous stable OCI digest")
        if current_path is None or not args.current_root:
            raise ManifestError("promotion CAS requires the current stable manifest and root")
        current = load_canonical_manifest(
            current_path,
            Path(args.current_root),
            reference_config(args.current_config, config),
        )
        if current["release"]["channel"] != "stable" or file_digest(current_path) != expected:
            raise ManifestError("current stable manifest does not satisfy the promotion CAS digest")
        if current["registry"] != candidate["registry"]:
            raise ManifestError("candidate and current stable registry authority differ")
    require_digest(args.candidate_oci_digest, "candidate OCI digest")
    promoted_at = require_timestamp(args.promoted_at, "promoted_at")
    if parsed_timestamp(promoted_at) < parsed_timestamp(candidate["release"]["created_at"]):
        raise ManifestError("promotion time precedes candidate creation")
    if current is not None and parsed_timestamp(promoted_at) < parsed_timestamp(current["release"]["created_at"]):
        raise ManifestError("promotion time precedes the current stable revision")
    result = copy.deepcopy(candidate)
    result["release"]["channel"] = "stable"
    result["release"]["operation"] = "promotion"
    result["release"]["created_at"] = promoted_at
    result["transition"] = {
        "type": "promotion",
        "candidate_manifest_digest": file_digest(candidate_path),
        "candidate_oci_digest": args.candidate_oci_digest,
        "expected_previous_stable_manifest_digest": expected,
        "expected_previous_stable_oci_digest": expected_oci,
    }
    validate_manifest(result, Path(args.output_root), config)
    write_canonical(Path(args.output), result)


def cmd_rollback(args: argparse.Namespace, config: dict[str, Any]) -> None:
    current_path = Path(args.current)
    target_path = Path(args.target)
    current = load_canonical_manifest(
        current_path,
        Path(args.current_root),
        reference_config(args.current_config, config),
    )
    target = load_canonical_manifest(target_path, Path(args.target_root), config)
    if current["release"]["channel"] != "stable" or target["release"]["channel"] != "stable":
        raise ManifestError("rollback requires stable current and target manifests")
    expected = require_digest(args.expected_current_manifest_digest, "expected current manifest digest")
    expected_oci = require_digest(args.expected_current_oci_digest, "expected current OCI digest")
    if file_digest(current_path) != expected:
        raise ManifestError("current stable manifest does not satisfy the rollback CAS digest")
    target_digest = file_digest(target_path)
    if target_digest == expected:
        raise ManifestError("rollback target must differ from the current stable manifest")
    if current["registry"] != target["registry"]:
        raise ManifestError("rollback target and current stable registry authority differ")
    require_digest(args.target_oci_digest, "rollback target OCI digest")
    rolled_back_at = require_timestamp(args.rolled_back_at, "rolled_back_at")
    if parsed_timestamp(rolled_back_at) < parsed_timestamp(current["release"]["created_at"]):
        raise ManifestError("rollback time precedes the current stable revision")
    result = copy.deepcopy(target)
    result["release"]["operation"] = "rollback"
    result["release"]["created_at"] = rolled_back_at
    result["transition"] = {
        "type": "rollback",
        "target_manifest_digest": target_digest,
        "target_oci_digest": args.target_oci_digest,
        "expected_previous_stable_manifest_digest": expected,
        "expected_previous_stable_oci_digest": expected_oci,
        "reason": args.reason,
    }
    validate_manifest(result, Path(args.output_root), config)
    write_canonical(Path(args.output), result)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", default="release/release-config.json")
    subparsers = parser.add_subparsers(dest="command", required=True)

    index = subparsers.add_parser("index-binaries")
    index.add_argument("--root", required=True)
    index.add_argument("--output", required=True)

    create = subparsers.add_parser("create-candidate")
    create.add_argument("--release-id", required=True)
    create.add_argument("--source-repository", required=True)
    create.add_argument("--source-commit", required=True)
    create.add_argument("--source-ref", required=True)
    create.add_argument("--created-at", required=True)
    create.add_argument("--images", required=True)
    create.add_argument("--binaries", required=True)
    create.add_argument("--checksums", required=True)
    create.add_argument("--root", required=True)
    create.add_argument("--output", required=True)

    validate = subparsers.add_parser("validate")
    validate.add_argument("--manifest", required=True)
    validate.add_argument("--root", required=True)

    promote = subparsers.add_parser("promote")
    promote.add_argument("--candidate", required=True)
    promote.add_argument("--candidate-root", required=True)
    promote.add_argument("--candidate-oci-digest", required=True)
    promote.add_argument("--expected-previous-manifest-digest", required=True)
    promote.add_argument("--expected-previous-oci-digest", required=True)
    promote.add_argument("--current-stable")
    promote.add_argument("--current-root")
    promote.add_argument("--current-config")
    promote.add_argument("--promoted-at", required=True)
    promote.add_argument("--output-root", required=True)
    promote.add_argument("--output", required=True)

    rollback = subparsers.add_parser("rollback")
    rollback.add_argument("--current", required=True)
    rollback.add_argument("--current-root", required=True)
    rollback.add_argument("--current-config")
    rollback.add_argument("--target", required=True)
    rollback.add_argument("--target-root", required=True)
    rollback.add_argument("--target-oci-digest", required=True)
    rollback.add_argument("--expected-current-manifest-digest", required=True)
    rollback.add_argument("--expected-current-oci-digest", required=True)
    rollback.add_argument("--reason", required=True)
    rollback.add_argument("--rolled-back-at", required=True)
    rollback.add_argument("--output-root", required=True)
    rollback.add_argument("--output", required=True)
    return parser


def main() -> int:
    args = build_parser().parse_args()
    try:
        config = load_config(Path(args.config))
        commands = {
            "index-binaries": cmd_index_binaries,
            "create-candidate": cmd_create_candidate,
            "validate": cmd_validate,
            "promote": cmd_promote,
            "rollback": cmd_rollback,
        }
        commands[args.command](args, config)
        return 0
    except (ManifestError, OSError) as exc:
        print(f"release manifest error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
