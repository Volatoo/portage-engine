#!/usr/bin/env python3
"""Fail-closed validators for recovery-drill operator evidence."""

from __future__ import annotations

import argparse
import json
import re
from pathlib import Path
from typing import Any, Iterable


READ_ACTIONS = {
    "s3:getobject",
    "s3:getobjectattributes",
    "s3:getobjectversion",
    "s3:listbucket",
    "s3:listbucketversions",
}
WRITE_ACTIONS = {"s3:putobject", "s3:abortmultipartupload"}
DELETE_ACTIONS = {"s3:deleteobject", "s3:deleteobjectversion"}


def fail(message: str) -> None:
    raise SystemExit(message)


def load_json(path: str) -> dict[str, Any]:
    try:
        value = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        fail(f"cannot read JSON evidence {path}: {error}")
    if not isinstance(value, dict):
        fail(f"JSON evidence {path} must be an object")
    return value


def unwrap_vault(document: dict[str, Any]) -> dict[str, Any]:
    data = document.get("data", document)
    if isinstance(data, dict) and isinstance(data.get("data"), dict):
        data = data["data"]
    if not isinstance(data, dict):
        fail("Vault evidence has no data object")
    return data


def validate_vault_status(arguments: argparse.Namespace) -> None:
    status = load_json(arguments.file)
    if status.get("initialized") is not True:
        fail("Vault is not initialized")
    if status.get("sealed") is not False:
        fail("Vault is sealed")
    if status.get("ha_enabled") is not True:
        fail("Vault HA is not enabled")
    if str(status.get("storage_type", "")).lower() in {"", "file", "inmem"}:
        fail("Vault status does not prove a durable HA storage backend")


def values(value: Any) -> list[str]:
    if value is None:
        return []
    if isinstance(value, list):
        return [str(item).strip() for item in value if str(item).strip()]
    return [item.strip() for item in str(value).split(",") if item.strip()]


def validate_vault_role(arguments: argparse.Namespace) -> None:
    role = unwrap_vault(load_json(arguments.file))
    unsafe_true = (
        "allow_any_name", "allow_glob_domains", "allow_subdomains",
        "server_flag", "code_signing_flag", "email_protection_flag",
    )
    for field in unsafe_true:
        if role.get(field) is True:
            fail(f"Vault PKI role must set {field}=false")
    if role.get("client_flag") is not True:
        fail("Vault PKI role must set client_flag=true")
    if role.get("use_csr_sans") is not True:
        fail("Vault PKI role must preserve the attempt-bound CSR URI SAN")
    if role.get("use_csr_common_name") is not True:
        fail("Vault PKI role must preserve the exact worker CSR common name")
    common_names = values(role.get("allowed_domains"))
    if common_names != [arguments.allowed_common_name] or role.get("allow_bare_domains") is not True:
        fail("Vault PKI role must allow only the exact bare worker common name")
    if values(role.get("allowed_other_sans")):
        fail("Vault PKI role must not allow arbitrary other SANs")
    allowed = values(role.get("allowed_uri_sans"))
    expected = arguments.allowed_uri_san
    if allowed != [expected]:
        fail(f"allowed_uri_sans must be exactly {expected!r}, got {allowed!r}")
    key_type = str(role.get("key_type", "")).lower()
    key_bits = int(role.get("key_bits", 0) or 0)
    if key_type == "ec" and key_bits in {256, 384}:
        pass
    elif key_type != "ed25519":
        fail("Vault PKI role must require an Ed25519, P-256, or P-384 CSR key")
    max_ttl = int(role.get("max_ttl", 0) or 0)
    if max_ttl <= 0 or max_ttl > 24 * 60 * 60:
        fail("Vault PKI role max_ttl must be positive and no more than 24h")


def statements(document: dict[str, Any]) -> Iterable[dict[str, Any]]:
    body = document.get("Policy", document)
    if isinstance(body, str):
        try:
            body = json.loads(body)
        except json.JSONDecodeError as error:
            fail(f"embedded IAM Policy is invalid JSON: {error}")
    entries = body.get("Statement", []) if isinstance(body, dict) else []
    if isinstance(entries, dict):
        entries = [entries]
    if not isinstance(entries, list) or not entries:
        fail("IAM evidence must contain at least one Statement")
    for statement in entries:
        if not isinstance(statement, dict):
            fail("IAM Statement entries must be objects")
        if "NotAction" in statement or "NotResource" in statement:
            fail("IAM NotAction/NotResource cannot prove a least-privilege Gate")
        yield statement


