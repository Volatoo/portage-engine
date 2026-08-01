#!/usr/bin/env python3
"""Real-host Public Beta identity and edge Gate.

All credentials are accepted only through the process environment and are
never included in output. Missing activation, real sessions, or boundary
inputs produce exit code 3 (not_run), never a passing result.
"""

from __future__ import annotations

import argparse
import base64
import datetime as dt
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any


BASELINE_COMMIT = "f79fca4"
PROVIDERS = {
    "authentik": "oidc",
    "google": "oidc",
    "github": "github",
}
CHECK_IDS = [
    "authentik_login_callback",
    "google_login_callback",
    "github_login_callback",
    "oauth_state_nonce_pkce",
    "session_max_lifetime",
    "session_idle_timeout",
    "session_single_revoke",
    "subject_revoke_all_step_up",
    "dashboard_logout",
    "authentik_backchannel_logout",
    "cross_provider_email_separation",
    "anonymous_surfaces",
    "livez_minimal",
    "readyz_minimal",
    "health_system_admin",
    "metrics_basic_auth",
    "https_cors",
    "webshell_not_routed",
    "backend_http_not_public",
    "edge_rate_limit",
]


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001
        return None


@dataclass
class Response:
    status: int
    headers: Any
    body: bytes

    def json(self) -> Any:
        return json.loads(self.body.decode("utf-8"))


class Gate:
    def __init__(self) -> None:
        self.checks: list[dict[str, Any]] = []
        self.recorded: set[str] = set()
        self.context = ssl.create_default_context()
        self.opener = urllib.request.build_opener(
            NoRedirect(), urllib.request.HTTPSHandler(context=self.context)
        )

    def record(self, check_id: str, status: str, summary: str, reason: str = "") -> None:
        if check_id in self.recorded:
            return
        item: dict[str, Any] = {
            "id": check_id,
            "status": status,
            "summary": summary,
        }
        if reason:
            item["reason"] = reason
        self.checks.append(item)
        self.recorded.add(check_id)

    def not_run(self, check_id: str, summary: str, missing: list[str]) -> None:
        self.record(
            check_id,
            "not_run",
            summary,
            "missing required runtime inputs: " + ", ".join(sorted(missing)),
        )

    def request(
        self,
        url: str,
        method: str = "GET",
        cookie: str = "",
        bearer: str = "",
        basic_user: str = "",
        basic_password: str = "",
        origin: str = "",
        body: bytes | None = None,
        extra_headers: dict[str, str] | None = None,
        timeout: float = 15.0,
    ) -> Response:
        headers = {
            "Accept": "application/json, text/html;q=0.9, */*;q=0.1",
            "User-Agent": "portage-public-beta-gate/1",
        }
        if cookie:
            headers["Cookie"] = "pe_session=" + cookie
        if bearer:
            headers["Authorization"] = "Bearer " + bearer
        if basic_user or basic_password:
            raw = (basic_user + ":" + basic_password).encode("utf-8")
            headers["Authorization"] = "Basic " + base64.b64encode(raw).decode("ascii")
        if origin:
            headers["Origin"] = origin
        if body is not None:
            headers["Content-Type"] = "application/json"
        if extra_headers:
            headers.update(extra_headers)
        request = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=timeout) as response:
                return Response(response.status, response.headers, response.read(1 << 20))
        except urllib.error.HTTPError as error:
            return Response(error.code, error.headers, error.read(1 << 20))


def env(name: str) -> str:
    return os.environ.get(name, "").strip()


def valid_https_base(raw: str) -> bool:
    try:
        parsed = urllib.parse.urlsplit(raw)
    except ValueError:
        return False
    return (
        parsed.scheme == "https"
        and bool(parsed.hostname)
        and not parsed.username
        and not parsed.password
        and parsed.query == ""
        and parsed.fragment == ""
        and parsed.path in ("", "/")
    )


def valid_backend_direct_url(raw: str) -> bool:
    try:
        parsed = urllib.parse.urlsplit(raw)
    except ValueError:
        return False
    return (
        parsed.scheme == "http"
        and bool(parsed.hostname)
        and not parsed.username
        and not parsed.password
        and parsed.query == ""
        and parsed.fragment == ""
    )


