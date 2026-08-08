#!/usr/bin/env python3
"""Capability-limited guest helper for deterministic Desktop E2E.

This helper is invoked only through QEMU Guest Agent by the direct PVE driver.
It intentionally has no generic command execution action.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import pathlib
import pwd
import re
import shutil
import subprocess
import sys
import time
import urllib.parse
import urllib.request


DESKTOP_USER = os.environ.get("PE_DESKTOP_USER", "portage-e2e")
STAGING_ROOT = pathlib.Path("/var/lib/portage-engine/staging-binhost")
STAGING_DIGEST = pathlib.Path("/var/lib/portage-engine/staging-binhost.digest")
STAGING_SIGNER = pathlib.Path("/var/lib/portage-engine/staging-binhost.signer")
STAGING_GNUPG = pathlib.Path("/var/lib/portage-engine/staging-gnupg")
BINREPO_CONFIG = pathlib.Path("/etc/portage/binrepos.conf/portage-engine-staging.conf")
EVIDENCE_ROOT = pathlib.Path("/run/portage-engine/desktop-evidence")
FIXTURE_ROOT = pathlib.Path("/usr/share/portage-engine/desktop-fixtures")
BUILD_PLAN = pathlib.Path("/etc/portage-engine/build-plan.json")
ATOM_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9+._-]*/[A-Za-z0-9][A-Za-z0-9+._-]*$")
APP_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9+._-]{0,127}\.desktop$")
DIGEST_RE = re.compile(r"^sha256:[a-f0-9]{64}$")
FINGERPRINT_RE = re.compile(r"^(?:[A-F0-9]{40}|[A-F0-9]{64})$")
FIXTURE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
KEY_RE = re.compile(r"^[A-Za-z0-9+_-]{1,128}$")
BUDGET_RE = re.compile(r"^[0-9]{1,3}$")
# The fallback for a driver that sends no budget, and the ceiling, which is the
# scenario schema's own per-step maximum in internal/desktop/scenario.go.
DEFAULT_ACCESSIBILITY_BUDGET = 60.0
MAX_ACCESSIBILITY_BUDGET = 300
SAFE_PATH_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9+._/-]{0,511}$")
MAX_MANIFEST_BYTES = 4 * 1024 * 1024
MAX_OBJECTS = 10000
MAX_TOTAL_BYTES = 20 * 1024 * 1024 * 1024
MAX_EVIDENCE_BYTES = 32 * 1024 * 1024
MAX_EVIDENCE_CHUNK = 192 * 1024


def fail(message: str) -> "None":
    raise SystemExit(message)


def atomic_write(path: pathlib.Path, data: bytes, mode: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    with temporary.open("wb") as stream:
        os.fchmod(stream.fileno(), mode)
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)


def checked_url(origin: str, relative: str) -> str:
    parsed = urllib.parse.urlsplit(origin)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        fail("staging origin must use HTTP(S)")
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        fail("staging origin must not contain credentials, query, or fragment")
    if not SAFE_PATH_RE.fullmatch(relative) or pathlib.PurePosixPath(relative).is_absolute() or ".." in pathlib.PurePosixPath(relative).parts:
        fail("staging manifest contains an unsafe path")
    base = origin.rstrip("/") + "/"
    result = urllib.parse.urljoin(base, relative)
    target = urllib.parse.urlsplit(result)
    if (target.scheme, target.hostname, target.port) != (parsed.scheme, parsed.hostname, parsed.port):
        fail("staging object escaped the reviewed origin")
    return result


def fetch(url: str, limit: int) -> bytes:
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, request, fp, code, message, headers, new_url):
            fail("staging server redirect is forbidden")

    request = urllib.request.Request(url, headers={"User-Agent": "portage-desktop-agent/1"})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=60) as response:
        if response.geturl() != url:
            fail("staging server redirect is forbidden")
        data = response.read(limit + 1)
    if len(data) > limit:
        fail("staging object exceeds reviewed size")
    return data


def primary_fingerprints(colon_output: str) -> list[str]:
    fingerprints: list[str] = []
    awaiting_primary = False
    for line in colon_output.splitlines():
        fields = line.split(":")
        if not fields:
            continue
        if fields[0] == "pub":
            awaiting_primary = True
        elif fields[0] == "sub":
            awaiting_primary = False
        elif fields[0] == "fpr" and awaiting_primary and len(fields) > 9:
            fingerprints.append(fields[9])
            awaiting_primary = False
    return fingerprints


def configure_staging_signer(key_path: str, expected_fingerprint: str) -> None:
    if not FIXTURE_RE.fullmatch(key_path) or not FINGERPRINT_RE.fullmatch(expected_fingerprint):
        fail("invalid staging signer policy")
    source = STAGING_ROOT / key_path
    try:
        info = source.lstat()
    except FileNotFoundError:
        fail("staging signer key is missing from the locked manifest")
    if not source.is_file() or source.is_symlink() or info.st_size < 1 or info.st_size > 1024 * 1024:
        fail("staging signer key is not a bounded regular file")
    if STAGING_GNUPG.exists():
        shutil.rmtree(STAGING_GNUPG)
    STAGING_GNUPG.mkdir(parents=True, mode=0o700)
    os.chmod(STAGING_GNUPG, 0o700)
    inspect = subprocess.run(
        [
            "/usr/bin/gpg",
            "--homedir",
            str(STAGING_GNUPG),
            "--batch",
            "--with-colons",
            "--import-options",
            "show-only",
            "--import",
            str(source),
        ],
        check=False,
        text=True,
        capture_output=True,
        timeout=30,
    )
    fingerprints = primary_fingerprints(inspect.stdout)
    if inspect.returncode != 0 or fingerprints != [expected_fingerprint]:
        fail("staging signer key does not match the reviewed primary fingerprint")
    subprocess.run(
        ["/usr/bin/gpg", "--homedir", str(STAGING_GNUPG), "--batch", "--import", str(source)],
        check=True,
        text=True,
        capture_output=True,
        timeout=30,
    )
    subprocess.run(
        ["/usr/bin/gpg", "--homedir", str(STAGING_GNUPG), "--batch", "--import-ownertrust"],
        input=f"{expected_fingerprint}:6:\n",
        check=True,
        text=True,
        capture_output=True,
        timeout=30,
    )
    nobody = pwd.getpwnam("nobody")
    for root, directories, files in os.walk(STAGING_GNUPG):
        os.chown(root, nobody.pw_uid, nobody.pw_gid)
        for name in directories + files:
            os.chown(pathlib.Path(root) / name, nobody.pw_uid, nobody.pw_gid)
    atomic_write(STAGING_SIGNER, (expected_fingerprint + "\n").encode(), 0o644)


def hydrate_staging(origin: str, expected_digest: str, key_path: str = "", expected_fingerprint: str = "") -> None:
    if not DIGEST_RE.fullmatch(expected_digest):
        fail("invalid staging manifest digest")
    manifest_url = checked_url(origin, "MANIFEST.json")
    raw = fetch(manifest_url, MAX_MANIFEST_BYTES)
    actual = "sha256:" + hashlib.sha256(raw).hexdigest()
    if actual != expected_digest:
        fail("staging manifest digest mismatch")
    try:
        manifest = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        fail(f"invalid staging manifest: {error}")
    if not isinstance(manifest, dict) or set(manifest) != {"schema_version", "objects"} or manifest["schema_version"] != 1:
        fail("staging manifest must use the strict schema_version 1 contract")
    objects = manifest["objects"]
    if not isinstance(objects, list) or not objects or len(objects) > MAX_OBJECTS:
        fail("staging manifest object count is invalid")
    reviewed: list[tuple[str, int, str]] = []
    seen: set[str] = set()
    total = 0
    for item in objects:
        if not isinstance(item, dict) or set(item) != {"path", "size", "sha256"}:
            fail("staging object uses an unknown or missing field")
        path, size, digest = item["path"], item["size"], item["sha256"]
        if not isinstance(path, str) or not isinstance(size, int) or not isinstance(digest, str):
            fail("staging object fields have invalid types")
        checked_url(origin, path)
        if path in seen or size < 0 or size > MAX_TOTAL_BYTES or not re.fullmatch(r"[a-f0-9]{64}", digest):
            fail("staging object metadata is invalid")
        seen.add(path)
        total += size
        if total > MAX_TOTAL_BYTES:
            fail("staging snapshot exceeds the total size limit")
        reviewed.append((path, size, digest))
    if "Packages" not in seen:
        fail("staging snapshot must contain a Packages index")

    temporary = STAGING_ROOT.with_name(f".{STAGING_ROOT.name}.new-{os.getpid()}")
    if temporary.exists():
        shutil.rmtree(temporary)
    temporary.mkdir(parents=True, mode=0o755)
    try:
        for relative, size, digest in reviewed:
            data = fetch(checked_url(origin, relative), size)
            if len(data) != size or hashlib.sha256(data).hexdigest() != digest:
                fail(f"staging object verification failed: {relative}")
            destination = temporary / pathlib.PurePosixPath(relative)
            destination.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
            atomic_write(destination, data, 0o644)
        previous = STAGING_ROOT.with_name(f".{STAGING_ROOT.name}.old-{os.getpid()}")
        if previous.exists():
            shutil.rmtree(previous)
        if STAGING_ROOT.exists():
            os.replace(STAGING_ROOT, previous)
        os.replace(temporary, STAGING_ROOT)
        if previous.exists():
            shutil.rmtree(previous)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)
    atomic_write(STAGING_DIGEST, (expected_digest + "\n").encode(), 0o644)
    signed = bool(key_path or expected_fingerprint)
    if signed:
        configure_staging_signer(key_path, expected_fingerprint)
    else:
        STAGING_SIGNER.unlink(missing_ok=True)
        if STAGING_GNUPG.exists():
            shutil.rmtree(STAGING_GNUPG)
    atomic_write(
        BINREPO_CONFIG,
        (
            "[portage-engine-staging]\n"
            "sync-uri = file:///var/lib/portage-engine/staging-binhost\n"
            "priority = 1000\n"
            f"verify-signature = {'true' if signed else 'false'}\n"
        ).encode(),
        0o644,
    )


def desktop_identity() -> tuple[pwd.struct_passwd, dict[str, str]]:
    try:
        user = pwd.getpwnam(DESKTOP_USER)
    except KeyError:
        fail("reviewed desktop user is missing")
    runtime = f"/run/user/{user.pw_uid}"
    environment = {
        "HOME": user.pw_dir,
        "USER": user.pw_name,
        "LOGNAME": user.pw_name,
        "DISPLAY": ":0",
        "XAUTHORITY": str(pathlib.Path(user.pw_dir) / ".Xauthority"),
        "XDG_RUNTIME_DIR": runtime,
        "DBUS_SESSION_BUS_ADDRESS": f"unix:path={runtime}/bus",
        "PATH": "/usr/local/bin:/usr/bin:/bin",
    }
    return user, environment


def run_desktop(command: list[str], *, timeout: int = 60, check: bool = True) -> subprocess.CompletedProcess[str]:
    user, environment = desktop_identity()
    runuser = shutil.which("runuser")
    if runuser is None:
        fail("runuser is missing from the desktop image")
    arguments = [runuser, "--user", user.pw_name, "--", "/usr/bin/env"]
    arguments.extend(f"{key}={value}" for key, value in environment.items())
    arguments.extend(command)
    return subprocess.run(arguments, check=check, text=True, capture_output=True, timeout=timeout)


def user_process_running(user: pwd.struct_passwd, names: set[str]) -> bool:
    for process in pathlib.Path("/proc").iterdir():
        if not process.name.isdigit():
            continue
        try:
            if process.stat().st_uid != user.pw_uid:
                continue
            command = (process / "cmdline").read_bytes().split(b"\0", 1)[0]
            if pathlib.Path(os.fsdecode(command)).name in names:
                return True
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            continue
    return False


def ensure_evidence_root() -> None:
    user, _ = desktop_identity()
    EVIDENCE_ROOT.mkdir(parents=True, exist_ok=True, mode=0o750)
    os.chown(EVIDENCE_ROOT, user.pw_uid, user.pw_gid)
    os.chmod(EVIDENCE_ROOT, 0o750)


def desktop_ready() -> None:
    user, environment = desktop_identity()
    config = pathlib.Path(user.pw_dir) / ".config"
    try:
        config_info = config.stat()
    except FileNotFoundError:
        fail("desktop user configuration directory is missing")
    if config_info.st_uid != user.pw_uid or config_info.st_gid != user.pw_gid or config_info.st_mode & 0o200 == 0:
        fail("desktop user configuration directory is not owner-writable")
    if not pathlib.Path("/tmp/.X11-unix/X0").is_socket():
        fail("desktop X socket is not ready")
    if not pathlib.Path(environment["XAUTHORITY"]).is_file():
        fail("desktop X authority is not ready")
    if not pathlib.Path(environment["XDG_RUNTIME_DIR"] + "/bus").is_socket():
        fail("desktop user bus is not ready")
    result = run_desktop(["/usr/bin/xrandr", "--query"], timeout=10, check=False)
    if result.returncode != 0 or " connected" not in result.stdout:
        fail("desktop display is not ready")
    if not user_process_running(user, {"xfce4-session"}) or not user_process_running(user, {"xfwm4"}):
        fail("XFCE session and window manager are not ready")


def assert_image(profile_id: str, image_id: str, generation: str, display_server: str) -> None:
    if not all(SAFE_PATH_RE.fullmatch(value) for value in (profile_id, image_id, generation)):
        fail("invalid expected desktop image identity")
    if display_server != "x11":
        fail("direct desktop guest agent supports only X11")
    try:
        raw = BUILD_PLAN.read_bytes()
    except FileNotFoundError:
        fail("desktop build plan identity is missing")
    if len(raw) > 1024 * 1024:
        fail("desktop build plan identity exceeds the reviewed limit")
    try:
        plan = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        fail(f"invalid desktop build plan identity: {error}")
    expected = {
        "profile_id": profile_id,
        "image_id": image_id,
        "generation": generation,
    }
    if not isinstance(plan, dict) or any(plan.get(key) != value for key, value in expected.items()):
        fail("desktop build plan identity does not match the reviewed scenario")


def reviewed_fixture(name: str, expected_digest: str) -> pathlib.Path:
    if not FIXTURE_RE.fullmatch(name) or not DIGEST_RE.fullmatch(expected_digest):
        fail("invalid desktop fixture policy")
    path = FIXTURE_ROOT / name
    try:
        info = path.lstat()
    except FileNotFoundError:
        fail("reviewed desktop fixture is missing")
    if not path.is_file() or path.is_symlink() or info.st_size < 1 or info.st_size > 1024 * 1024:
        fail("reviewed desktop fixture is not a bounded regular file")
    actual = "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()
    if actual != expected_digest:
        fail("reviewed desktop fixture digest mismatch")
    return path


def launch_application(application: str, fixture: pathlib.Path | None = None) -> None:
    if not APP_RE.fullmatch(application):
        fail("invalid desktop application ID")
    unit = "portage-e2e-" + hashlib.sha256(application.encode()).hexdigest()[:16]
    _, environment = desktop_identity()
    # gtk-launch exits after spawning the desktop process. The default
    # KillMode=control-group would then kill that child as the transient
    # unit becomes inactive, producing a false-success launch with no
    # window. Keep the reviewed child alive until close or VM stop.
    command = [
        "/usr/bin/systemd-run",
        "--user",
        "--property=KillMode=process",
        f"--unit={unit}",
    ]
    command.extend(f"--setenv={key}={value}" for key, value in environment.items())
    command.extend(["/usr/bin/gtk-launch", application.removesuffix(".desktop")])
    if fixture is not None:
        command.append(fixture.as_uri())
    run_desktop(command)


def install_binpkg(atom: str, digest: str, signer_fingerprint: str = "", raw_log_path: str = "") -> None:
    if not ATOM_RE.fullmatch(atom) or not DIGEST_RE.fullmatch(digest):
        fail("invalid package atom or staging digest")
    if not STAGING_DIGEST.is_file() or STAGING_DIGEST.read_text().strip() != digest:
        fail("hydrated staging snapshot does not match the scenario")
    signed = bool(signer_fingerprint or raw_log_path)
    if signed:
        if not FINGERPRINT_RE.fullmatch(signer_fingerprint) or not STAGING_SIGNER.is_file():
            fail("signed install lacks a reviewed signer policy")
        if STAGING_SIGNER.read_text().strip() != signer_fingerprint or not STAGING_GNUPG.is_dir():
            fail("hydrated staging signer does not match the scenario")
        ensure_evidence_root()
        log_path = evidence_path(raw_log_path, ".log")
    else:
        log_path = None
    environment = os.environ.copy()
    if signed:
        environment["FEATURES"] = "binpkg-request-signature"
        environment["BINPKG_GPG_VERIFY_GPG_HOME"] = str(STAGING_GNUPG)
    result = subprocess.run(
        [
            "/usr/bin/emerge",
            "--usepkgonly",
            "--binpkg-respect-use=y",
            "--oneshot",
            "--jobs=1",
            atom,
        ],
        check=False,
        text=True,
        capture_output=True,
        timeout=900,
        env=environment,
    )
    if log_path is not None:
        header = (
            f"atom={atom}\n"
            f"staging_digest={digest}\n"
            f"signature_required=true\n"
            f"signer_fingerprint={signer_fingerprint}\n"
            f"exit_code={result.returncode}\n"
        )
        atomic_write(log_path, (header + result.stdout + result.stderr).encode(), 0o640)
    if result.returncode != 0:
        requirement = "signature-required " if signed else ""
        fail(f"{requirement}binpkg install failed with exit code {result.returncode}")


def accessibility_budget(seconds: str | None) -> float:
    """Seconds an accessibility wait may take, from the caller's step budget.

    The driver passes the step's own remaining time. It used to be hardcoded at
    60 here, which silently capped every scenario: a step declaring
    timeout_seconds 90 got 60, failed with "selector was not found" at 60, and
    read as a desktop that never drew its window rather than as a budget the
    guest refused to honour. DEFAULT_ACCESSIBILITY_BUDGET stays for a driver
    that sends nothing, and the ceiling is the scenario schema's own per-step
    maximum so the guest cannot be told to wait longer than a step may last.
    """
    if seconds is None:
        return DEFAULT_ACCESSIBILITY_BUDGET
    if not BUDGET_RE.fullmatch(seconds):
        fail("invalid accessibility wait budget")
    budget = int(seconds)
    if budget < 1 or budget > MAX_ACCESSIBILITY_BUDGET:
        fail("accessibility wait budget is out of range")
    return float(budget)


def wait_accessible(role: str, name: str, state: str, seconds: str | None = None) -> None:
    if not role or not name or len(role) > 128 or len(name) > 256:
        fail("invalid accessibility selector")
    deadline = time.monotonic() + accessibility_budget(seconds)
    while time.monotonic() < deadline:
        if accessible_present(role, name, state):
            return
        time.sleep(1)
    fail("accessibility selector was not found")


def close_accessible(role: str, name: str, state: str, seconds: str | None = None) -> None:
    if not role or not name or len(role) > 128 or len(name) > 256:
        fail("invalid accessibility selector")
    if not accessible_present(role, name, state):
        fail("accessibility selector was not present before close")
    run_desktop(["/usr/bin/xdotool", "key", "--clearmodifiers", "Alt+F4"])
    deadline = time.monotonic() + accessibility_budget(seconds)
    while time.monotonic() < deadline:
        if not accessible_present(role, name, state):
            return
        time.sleep(1)
    fail("accessibility selector remained after normal window close")


def accessible_present(role: str, name: str, state: str) -> bool:
    result = run_desktop(
        [sys.executable, __file__, "_a11y", role, name, state],
        timeout=10,
        check=False,
    )
    if result.returncode != 0:
        fail("accessibility query failed in the reviewed desktop session")
    return result.stdout.strip() == "found"


def evidence_path(raw: str, suffix: str) -> pathlib.Path:
    path = pathlib.Path(raw)
    if path.parent != EVIDENCE_ROOT or not path.name.endswith(suffix) or not SAFE_PATH_RE.fullmatch(path.name):
        fail("evidence path is outside the reviewed runtime directory")
    return path


def existing_evidence_path(raw: str) -> pathlib.Path:
    path = pathlib.Path(raw)
    if path.parent != EVIDENCE_ROOT or path.suffix not in {".b64", ".log", ".json"} or not SAFE_PATH_RE.fullmatch(path.name):
        fail("evidence path is outside the reviewed runtime directory")
    try:
        info = path.lstat()
    except FileNotFoundError:
        fail("evidence file is missing")
    if not path.is_file() or path.is_symlink() or info.st_size > MAX_EVIDENCE_BYTES:
        fail("evidence file type or size is invalid")
    return path


def scan_accessibility(role: str, name: str, state: str) -> bool:
    import gi

    gi.require_version("Atspi", "2.0")
    from gi.repository import Atspi

    desktop = Atspi.get_desktop(0)
    stack = [desktop]
    visited = 0
    state_type = {
        "visible": Atspi.StateType.VISIBLE,
        "showing": Atspi.StateType.SHOWING,
        "enabled": Atspi.StateType.ENABLED,
        "focused": Atspi.StateType.FOCUSED,
    }.get(state)
    if state_type is None:
        fail("unsupported accessibility state")
    while stack and visited < 4096:
        node = stack.pop()
        visited += 1
        try:
            if node.get_role_name().lower() == role.lower() and node.get_name() == name and node.get_state_set().contains(state_type):
                return True
            count = min(node.get_child_count(), 256)
            for index in range(count - 1, -1, -1):
                child = node.get_child_at_index(index)
                if child is not None:
                    stack.append(child)
        except Exception:
            continue
    return False


def dump_accessibility(path: pathlib.Path) -> None:
    import gi

    gi.require_version("Atspi", "2.0")
    from gi.repository import Atspi

    state_types = {
        "visible": Atspi.StateType.VISIBLE,
        "showing": Atspi.StateType.SHOWING,
        "enabled": Atspi.StateType.ENABLED,
        "focused": Atspi.StateType.FOCUSED,
    }
    stack = [(Atspi.get_desktop(0), -1)]
    nodes: list[dict[str, object]] = []
    while stack and len(nodes) < 4096:
        node, parent = stack.pop()
        try:
            index = len(nodes)
            state_set = node.get_state_set()
            nodes.append(
                {
                    "index": index,
                    "parent": parent,
                    "role": node.get_role_name()[:128],
                    "name": node.get_name()[:512],
                    "states": [name for name, value in state_types.items() if state_set.contains(value)],
                }
            )
            count = min(node.get_child_count(), 256)
            for child_index in range(count - 1, -1, -1):
                child = node.get_child_at_index(child_index)
                if child is not None:
                    stack.append((child, index))
        except Exception:
            continue
    if not nodes:
        fail("accessibility tree is empty")
    atomic_write(
        path,
        json.dumps({"schema_version": 1, "nodes": nodes}, ensure_ascii=False, separators=(",", ":")).encode(),
        0o640,
    )


def main(arguments: list[str]) -> None:
    if not arguments:
        fail("desktop guest action is required")
    action, *values = arguments
    if action == "prepare" and len(values) in {2, 4}:
        hydrate_staging(*values)
    elif action == "assert-image" and len(values) == 4:
        assert_image(*values)
    elif action == "desktop-ready" and not values:
        desktop_ready()
    elif action == "install" and len(values) in {2, 4}:
        install_binpkg(*values)
    elif action == "launch" and len(values) == 1:
        launch_application(values[0])
    elif action == "launch-fixture" and len(values) == 3:
        application, fixture, digest = values
        launch_application(application, reviewed_fixture(fixture, digest))
    elif action == "wait-accessible" and len(values) in {3, 4}:
        wait_accessible(*values)
    elif action == "_a11y" and len(values) == 3:
        print("found" if scan_accessibility(*values) else "missing")
    elif action == "close" and len(values) in {3, 4}:
        close_accessible(*values)
    elif action == "key" and len(values) == 1:
        if not KEY_RE.fullmatch(values[0]):
            fail("invalid reviewed key sequence")
        run_desktop(["/usr/bin/xdotool", "key", "--clearmodifiers", values[0]])
    elif action == "type" and len(values) == 1:
        if not values[0] or len(values[0]) > 4096 or "\x00" in values[0]:
            fail("invalid typed text")
        run_desktop(["/usr/bin/xdotool", "type", "--clearmodifiers", "--delay", "20", "--", values[0]])
    elif action == "click" and len(values) == 2:
        try:
            x, y = int(values[0]), int(values[1])
        except ValueError:
            fail("click coordinates must be integers")
        if not (0 <= x < 1280 and 0 <= y < 720):
            fail("click coordinates are outside the reviewed display")
        run_desktop(["/usr/bin/xdotool", "mousemove", "--sync", str(x), str(y), "click", "1"])
    elif action == "screenshot" and len(values) == 1:
        ensure_evidence_root()
        encoded = evidence_path(values[0], ".png.b64")
        png = encoded.with_suffix("")
        run_desktop(["/usr/bin/scrot", "--overwrite", str(png)])
        data = png.read_bytes()
        if not data.startswith(b"\x89PNG\r\n\x1a\n"):
            fail("screenshot command did not produce PNG")
        atomic_write(encoded, base64.b64encode(data) + b"\n", 0o640)
        png.unlink()
    elif action == "dump-accessibility" and len(values) == 1:
        ensure_evidence_root()
        destination = evidence_path(values[0], ".a11y.json")
        run_desktop([sys.executable, __file__, "_dump_a11y", str(destination)], timeout=60)
    elif action == "_dump_a11y" and len(values) == 1:
        dump_accessibility(evidence_path(values[0], ".a11y.json"))
    elif action == "collect-logs" and len(values) == 3:
        scope, application, raw_path = values
        if scope not in {"application", "system", "desktop"}:
            fail("unsupported log scope")
        ensure_evidence_root()
        destination = evidence_path(raw_path, ".log")
        command = ["/usr/bin/journalctl", "--boot", "--no-pager", "--output=short-iso", "--lines=2000"]
        if scope == "application":
            if not APP_RE.fullmatch(application):
                fail("application log scope requires a reviewed application ID")
            unit = "portage-e2e-" + hashlib.sha256(application.encode()).hexdigest()[:16]
            result = run_desktop(command + ["--user", "--user-unit", unit], timeout=60, check=False)
        elif scope == "desktop":
            if application:
                fail("desktop log scope does not accept application ID")
            result = run_desktop(command + ["--user", "--user-unit", "graphical-session.target"], timeout=60, check=False)
        else:
            if application:
                fail("system log scope does not accept application ID")
            result = subprocess.run(command, check=False, text=True, capture_output=True, timeout=60)
        atomic_write(destination, (result.stdout + result.stderr).encode(), 0o640)
    elif action == "evidence-info" and len(values) == 1:
        source = existing_evidence_path(values[0])
        digest = hashlib.sha256()
        with source.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
        print(json.dumps({"size": source.stat().st_size, "sha256": "sha256:" + digest.hexdigest()}, separators=(",", ":")))
    elif action == "read-evidence" and len(values) == 3:
        source = existing_evidence_path(values[0])
        try:
            offset, limit = int(values[1]), int(values[2])
        except ValueError:
            fail("evidence offset and limit must be integers")
        size = source.stat().st_size
        if offset < 0 or offset > size or limit < 1 or limit > MAX_EVIDENCE_CHUNK or offset + limit > size:
            fail("evidence chunk is outside the reviewed file")
        with source.open("rb") as stream:
            stream.seek(offset)
            data = stream.read(limit)
        if len(data) != limit:
            fail("short evidence read")
        print(base64.b64encode(data).decode("ascii"))
    else:
        fail("unsupported or malformed desktop guest action")


if __name__ == "__main__":
    main(sys.argv[1:])