def actions(statement: dict[str, Any]) -> set[str]:
    raw = statement.get("Action", [])
    if isinstance(raw, str):
        raw = [raw]
    if not isinstance(raw, list):
        fail("IAM Action must be a string or list")
    result = {str(action).lower() for action in raw}
    if any("*" in action for action in result):
        fail("wildcard IAM actions are forbidden in drill evidence")
    return result


def resources(statement: dict[str, Any]) -> list[str]:
    raw = statement.get("Resource", [])
    if isinstance(raw, str):
        raw = [raw]
    if not isinstance(raw, list) or not raw:
        fail("IAM Resource must be a non-empty string or list")
    return [str(resource) for resource in raw]


def allowed_statement(statement: dict[str, Any]) -> bool:
    return str(statement.get("Effect", "")).lower() == "allow"


def denied_statement(statement: dict[str, Any]) -> bool:
    return str(statement.get("Effect", "")).lower() == "deny"


def inspect_policy(path: str) -> tuple[set[str], set[str], list[tuple[set[str], list[str]]]]:
    allow_actions: set[str] = set()
    deny_actions: set[str] = set()
    allowed: list[tuple[set[str], list[str]]] = []
    for statement in statements(load_json(path)):
        current_actions = actions(statement)
        if allowed_statement(statement):
            allow_actions.update(current_actions)
            allowed.append((current_actions, resources(statement)))
        elif denied_statement(statement):
            deny_actions.update(current_actions)
        else:
            fail("IAM Statement Effect must be exactly Allow or Deny")
    return allow_actions, deny_actions, allowed


def granted(allow_actions: set[str], deny_actions: set[str]) -> set[str]:
    # An explicit Deny always wins in IAM, so a capability is only proven by an
    # Allow that no other statement takes back, and a forbidden capability is
    # only present when it survives the Denies. Folding both effects into one
    # action set certified signers that provably could not PutObject and
    # rejected read policies that had been narrowed with a redundant Deny.
    return allow_actions - deny_actions


def require_delete_scope(
    name: str,
    allowed: list[tuple[set[str], list[str]]],
    denied: set[str],
    marker: str,
) -> None:
    found = False
    for current_actions, current_resources in allowed:
        if current_actions & (DELETE_ACTIONS - denied):
            found = True
            for resource in current_resources:
                if marker not in resource or not resource.endswith("*"):
                    fail(f"{name} DeleteObject resource is not confined to {marker}*: {resource}")
    if not found:
        fail(f"{name} policy does not grant its required scoped DeleteObject")


def require_mutation_scope(
    name: str,
    allowed: list[tuple[set[str], list[str]]],
    relevant_actions: set[str],
    marker: str,
) -> None:
    for current_actions, current_resources in allowed:
        if current_actions & relevant_actions:
            for resource in current_resources:
                if marker not in resource or not resource.endswith("*"):
                    fail(f"{name} mutation resource is not confined to {marker}*: {resource}")


def reject_delete(name: str, allowed: list[tuple[set[str], list[str]]]) -> None:
    if any(current & DELETE_ACTIONS for current, _ in allowed):
        fail(f"{name} policy must not grant DeleteObject")


