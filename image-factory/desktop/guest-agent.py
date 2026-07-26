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
BINREPO_CONFIG = pathlib.Path("/etc/portage/binrepos.conf/portage-engine-staging.conf")
EVIDENCE_ROOT = pathlib.Path("/run/portage-engine/desktop-evidence")
ATOM_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9+._-]*/[A-Za-z0-9][A-Za-z0-9+._-]*$")
APP_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9+._-]{0,127}\.desktop$")
DIGEST_RE = re.compile(r"^sha256:[a-f0-9]{64}$")
KEY_RE = re.compile(r"^[A-Za-z0-9+_-]{1,128}$")
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


def hydrate_staging(origin: str, expected_digest: str) -> None:
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
    atomic_write(
        BINREPO_CONFIG,
        b"[portage-engine-staging]\nsync-uri = file:///var/lib/portage-engine/staging-binhost\npriority = 1000\n",
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
    if action == "prepare" and len(values) == 2:
        hydrate_staging(values[0], values[1])
    elif action == "desktop-ready" and not values:
        desktop_ready()
    elif action == "install" and len(values) == 2:
        atom, digest = values
        if not ATOM_RE.fullmatch(atom) or not DIGEST_RE.fullmatch(digest):
            fail("invalid package atom or staging digest")
        if not STAGING_DIGEST.is_file() or STAGING_DIGEST.read_text().strip() != digest:
            fail("hydrated staging snapshot does not match the scenario")
        subprocess.run(
            ["/usr/bin/emerge", "--usepkgonly", "--binpkg-respect-use=y", "--oneshot", "--jobs=1", atom],
            check=True,
            timeout=900,
        )
    elif action == "launch" and len(values) == 1:
        application = values[0]
        if not APP_RE.fullmatch(application):
            fail("invalid desktop application ID")
        unit = "portage-e2e-" + hashlib.sha256(application.encode()).hexdigest()[:16]
        _, environment = desktop_identity()
        # gtk-launch exits after spawning the desktop process. The default
        # KillMode=control-group would then kill that child as the transient
        # unit becomes inactive, producing a false-success launch with no
        # window. Keep the reviewed child alive until the VM is stopped.
        command = [
            "/usr/bin/systemd-run",
            "--user",
            "--property=KillMode=process",
            f"--unit={unit}",
        ]
        command.extend(f"--setenv={key}={value}" for key, value in environment.items())
        command.extend(["/usr/bin/gtk-launch", application.removesuffix(".desktop")])
        run_desktop(command)
    elif action == "wait-accessible" and len(values) == 3:
        role, name, state = values
        if not role or not name or len(role) > 128 or len(name) > 256:
            fail("invalid accessibility selector")
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            result = run_desktop([sys.executable, __file__, "_a11y", role, name, state], timeout=10)
            if result.stdout.strip() == "found":
                return
            time.sleep(1)
        fail("accessibility selector was not found")
    elif action == "_a11y" and len(values) == 3:
        print("found" if scan_accessibility(*values) else "missing")
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