def join(base: str, path: str) -> str:
    return base.rstrip("/") + path


def iso_time(raw: str) -> dt.datetime:
    parsed = dt.datetime.fromisoformat(raw.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamp has no timezone")
    return parsed.astimezone(dt.timezone.utc)


def status_is_unauthorized(status: int) -> bool:
    return status in (401, 403)


def provider_flow_checks(gate: Gate, public_base: str) -> None:
    failures: list[str] = []
    for provider_id, provider_type in PROVIDERS.items():
        try:
            start = gate.request(
                join(public_base, f"/auth/provider/{provider_id}/start")
            )
            if start.status != 302:
                failures.append(provider_id + ":start_status")
                continue
            location = start.headers.get("Location", "")
            parsed = urllib.parse.urlsplit(location)
            query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
            set_cookie = start.headers.get("Set-Cookie", "").lower()
            required = (
                parsed.scheme == "https"
                and len(query.get("state", [""])[0]) >= 32
                and len(query.get("code_challenge", [""])[0]) >= 32
                and query.get("code_challenge_method", [""])[0] == "S256"
                and "pe_oidc_flow=" in set_cookie
                and "path=/auth/" in set_cookie
                and "secure" in set_cookie
                and "httponly" in set_cookie
                and "samesite=lax" in set_cookie
            )
            nonce = query.get("nonce", [""])[0]
            if provider_type == "oidc":
                required = required and len(nonce) >= 32
            else:
                required = required and nonce == ""
            invalid = gate.request(
                join(
                    public_base,
                    f"/auth/provider/{provider_id}/callback?state=invalid&code=invalid",
                )
            )
            required = required and invalid.status == 400
            if not required:
                failures.append(provider_id + ":protocol_contract")
        except (OSError, ValueError, urllib.error.URLError):
            failures.append(provider_id + ":request_error")
    if failures:
        gate.record(
            "oauth_state_nonce_pkce",
            "fail",
            "all provider starts use state, PKCE S256, scoped secure cookies, and OIDC nonce",
            "provider protocol checks failed: " + ", ".join(failures),
        )
    else:
        gate.record(
            "oauth_state_nonce_pkce",
            "pass",
            "all provider starts use state, PKCE S256, scoped secure cookies, and OIDC nonce",
        )


def provider_login_checks(
    gate: Gate, public_base: str
) -> dict[str, dict[str, Any]]:
    principals: dict[str, dict[str, Any]] = {}
    for provider_id in PROVIDERS:
        check_id = provider_id + "_login_callback"
        cookie_name = "PORTAGE_GATE_" + provider_id.upper() + "_COOKIE"
        cookie = env(cookie_name)
        if not cookie:
            gate.not_run(
                check_id,
                f"real {provider_id} Dashboard callback established a platform session",
                [cookie_name],
            )
            continue
        try:
            response = gate.request(join(public_base, "/api/iam/me"), cookie=cookie)
            payload = response.json() if response.status == 200 else {}
            principal = payload.get("principal", {})
            if (
                response.status == 200
                and principal.get("provider_id") == provider_id
                and principal.get("authentication") == "federated-session"
                and principal.get("session_id")
                and principal.get("subject_id")
                and principal.get("issuer")
                and principal.get("subject")
            ):
                principals[provider_id] = principal
                gate.record(
                    check_id,
                    "pass",
                    f"real {provider_id} Dashboard callback established a platform session",
                )
            else:
                gate.record(
                    check_id,
                    "fail",
                    f"real {provider_id} Dashboard callback established a platform session",
                    "session did not resolve to the expected provider identity",
                )
        except (ValueError, OSError, urllib.error.URLError, json.JSONDecodeError):
            gate.record(
                check_id,
                "fail",
                f"real {provider_id} Dashboard callback established a platform session",
                "callback session verification request failed",
            )
    return principals


def anonymous_and_probe_checks(gate: Gate, public_base: str, api_base: str) -> None:
    binpkg_path = env("PORTAGE_GATE_BINPKG_PACKAGES_PATH")
    if not binpkg_path:
        gate.not_run(
            "anonymous_surfaces",
            "packages, docs, status, and a real binhost Packages index are anonymous",
            ["PORTAGE_GATE_BINPKG_PACKAGES_PATH"],
        )
    elif not binpkg_path.startswith("/binpkgs/") or not binpkg_path.endswith("/Packages"):
        gate.record(
            "anonymous_surfaces",
            "fail",
            "packages, docs, status, and a real binhost Packages index are anonymous",
            "binpkg probe path is outside the reviewed anonymous namespace",
        )
    else:
        try:
            paths = ["/packages", "/docs", "/status", binpkg_path]
            statuses = [gate.request(join(public_base, path)).status for path in paths]
            gate.record(
                "anonymous_surfaces",
                "pass" if all(status == 200 for status in statuses) else "fail",
                "packages, docs, status, and a real binhost Packages index are anonymous",
                "one or more anonymous surfaces did not return 200"
                if any(status != 200 for status in statuses)
                else "",
            )
        except (OSError, urllib.error.URLError):
            gate.record(
                "anonymous_surfaces",
                "fail",
                "packages, docs, status, and a real binhost Packages index are anonymous",
                "anonymous surface request failed",
            )

    for check_id, path, expected in (
        ("livez_minimal", "/livez", {"status": "alive"}),
        ("readyz_minimal", "/readyz", {"status": "ready"}),
    ):
        try:
            response = gate.request(join(api_base, path))
            payload = response.json() if response.status == 200 else None
            passed = response.status == 200 and payload == expected
            gate.record(
                check_id,
                "pass" if passed else "fail",
                path + " is anonymous and returns only minimal status",
                "probe was unavailable or exposed additional fields" if not passed else "",
            )
        except (OSError, ValueError, urllib.error.URLError, json.JSONDecodeError):
            gate.record(
                check_id,
                "fail",
                path + " is anonymous and returns only minimal status",
                "probe request failed",
            )


def health_metrics_cors_checks(
    gate: Gate, public_base: str, api_base: str, metrics_base: str
) -> None:
    admin_token = env("PORTAGE_GATE_ADMIN_API_TOKEN")
    if not admin_token:
        gate.not_run(
            "health_system_admin",
            "/health rejects anonymous callers and accepts a system administrator",
            ["PORTAGE_GATE_ADMIN_API_TOKEN"],
        )
    else:
        try:
            anonymous = gate.request(join(api_base, "/health"))
            admin = gate.request(join(api_base, "/health"), bearer=admin_token)
            payload = admin.json() if admin.status == 200 else {}
            passed = (
                anonymous.status == 401
                and admin.status == 200
                and isinstance(payload.get("checks"), dict)
            )
            gate.record(
                "health_system_admin",
                "pass" if passed else "fail",
                "/health rejects anonymous callers and accepts a system administrator",
                "health authentication or healthy inventory contract failed" if not passed else "",
            )
        except (OSError, ValueError, urllib.error.URLError, json.JSONDecodeError):
            gate.record(
                "health_system_admin",
                "fail",
                "/health rejects anonymous callers and accepts a system administrator",
                "health boundary request failed",
            )

    metrics_user = env("PORTAGE_GATE_METRICS_USERNAME")
    metrics_password = env("PORTAGE_GATE_METRICS_PASSWORD")
    missing = []
    if not metrics_base:
        missing.append("PORTAGE_METRICS_BASE_URL")
    if not metrics_user:
        missing.append("PORTAGE_GATE_METRICS_USERNAME")
    if not metrics_password:
        missing.append("PORTAGE_GATE_METRICS_PASSWORD")
    if metrics_base and not valid_https_base(metrics_base):
        gate.record(
            "metrics_basic_auth",
            "fail",
            "the separate metrics hostname requires independent Basic Auth",
            "PORTAGE_METRICS_BASE_URL is not an HTTPS origin",
        )
    elif missing:
        gate.not_run(
            "metrics_basic_auth",
            "the separate metrics hostname requires independent Basic Auth",
            missing,
        )
    else:
        try:
            anonymous = gate.request(join(metrics_base, "/metrics/prometheus"))
            authenticated = gate.request(
                join(metrics_base, "/metrics/prometheus"),
                basic_user=metrics_user,
                basic_password=metrics_password,
            )
            passed = (
                anonymous.status == 401
                and "basic" in anonymous.headers.get("WWW-Authenticate", "").lower()
                and authenticated.status == 200
                and b"portage_builds_total" in authenticated.body
            )
            gate.record(
                "metrics_basic_auth",
                "pass" if passed else "fail",
                "the separate metrics hostname requires independent Basic Auth",
                "metrics authentication contract failed" if not passed else "",
            )
        except (OSError, urllib.error.URLError):
            gate.record(
                "metrics_basic_auth",
                "fail",
                "the separate metrics hostname requires independent Basic Auth",
                "metrics request failed",
            )

    try:
        allowed_origin = public_base.rstrip("/")
        allowed = gate.request(
            join(api_base, "/api/v1/public/status"), origin=allowed_origin
        )
        foreign = gate.request(
            join(api_base, "/api/v1/public/status"),
            origin="https://foreign.invalid",
        )
        passed = (
            allowed.headers.get("Access-Control-Allow-Origin", "") == allowed_origin
            and foreign.headers.get("Access-Control-Allow-Origin", "") == ""
        )
        gate.record(
            "https_cors",
            "pass" if passed else "fail",
            "HTTPS CORS allowlist echoes only the reviewed Dashboard origin",
            "allowed or foreign-origin CORS contract failed" if not passed else "",
        )
    except (OSError, urllib.error.URLError):
        gate.record(
            "https_cors",
            "fail",
            "HTTPS CORS allowlist echoes only the reviewed Dashboard origin",
            "CORS request failed",
        )


def shell_and_backend_checks(gate: Gate, public_base: str, api_base: str) -> None:
    try:
        headers = {
            "Connection": "Upgrade",
            "Upgrade": "websocket",
            "Sec-WebSocket-Key": base64.b64encode(os.urandom(16)).decode("ascii"),
            "Sec-WebSocket-Version": "13",
        }
        probes = [
            gate.request(join(public_base, "/shell/probe"), extra_headers=headers),
            gate.request(join(public_base, "/api/shell"), extra_headers=headers),
            gate.request(
                join(api_base, "/api/v1/instances/shell?id=probe"),
                origin="https://foreign.invalid",
                extra_headers=headers,
            ),
        ]
        passed = all(response.status == 404 for response in probes)
        gate.record(
            "webshell_not_routed",
            "pass" if passed else "fail",
            "Public Beta has no routed Dashboard or API WebShell endpoint",
            "a WebShell route was reachable at the edge" if not passed else "",
        )
    except (OSError, urllib.error.URLError):
        gate.record(
            "webshell_not_routed",
            "fail",
            "Public Beta has no routed Dashboard or API WebShell endpoint",
            "WebShell denial request failed",
        )

    direct_url = env("PORTAGE_GATE_BACKEND_DIRECT_URL")
    if not direct_url:
        gate.not_run(
            "backend_http_not_public",
            "an external verifier cannot reach the backend HTTP listener directly",
            ["PORTAGE_GATE_BACKEND_DIRECT_URL"],
        )
    elif not valid_backend_direct_url(direct_url):
        gate.record(
            "backend_http_not_public",
            "fail",
            "an external verifier cannot reach the backend HTTP listener directly",
            "PORTAGE_GATE_BACKEND_DIRECT_URL is not an HTTP URL without credentials",
        )
    else:
        try:
            response = gate.request(direct_url, timeout=5)
            gate.record(
                "backend_http_not_public",
                "fail",
                "an external verifier cannot reach the backend HTTP listener directly",
                f"backend returned HTTP {response.status}",
            )
        except (OSError, urllib.error.URLError):
            gate.record(
                "backend_http_not_public",
                "pass",
                "an external verifier cannot reach the backend HTTP listener directly",
            )


def cross_provider_check(
    gate: Gate, principals: dict[str, dict[str, Any]]
) -> None:
    shared_email = env("PORTAGE_GATE_SHARED_EMAIL")
    provider_names = [
        value.strip()
        for value in env("PORTAGE_GATE_SHARED_EMAIL_PROVIDERS").split(",")
        if value.strip()
    ] or ["authentik", "google"]
    missing: list[str] = []
    if not shared_email:
        missing.append("PORTAGE_GATE_SHARED_EMAIL")
    for name in provider_names:
        if name not in principals:
            missing.append("PORTAGE_GATE_" + name.upper() + "_COOKIE")
    if missing:
        gate.not_run(
            "cross_provider_email_separation",
            "same email across providers remains distinct issuer+subject identities",
            missing,
        )
        return
    selected = [principals[name] for name in provider_names]
    passed = (
        len(selected) >= 2
        and all(item.get("email", "").casefold() == shared_email.casefold() for item in selected)
        and len({item.get("subject_id") for item in selected}) == len(selected)
        and len({item.get("issuer") for item in selected}) == len(selected)
    )
    gate.record(
        "cross_provider_email_separation",
        "pass" if passed else "fail",
        "same email across providers remains distinct issuer+subject identities",
        "emails were not equal or durable subject identities were merged" if not passed else "",
    )


def current_principal(gate: Gate, public_base: str, cookie: str) -> dict[str, Any] | None:
    response = gate.request(join(public_base, "/api/iam/me"), cookie=cookie)
    if response.status != 200:
        return None
    payload = response.json()
    principal = payload.get("principal")
    return principal if isinstance(principal, dict) else None


def session_max_lifetime_check(gate: Gate, public_base: str) -> None:
    cookie = env("PORTAGE_GATE_MAX_LIFETIME_COOKIE")
    expected_raw = env("PORTAGE_GATE_MAX_LIFETIME_SECONDS")
    missing = []
    if not cookie:
        missing.append("PORTAGE_GATE_MAX_LIFETIME_COOKIE")
    if not expected_raw:
        missing.append("PORTAGE_GATE_MAX_LIFETIME_SECONDS")
    if missing:
        gate.not_run(
            "session_max_lifetime",
            "issued platform session is bounded by the configured maximum lifetime",
            missing,
        )
        return
    try:
        expected = int(expected_raw)
        principal = current_principal(gate, public_base, cookie)
        response = gate.request(join(public_base, "/api/iam/sessions"), cookie=cookie)
        payload = response.json() if response.status == 200 else {}
        current_id = payload.get("current_session_id")
        session = next(
            (item for item in payload.get("sessions", []) if item.get("id") == current_id),
            None,
        )
        lifetime = (
            iso_time(session["expires_at"]) - iso_time(session["issued_at"])
        ).total_seconds()
        passed = (
            principal is not None
            and expected > 0
            and lifetime > 0
            and lifetime <= expected + 2
        )
        gate.record(
            "session_max_lifetime",
            "pass" if passed else "fail",
            "issued platform session is bounded by the configured maximum lifetime",
            "observed session lifetime exceeded policy" if not passed else "",
        )
    except (TypeError, ValueError, KeyError, StopIteration, json.JSONDecodeError):
        gate.record(
            "session_max_lifetime",
            "fail",
            "issued platform session is bounded by the configured maximum lifetime",
            "session lifetime evidence was invalid",
        )


def single_session_revoke_check(gate: Gate, public_base: str) -> None:
    actor = env("PORTAGE_GATE_REVOKE_ACTOR_COOKIE")
    target = env("PORTAGE_GATE_REVOKE_TARGET_COOKIE")
    missing = []
    if not actor:
        missing.append("PORTAGE_GATE_REVOKE_ACTOR_COOKIE")
    if not target:
        missing.append("PORTAGE_GATE_REVOKE_TARGET_COOKIE")
    if missing:
        gate.not_run(
            "session_single_revoke",
            "one session can be revoked without revoking a sibling session",
            missing,
        )
        return
    try:
        actor_principal = current_principal(gate, public_base, actor)
        target_principal = current_principal(gate, public_base, target)
        same_subject = (
            actor_principal
            and target_principal
            and actor_principal.get("subject_id") == target_principal.get("subject_id")
            and actor_principal.get("session_id") != target_principal.get("session_id")
        )
        target_id = target_principal.get("session_id", "") if target_principal else ""
        revoked = gate.request(
            join(
                public_base,
                "/api/iam/sessions?session_id="
                + urllib.parse.quote(target_id, safe=""),
            ),
            method="DELETE",
            cookie=actor,
        )
        target_after = gate.request(join(public_base, "/api/iam/me"), cookie=target)
        actor_after = gate.request(join(public_base, "/api/iam/me"), cookie=actor)
        passed = bool(
            same_subject
            and revoked.status == 200
            and status_is_unauthorized(target_after.status)
            and actor_after.status == 200
        )
        gate.record(
            "session_single_revoke",
            "pass" if passed else "fail",
            "one session can be revoked without revoking a sibling session",
            "single-session revocation isolation failed" if not passed else "",
        )
    except (OSError, ValueError, urllib.error.URLError, json.JSONDecodeError):
        gate.record(
            "session_single_revoke",
            "fail",
            "one session can be revoked without revoking a sibling session",
            "single-session revocation request failed",
        )


def dashboard_logout_check(gate: Gate, public_base: str) -> None:
    cookie = env("PORTAGE_GATE_LOGOUT_COOKIE")
    if not cookie:
        gate.not_run(
            "dashboard_logout",
            "Dashboard logout clears the cookie and revokes its platform session",
            ["PORTAGE_GATE_LOGOUT_COOKIE"],
        )
        return
    try:
        before = current_principal(gate, public_base, cookie)
        logout = gate.request(join(public_base, "/logout"), cookie=cookie)
        after = gate.request(join(public_base, "/api/iam/me"), cookie=cookie)
        set_cookie = logout.headers.get("Set-Cookie", "").lower()
        passed = bool(
            before
            and logout.status == 302
            and "pe_session=" in set_cookie
            and ("max-age=0" in set_cookie or "max-age=-1" in set_cookie)
            and status_is_unauthorized(after.status)
        )
        gate.record(
            "dashboard_logout",
            "pass" if passed else "fail",
            "Dashboard logout clears the cookie and revokes its platform session",
            "logout cookie or backend revocation contract failed" if not passed else "",
        )
    except (OSError, ValueError, urllib.error.URLError, json.JSONDecodeError):
        gate.record(
            "dashboard_logout",
            "fail",
            "Dashboard logout clears the cookie and revokes its platform session",
            "logout request failed",
        )


def backchannel_logout_check(gate: Gate, public_base: str) -> None:
    cookie = env("PORTAGE_GATE_BACKCHANNEL_COOKIE")
    wait_raw = env("PORTAGE_GATE_BACKCHANNEL_WAIT_SECONDS")
    missing = []
    if not cookie:
        missing.append("PORTAGE_GATE_BACKCHANNEL_COOKIE")
    if not wait_raw:
        missing.append("PORTAGE_GATE_BACKCHANNEL_WAIT_SECONDS")
    if missing:
        gate.not_run(
            "authentik_backchannel_logout",
            "Authentik back-channel logout revokes an initially valid session",
            missing,
        )
        return
    try:
        wait_seconds = int(wait_raw)
        before = current_principal(gate, public_base, cookie)
        if not before or before.get("provider_id") != "authentik" or wait_seconds < 1:
            raise ValueError("invalid initial back-channel probe")
        print(
            "Waiting for the deployment operator/automation to trigger Authentik logout...",
            file=sys.stderr,
        )
        deadline = time.monotonic() + wait_seconds
        revoked = False
        while time.monotonic() < deadline:
            time.sleep(min(5, max(1, int(deadline - time.monotonic()))))
            response = gate.request(join(public_base, "/api/iam/me"), cookie=cookie)
            if status_is_unauthorized(response.status):
                revoked = True
                break
        gate.record(
            "authentik_backchannel_logout",
            "pass" if revoked else "fail",
            "Authentik back-channel logout revokes an initially valid session",
            "session remained valid through the logout observation window"
            if not revoked
            else "",
        )
    except (OSError, ValueError, urllib.error.URLError, json.JSONDecodeError):
        gate.record(
            "authentik_backchannel_logout",
            "fail",
            "Authentik back-channel logout revokes an initially valid session",
            "back-channel logout observation failed",
        )


def idle_timeout_check(gate: Gate, public_base: str) -> None:
    cookie = env("PORTAGE_GATE_IDLE_COOKIE")
    idle_raw = env("PORTAGE_GATE_IDLE_TIMEOUT_SECONDS")
    wait_raw = env("PORTAGE_GATE_IDLE_WAIT_SECONDS")
    missing = []
    if not cookie:
        missing.append("PORTAGE_GATE_IDLE_COOKIE")
    if not idle_raw:
        missing.append("PORTAGE_GATE_IDLE_TIMEOUT_SECONDS")
    if not wait_raw:
        missing.append("PORTAGE_GATE_IDLE_WAIT_SECONDS")
    if missing:
        gate.not_run(
            "session_idle_timeout",
            "an untouched dedicated session expires after the configured idle timeout",
            missing,
        )
        return
    try:
        idle_seconds = int(idle_raw)
        wait_seconds = int(wait_raw)
        before = current_principal(gate, public_base, cookie)
        if not before or idle_seconds < 1 or wait_seconds <= idle_seconds:
            raise ValueError("invalid idle probe window")
        print(f"Waiting {wait_seconds}s for the dedicated idle session...", file=sys.stderr)
        time.sleep(wait_seconds)
        after = gate.request(join(public_base, "/api/iam/me"), cookie=cookie)
        passed = status_is_unauthorized(after.status)
        gate.record(
            "session_idle_timeout",
            "pass" if passed else "fail",
            "an untouched dedicated session expires after the configured idle timeout",
            "dedicated session remained valid after the idle window" if not passed else "",
        )
    except (OSError, ValueError, urllib.error.URLError, json.JSONDecodeError):
        gate.record(
            "session_idle_timeout",
            "fail",
            "an untouched dedicated session expires after the configured idle timeout",
            "idle-timeout observation failed",
        )


def revoke_all_step_up_check(gate: Gate, public_base: str) -> None:
    stale = env("PORTAGE_GATE_STEP_UP_STALE_COOKIE")
    fresh = env("PORTAGE_GATE_STEP_UP_FRESH_COOKIE")
    probe = env("PORTAGE_GATE_REVOKE_ALL_PROBE_COOKIE")
    missing = []
    if not stale:
        missing.append("PORTAGE_GATE_STEP_UP_STALE_COOKIE")
    if not fresh:
        missing.append("PORTAGE_GATE_STEP_UP_FRESH_COOKIE")
    if not probe:
        missing.append("PORTAGE_GATE_REVOKE_ALL_PROBE_COOKIE")
    if missing:
        gate.not_run(
            "subject_revoke_all_step_up",
            "revoke-all rejects stale auth, accepts fresh step-up, and advances the subject boundary",
            missing,
        )
        return
    try:
        stale_principal = current_principal(gate, public_base, stale)
        fresh_principal = current_principal(gate, public_base, fresh)
        probe_principal = current_principal(gate, public_base, probe)
        same_subject = (
            stale_principal
            and fresh_principal
            and probe_principal
            and len(
                {
                    stale_principal.get("subject_id"),
                    fresh_principal.get("subject_id"),
                    probe_principal.get("subject_id"),
                }
            )
            == 1
        )
        body = json.dumps({"reason": "public_beta_gate"}).encode("utf-8")
        stale_result = gate.request(
            join(public_base, "/api/iam/sessions/revoke-all"),
            method="POST",
            cookie=stale,
            body=body,
        )
        fresh_result = gate.request(
            join(public_base, "/api/iam/sessions/revoke-all"),
            method="POST",
            cookie=fresh,
            body=body,
        )
        probe_after = gate.request(join(public_base, "/api/iam/me"), cookie=probe)
        passed = bool(
            same_subject
            and stale_result.status == 428
            and fresh_result.status == 200
            and status_is_unauthorized(probe_after.status)
        )
        gate.record(
            "subject_revoke_all_step_up",
            "pass" if passed else "fail",
            "revoke-all rejects stale auth, accepts fresh step-up, and advances the subject boundary",
            "fresh-auth step-up or subject-wide revocation contract failed" if not passed else "",
        )
    except (OSError, ValueError, urllib.error.URLError, json.JSONDecodeError):
        gate.record(
            "subject_revoke_all_step_up",
            "fail",
            "revoke-all rejects stale auth, accepts fresh step-up, and advances the subject boundary",
            "step-up/revoke-all request failed",
        )


def rate_limit_check(gate: Gate, public_base: str) -> None:
    enabled = env("PORTAGE_GATE_RATE_LIMIT_PROBE")
    if enabled != "1":
        gate.not_run(
            "edge_rate_limit",
            "the edge enforces a source-IP identity-request rate limit",
            ["PORTAGE_GATE_RATE_LIMIT_PROBE=1"],
        )
        return
    count_raw = env("PORTAGE_GATE_RATE_LIMIT_REQUESTS") or "12"
    try:
        count = int(count_raw)
        if count < 2 or count > 100:
            raise ValueError("invalid rate probe count")
        statuses = [
            gate.request(join(public_base, "/auth/provider/google/start")).status
            for _ in range(count)
        ]
        passed = 429 in statuses
        gate.record(
            "edge_rate_limit",
            "pass" if passed else "fail",
            "the edge enforces a source-IP identity-request rate limit",
            "rate probe did not observe HTTP 429" if not passed else "",
        )
    except (OSError, ValueError, urllib.error.URLError):
        gate.record(
            "edge_rate_limit",
            "fail",
            "the edge enforces a source-IP identity-request rate limit",
            "rate-limit request failed",
        )


def not_run_manifest(gate: Gate, reason: str) -> None:
    for check_id in CHECK_IDS:
        gate.record(
            check_id,
            "not_run",
            "real-host check requires explicit activation and deployment-owned inputs",
            reason,
        )


def write_manifest(path: str, manifest: dict[str, Any]) -> None:
    encoded = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode("utf-8")
    if not path:
        sys.stdout.buffer.write(encoded)
        return
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC
    descriptor = os.open(path, flags, 0o600)
    with os.fdopen(descriptor, "wb") as output:
        output.write(encoded)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default="")
    args = parser.parse_args()

    gate = Gate()
    run_live = env("PORTAGE_GATE_RUN_LIVE") == "1"
    public_base = env("PORTAGE_PUBLIC_BASE_URL")
    api_base = env("PORTAGE_API_BASE_URL")
    metrics_base = env("PORTAGE_METRICS_BASE_URL")

    if not run_live:
        not_run_manifest(
            gate,
            "PORTAGE_GATE_RUN_LIVE=1 was not set; no production credential was used",
        )
    elif not valid_https_base(public_base) or not valid_https_base(api_base):
        not_run_manifest(
            gate,
            "PORTAGE_PUBLIC_BASE_URL and PORTAGE_API_BASE_URL must be HTTPS origins",
        )
    else:
        provider_flow_checks(gate, public_base)
        principals = provider_login_checks(gate, public_base)
        anonymous_and_probe_checks(gate, public_base, api_base)
        health_metrics_cors_checks(gate, public_base, api_base, metrics_base)
        shell_and_backend_checks(gate, public_base, api_base)
        cross_provider_check(gate, principals)
        session_max_lifetime_check(gate, public_base)
        single_session_revoke_check(gate, public_base)
        dashboard_logout_check(gate, public_base)
        backchannel_logout_check(gate, public_base)
        revoke_all_step_up_check(gate, public_base)
        idle_timeout_check(gate, public_base)
        rate_limit_check(gate, public_base)

    for check_id in CHECK_IDS:
        if check_id not in gate.recorded:
            gate.record(
                check_id,
                "not_run",
                "real-host check was not reached",
                "required predecessor evidence was unavailable",
            )

    statuses = {item["status"] for item in gate.checks}
    if "fail" in statuses:
        overall = "fail"
        exit_code = 1
    elif "not_run" in statuses:
        overall = "not_run"
        exit_code = 3
    else:
        overall = "pass"
        exit_code = 0

    sensitive_inputs = [
        name
        for name in os.environ
        if name.startswith("PORTAGE_GATE_")
        and (
            name.endswith("_COOKIE")
            or name.endswith("_TOKEN")
            or name.endswith("_PASSWORD")
        )
        and bool(os.environ.get(name))
    ]
    manifest = {
        "schema_version": 1,
        "gate": "public_identity_real_host",
        "generated_at": dt.datetime.now(dt.timezone.utc)
        .replace(microsecond=0)
        .isoformat()
        .replace("+00:00", "Z"),
        "baseline_commit": BASELINE_COMMIT,
        "status": overall,
        "credentials_used": bool(run_live and sensitive_inputs),
        "secrets_persisted": False,
        "checks": gate.checks,
    }
    write_manifest(args.output, manifest)
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