def validate_object_policies(arguments: argparse.Namespace) -> None:
    principals = [
        arguments.read_principal,
        arguments.executor_principal,
        arguments.signer_principal,
        arguments.quarantine_gc_principal,
        arguments.generation_gc_principal,
    ]
    if len(set(principals)) != len(principals):
        fail("read, executor, signer, quarantine-GC, and generation-GC principals must be distinct")

    read_allow, read_deny, read_allowed = inspect_policy(arguments.read_policy)
    read_actions = granted(read_allow, read_deny)
    if not (read_actions & READ_ACTIONS):
        fail("read/API policy has no S3 read permission")
    if read_actions & (WRITE_ACTIONS | DELETE_ACTIONS):
        fail("read/API policy contains object write or delete permission")
    reject_delete("read/API", read_allowed)

    for name, path in (("executor", arguments.executor_policy),):
        allow, deny, current_allowed = inspect_policy(path)
        current_actions = granted(allow, deny)
        if not (current_actions & READ_ACTIONS) or not (current_actions & WRITE_ACTIONS):
            fail(f"{name} policy must have explicit read and create permissions")
        require_delete_scope(name, current_allowed, deny, "/.quarantine/")

    signer_allow, signer_deny, signer_allowed = inspect_policy(arguments.signer_policy)
    signer_actions = granted(signer_allow, signer_deny)
    if not (signer_actions & READ_ACTIONS) or not (signer_actions & WRITE_ACTIONS):
        fail("signer policy must have explicit read and create permissions")
    require_delete_scope("signer", signer_allowed, signer_deny, "/.quarantine/")
    require_mutation_scope(
        "signer", signer_allowed, WRITE_ACTIONS | DELETE_ACTIONS, "/.quarantine/"
    )

    quarantine_allow, quarantine_deny, quarantine_allowed = inspect_policy(
        arguments.quarantine_gc_policy
    )
    quarantine_actions = granted(quarantine_allow, quarantine_deny)
    if not (quarantine_actions & READ_ACTIONS) or quarantine_actions & WRITE_ACTIONS:
        fail("quarantine GC policy must be read+delete only")
    require_delete_scope(
        "quarantine GC", quarantine_allowed, quarantine_deny, "/.quarantine/"
    )
    generation_allow, generation_deny, generation_allowed = inspect_policy(
        arguments.generation_gc_policy
    )
    generation_actions = granted(generation_allow, generation_deny)
    if not (generation_actions & READ_ACTIONS) or generation_actions & WRITE_ACTIONS:
        fail("generation GC policy must be read+delete only")
    require_delete_scope(
        "generation GC", generation_allowed, generation_deny, "/.generations/"
    )
    print(json.dumps({
        "status": "passed",
        "principals": principals,
        "delete_separation": [".quarantine/*", ".generations/*"],
    }, sort_keys=True))


def validate_encrypted_backup(arguments: argparse.Namespace) -> None:
    path = Path(arguments.file)
    if not path.is_file() or path.stat().st_size < 64:
        fail("encrypted signer backup is missing or implausibly small")
    prefix = path.read_bytes()[:64]
    # Keep private-key sentinels split so repository secret scanners do not
    # mistake this defensive detector for committed key material.
    if b"BEGIN PGP " + b"PRIVATE KEY BLOCK" in prefix:
        fail("signer backup appears to be an unencrypted private-key export")
    if re.search(br"BEGIN (RSA|EC|OPENSSH) PRIVATE KEY", prefix):
        fail("signer backup appears to contain a plaintext private key")
    packet_text = Path(arguments.packet_log).read_text(
        encoding="utf-8", errors="replace"
    ).lower()
    if "encrypted data packet" not in packet_text:
        fail("GPG packet evidence does not contain an encrypted data packet")
    if not ("pubkey enc packet" in packet_text or "symkey enc packet" in packet_text):
        fail("GPG packet evidence does not identify a key- or passphrase-encrypted envelope")
    print(json.dumps({"status": "passed", "size_bytes": path.stat().st_size}))


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser()
    commands = root.add_subparsers(dest="operation", required=True)
    vault_status = commands.add_parser("vault-status")
    vault_status.add_argument("--file", required=True)
    vault_status.set_defaults(function=validate_vault_status)

    vault_role = commands.add_parser("vault-role")
    vault_role.add_argument("--file", required=True)
    vault_role.add_argument("--allowed-uri-san", required=True)
    vault_role.add_argument("--allowed-common-name", required=True)
    vault_role.set_defaults(function=validate_vault_role)

    policies = commands.add_parser("object-policies")
    for name in (
        "read-policy", "executor-policy", "signer-policy",
        "quarantine-gc-policy", "generation-gc-policy", "read-principal",
        "executor-principal", "signer-principal", "quarantine-gc-principal",
        "generation-gc-principal",
    ):
        policies.add_argument(f"--{name}", required=True)
    policies.set_defaults(function=validate_object_policies)

    encrypted = commands.add_parser("encrypted-backup")
    encrypted.add_argument("--file", required=True)
    encrypted.add_argument("--packet-log", required=True)
    encrypted.set_defaults(function=validate_encrypted_backup)
    return root


def main() -> None:
    arguments = parser().parse_args()
    arguments.function(arguments)


if __name__ == "__main__":
    main()
