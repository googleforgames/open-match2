#!/usr/bin/env python3
# Copyright 2024 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Deployment manifest and Dockerfile validator for Open Match 2 (om-core).

Audits Cloud Run Knative Service manifests, Cloud Build pipelines, OpenTelemetry
Collector sidecar Dockerfiles, and GKE deployment manifests for:
  1. Valid Apache 2.0 license header URLs.
  2. Unreplaced template placeholders ($SERVICE_ACCOUNT, $PRIMARY_ENDPOINT,
     $READ_ENDPOINT, $GCP_SA_FOR_METRICS) in --strict-templates mode vs.
     default template mode.
  3. Hardcoded personal/sample development GCP project IDs or GKE cluster names.
  4. Single-line and two-line Kubernetes / Cloud Build OM_* integer environment
     variables that must be strictly positive (> 0).
  5. OpenTelemetry Collector sidecar Dockerfile config paths
     (/etc/otelcol-google/config.yaml and /etc/otelcol-contrib/config.yaml) and
     bare ENTRYPOINT overrides.
  6. Cloud Run Knative annotations, container limits, OM_LOGGING_LEVEL, request
     timeouts, and gcloud run deploy flags (--no-cpu-throttling, gen2,
     --min-instances, --no-allow-unauthenticated, --ingress, --timeout).
  7. GKE Deployment replicas, terminationGracePeriodSeconds, and om-core native
     lifecycle.preStop.sleep.seconds draining settings.

Exit codes:
  0 - All audited files passed validation (warnings may be present in non-strict mode).
  1 - One or more validation errors were detected.
  2 - One or more target files were missing or unreadable.
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass
from pathlib import Path
import re
import shlex
import sys
from typing import Dict, List, Optional, Sequence, Set, Tuple

DEFAULT_RELATIVE_PATHS: Tuple[str, ...] = (
    "ci.yaml",
    "deploy/cloud_run/service.yaml",
    "deploy/cloud_run/cloudbuild.yaml",
    "deploy/cloud_run/otel-collector-sidecar/Dockerfile",
    "deploy/cloud_run/otel-collector-sidecar/config.yaml",
    "deploy/gke/om.yaml",
)

TEMPLATE_PLACEHOLDERS: Tuple[str, ...] = (
    "$SERVICE_ACCOUNT",
    "$PRIMARY_ENDPOINT",
    "$READ_ENDPOINT",
    "$GCP_SA_FOR_METRICS",
)

# Hardcoded personal or sample development GCP project IDs and GKE cluster names.
HARDCODED_SAMPLE_IDENTIFIERS: Dict[str, str] = {
    "peteryizhong-gke-dev": "hardcoded sample GCP project ID 'peteryizhong-gke-dev'",
    "yi-standard-cluster": "hardcoded sample GKE cluster name 'yi-standard-cluster'",
    "your-project-id": "placeholder GCP project ID 'your-project-id'",
    "my-gcp-project": "placeholder GCP project ID 'my-gcp-project'",
    "example-project-id": "placeholder GCP project ID 'example-project-id'",
    "your-cluster-name": "placeholder GKE cluster name 'your-cluster-name'",
}

# Required-positive (> 0) integer OM_* configuration variables (union of Task 2.1
# guarded positive integers and outgoing batch/wait integer parameters).
REQUIRED_POSITIVE_INT_VARS: Set[str] = {
    "OM_CACHE_IN_MAX_UPDATES_PER_POLL",
    "OM_CACHE_IN_MAX_APPLY_DURATION_MS",
    "OM_CACHE_OUT_MAX_QUEUE_THRESHOLD",
    "OM_CACHE_OUT_WAIT_TIMEOUT_MS",
    "OM_CACHE_IN_QUEUE_BUFFER_SIZE",
    "OM_CACHE_OUT_QUEUE_BUFFER_SIZE",
    "OM_CACHE_OUT_MAX_PACKED_KEYS",
    "OM_CACHE_EXPIRATION_INTERVAL_MS",
    "OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE",
    "OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS",
    "OM_MAX_STATE_UPDATES_PER_CALL",
    "OM_REDIS_POOL_MAX_IDLE",
    "OM_REDIS_POOL_MAX_ACTIVE",
    "OM_CACHE_TICKET_TTL_MS",
    "OM_MMF_TIMEOUT_SECS",
    "OM_PROM_PORT",
    "OM_GRPC_PORT",
    "PORT",
    "REDISPORT",
}

VALID_INGRESS_VALUES: Set[str] = {
    "internal",
    "internal-and-cloud-load-balancing",
    "all",
}

# Derived from the default timeouts set in internal/config/config.go
# (OM_MMF_TIMEOUT_SECS = 600 + OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS = 60000 / 1000 = 660).
# Update this constant if those defaults in internal/config/config.go change.
MIN_CLOUD_RUN_TIMEOUT_SECONDS: int = 660
MAX_CLOUD_RUN_TIMEOUT_SECONDS: int = 3600

CLOUD_RUN_TIMEOUT_RULE: str = (
    "The Cloud Run request timeout must be at least `OM_MMF_TIMEOUT_SECS` + "
    "`OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` ÷ 1000. Both are defined in "
    "`internal/config/config.go` and overridable by environment variables. With the "
    "default timeouts set in `internal/config/config.go`, that is 660 s. The Cloud Run "
    "maximum is 3600 s."
)

# Default graceful shutdown budget: 5 s server drain in start (server.go) +
# 3 s replication stop in main.go = 8 s.
DEFAULT_SHUTDOWN_BUDGET_SECONDS: int = 8


@dataclass(frozen=True)
class Finding:
    severity: str  # "ERROR" or "WARN"
    path: str
    line: int
    message: str

    def format(self) -> str:
        loc = f"{self.path}:{self.line}" if self.line > 0 else self.path
        return f"[{self.severity}] {loc}: {self.message}"


def _repo_root() -> Path:
    return Path(__file__).resolve().parents[4]


def _is_tracked_gke_template(file_path: Path) -> bool:
    try:
        tracked = (_repo_root() / "deploy" / "gke" / "om.yaml").resolve()
        return file_path.resolve() == tracked
    except OSError:
        return False


def _unquote(val: str) -> str:
    val = val.strip()
    if len(val) >= 2 and ((val[0] == "'" and val[-1] == "'") or (val[0] == '"' and val[-1] == '"')):
        return val[1:-1].strip()
    return val


def _strip_inline_comment(line: str) -> str:
    in_single = False
    in_double = False
    for idx, ch in enumerate(line):
        if ch == "'" and not in_double:
            in_single = not in_single
        elif ch == '"' and not in_single:
            in_double = not in_double
        elif ch == "#" and not in_single and not in_double:
            if idx == 0 or line[idx - 1].isspace():
                return line[:idx].rstrip()
    return line.rstrip()


def _check_common_text_rules(
    file_path: Path,
    display_path: str,
    lines: Sequence[str],
    strict_templates: bool,
    findings: List[Finding],
) -> None:
    is_gke_repo_template = _is_tracked_gke_template(file_path)

    for line_no, raw_line in enumerate(lines, start=1):
        if "https:#www.apache.org/licenses/LICENSE-2.0" in raw_line:
            findings.append(
                Finding(
                    "ERROR",
                    display_path,
                    line_no,
                    "Malformed Apache 2.0 license URL 'https:#www.apache.org/licenses/LICENSE-2.0' "
                    "(expected 'https://www.apache.org/licenses/LICENSE-2.0').",
                )
            )

        stripped = raw_line.strip()
        if stripped.startswith("#"):
            continue

        code_part = _strip_inline_comment(raw_line)

        for placeholder in TEMPLATE_PLACEHOLDERS:
            if placeholder in code_part:
                severity = "ERROR" if strict_templates else "WARN"
                findings.append(
                    Finding(
                        severity,
                        display_path,
                        line_no,
                        f"Unreplaced template placeholder '{placeholder}' "
                        f"({'treated as error under --strict-templates' if strict_templates else 'replace before production deploy, or pass --strict-templates to fail'}).",
                    )
                )

        for sample_id, description in HARDCODED_SAMPLE_IDENTIFIERS.items():
            if re.search(rf"(?<![A-Za-z0-9_-]){re.escape(sample_id)}(?![A-Za-z0-9_-])", code_part):
                # On the tracked deploy/gke/om.yaml repository template in non-strict mode,
                # report sample template values as warnings so default repo validation exits 0.
                # In --strict-templates mode or on any other manifest file, report as an ERROR.
                severity = (
                    "WARN"
                    if (is_gke_repo_template and not strict_templates)
                    else "ERROR"
                )
                findings.append(
                    Finding(
                        severity,
                        display_path,
                        line_no,
                        f"Found {description}; replace with target environment value.",
                    )
                )


def _extract_substitutions(lines: Sequence[str]) -> Dict[str, Tuple[int, str]]:
    """Extracts Cloud Build substitutions mapping (_VAR -> (line_no, value))."""
    subs: Dict[str, Tuple[int, str]] = {}
    in_subs = False
    subs_indent = 0

    for line_no, raw_line in enumerate(lines, start=1):
        code = _strip_inline_comment(raw_line)
        if not code.strip():
            continue
        indent = len(code) - len(code.lstrip(" "))
        stripped = code.strip()

        if re.match(r"^substitutions\s*:\s*$", stripped):
            in_subs = True
            subs_indent = indent
            continue

        if in_subs:
            if indent <= subs_indent and not stripped.startswith("-"):
                in_subs = False
                continue
            m = re.match(r"^([A-Za-z0-9_]+)\s*:\s*(.*)$", stripped)
            if m:
                subs[m.group(1)] = (line_no, _unquote(m.group(2)))
    return subs


def _validate_positive_om_int(
    var_name: str,
    raw_value: str,
    line_no: int,
    display_path: str,
    substitutions: Dict[str, Tuple[int, str]],
    findings: List[Finding],
) -> None:
    canonical_name = var_name[1:] if var_name.startswith("_OM_") else var_name
    if canonical_name not in REQUIRED_POSITIVE_INT_VARS:
        return

    val = _unquote(raw_value)

    # Resolve Cloud Build ${_VAR} substitution if present.
    sub_match = re.match(r"^\$\{([A-Za-z0-9_]+)\}$", val)
    if sub_match:
        sub_key = sub_match.group(1)
        if sub_key in substitutions:
            line_no, val = substitutions[sub_key]
            val = _unquote(val)
        else:
            return

    # Ignore unreplaced template placeholders (handled by placeholder checks).
    if val.startswith("$"):
        return

    try:
        parsed = int(val, 10)
    except ValueError:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                line_no,
                f"Environment variable '{canonical_name}' must be a positive integer (> 0), got non-integer '{val}'.",
            )
        )
        return

    if parsed <= 0:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                line_no,
                f"Environment variable '{canonical_name}' must be > 0, got {parsed}.",
            )
        )


def _audit_env_vars(
    display_path: str,
    lines: Sequence[str],
    substitutions: Dict[str, Tuple[int, str]],
    findings: List[Finding],
) -> None:
    """Audits single-line, flow-map, two-line Kubernetes env:, and Cloud Build env vars."""
    pending_env_name: Optional[str] = None
    pending_env_line: int = 0
    pending_env_value: Optional[str] = None
    pending_env_value_line: int = 0

    for line_no, raw_line in enumerate(lines, start=1):
        code = _strip_inline_comment(raw_line)
        stripped = code.strip()
        if not stripped:
            continue

        # 1. Inline flow mapping: - { name: OM_..., value: "..." }
        flow_name = re.search(r"\bname\s*:\s*(['\"]?[A-Za-z0-9_]+['\"]?)", stripped)
        flow_val = re.search(r"\bvalue\s*:\s*([^,}]+)", stripped)
        if flow_name and flow_val:
            name_str = _unquote(flow_name.group(1))
            val_str = _unquote(flow_val.group(1))
            _validate_positive_om_int(name_str, val_str, line_no, display_path, substitutions, findings)
            pending_env_name = None
            pending_env_value = None
            continue

        # 2. Two-line Kubernetes env list entry:
        #    - name: OM_...
        #      value: "..."
        name_match = re.match(r"^(?:-\s*)?name\s*:\s*(.+)$", stripped)
        if name_match:
            extracted_name = _unquote(name_match.group(1))
            if pending_env_value is not None and stripped.startswith("name:"):
                _validate_positive_om_int(
                    extracted_name,
                    pending_env_value,
                    pending_env_value_line,
                    display_path,
                    substitutions,
                    findings,
                )
                pending_env_value = None
                pending_env_name = None
            else:
                pending_env_name = extracted_name
                pending_env_line = line_no
                pending_env_value = None
            continue

        val_match = re.match(r"^(?:-\s*)?value\s*:\s*(.*)$", stripped)
        if val_match:
            extracted_val = _unquote(val_match.group(1))
            if pending_env_name is not None:
                _validate_positive_om_int(
                    pending_env_name,
                    extracted_val,
                    line_no,
                    display_path,
                    substitutions,
                    findings,
                )
                pending_env_name = None
            elif stripped.startswith("-"):
                pending_env_value = extracted_val
                pending_env_value_line = line_no
            continue

        # If another list item or unrelated key starts, clear pending two-line state.
        if stripped.startswith("- ") or ":" in stripped:
            pending_env_name = None
            pending_env_value = None

        # 3. Single-line direct key-value pair: OM_...: <val> or _OM_...: <val>
        kv_match = re.match(r"^(?:-\s*)?(['\"]?_?(?:OM_[A-Za-z0-9_]+|PORT|REDISPORT)['\"]?)\s*:\s*(.+)$", stripped)
        if kv_match:
            k_name = _unquote(kv_match.group(1))
            v_str = _unquote(kv_match.group(2))
            _validate_positive_om_int(k_name, v_str, line_no, display_path, substitutions, findings)
            continue

        # 4. Key=Value assignments inside args (e.g. "OM_CACHE_IN_MAX_UPDATES_PER_POLL=0")
        for assign_match in re.finditer(
            r"\b(OM_[A-Za-z0-9_]+|PORT|REDISPORT)=([^\s,'\"]+)",
            stripped,
        ):
            _validate_positive_om_int(
                assign_match.group(1),
                assign_match.group(2),
                line_no,
                display_path,
                substitutions,
                findings,
            )


def _audit_otel_dockerfile(
    display_path: str,
    lines: Sequence[str],
    findings: List[Finding],
) -> None:
    """Audits an OpenTelemetry Collector sidecar Dockerfile."""
    logical_lines: List[Tuple[int, str]] = []
    current_parts: List[str] = []
    start_line = 1

    for line_no, raw_line in enumerate(lines, start=1):
        stripped = raw_line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        code = _strip_inline_comment(raw_line).strip()
        if not code:
            continue
        if not current_parts:
            start_line = line_no
        if code.endswith("\\"):
            current_parts.append(code[:-1].strip())
        else:
            current_parts.append(code)
            logical_lines.append((start_line, " ".join(current_parts)))
            current_parts = []
    if current_parts:
        logical_lines.append((start_line, " ".join(current_parts)))

    from_images: List[Tuple[int, str]] = []
    copied_dests: Set[str] = set()
    entrypoints: List[Tuple[int, str]] = []
    cmds: List[Tuple[int, str]] = []

    for line_no, instruction in logical_lines:
        upper = instruction.upper()
        if upper.startswith("FROM "):
            from_images.append((line_no, instruction[5:].strip()))
        elif upper.startswith("ADD ") or upper.startswith("COPY "):
            parts = instruction.split()
            # Filter out --chown / --chmod flags
            args = [p.strip("'\"") for p in parts[1:] if not p.startswith("--")]
            if len(args) >= 2:
                src_tokens = args[:-1]
                dest = args[-1]
                if any(s.endswith("config.yaml") for s in src_tokens):
                    copied_dests.add(dest)
        elif upper.startswith("ENTRYPOINT "):
            entrypoints.append((line_no, instruction[11:].strip()))
        elif upper.startswith("CMD "):
            cmds.append((line_no, instruction[4:].strip()))

    if not from_images:
        findings.append(
            Finding("ERROR", display_path, 1, "Dockerfile is missing a FROM instruction.")
        )
        return

    uses_google_otel = any("otelcol-google" in img for _, img in from_images)
    uses_contrib_otel = any("opentelemetry-collector-contrib" in img or "otelcol-contrib" in img for _, img in from_images)

    has_google_cfg = "/etc/otelcol-google/config.yaml" in copied_dests
    has_contrib_cfg = "/etc/otelcol-contrib/config.yaml" in copied_dests

    if uses_google_otel and not has_google_cfg:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                from_images[0][0],
                "Dockerfile uses 'otelcol-google' base image but does not copy config.yaml to "
                "'/etc/otelcol-google/config.yaml'.",
            )
        )

    if uses_contrib_otel and not has_contrib_cfg:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                from_images[0][0],
                "Dockerfile uses 'opentelemetry-collector-contrib' base image but does not copy "
                "config.yaml to '/etc/otelcol-contrib/config.yaml'.",
            )
        )

    if not has_google_cfg and not has_contrib_cfg:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                from_images[0][0],
                "OTel Collector Dockerfile must copy config.yaml to '/etc/otelcol-google/config.yaml' "
                "and/or '/etc/otelcol-contrib/config.yaml'.",
            )
        )
    elif not (has_google_cfg and has_contrib_cfg):
        findings.append(
            Finding(
                "WARN",
                display_path,
                from_images[0][0],
                "Recommend copying config.yaml to both '/etc/otelcol-google/config.yaml' and "
                "'/etc/otelcol-contrib/config.yaml' for compatibility across collector distributions.",
            )
        )

    for ep_line, ep_val in entrypoints:
        has_config_in_ep = "--config" in ep_val
        has_config_in_cmd = any("--config" in cmd_val for _, cmd_val in cmds)
        if not has_config_in_ep and not has_config_in_cmd:
            findings.append(
                Finding(
                    "ERROR",
                    display_path,
                    ep_line,
                    "Bare ENTRYPOINT override omits '--config' and no CMD provides '--config'; "
                    "collector may fail to load its configuration file.",
                )
            )


@dataclass
class _ContainerSpec:
    start_line: int
    name: Optional[str] = None
    image: Optional[str] = None
    limits_cpu: Optional[str] = None
    limits_memory: Optional[str] = None
    has_prestop: bool = False
    prestop_sleep_seconds: Optional[Tuple[int, str]] = None


def _parse_k8s_containers(lines: Sequence[str], start_line_offset: int = 0) -> List[_ContainerSpec]:
    """Parses container blocks under `containers:` in a Knative or Kubernetes manifest."""
    containers: List[_ContainerSpec] = []
    in_containers = False
    containers_indent = 0
    container_item_indent: Optional[int] = None
    container_key_indent: Optional[int] = None
    current: Optional[_ContainerSpec] = None

    current_top_section: Optional[str] = None
    in_limits = False
    limits_indent = 0
    in_prestop = False
    prestop_indent = 0
    in_sleep = False
    sleep_indent = 0

    def _apply_container_kv(spec: _ContainerSpec, key: str, val_part: str) -> None:
        if key == "name" and spec.name is None:
            spec.name = _unquote(val_part)
        elif key == "image" and spec.image is None:
            spec.image = _unquote(val_part)
        elif key == "resources":
            m_flow_lim = re.search(r"\blimits\s*:\s*\{([^}]*)\}", val_part)
            if m_flow_lim:
                inner = m_flow_lim.group(1)
                m_c = re.search(r"\bcpu\s*:\s*([^,\s}]+)", inner)
                m_m = re.search(r"\bmemory\s*:\s*([^,\s}]+)", inner)
                if m_c:
                    spec.limits_cpu = _unquote(m_c.group(1))
                if m_m:
                    spec.limits_memory = _unquote(m_m.group(1))

    for idx, raw_line in enumerate(lines):
        line_no = start_line_offset + idx + 1
        raw_code = _strip_inline_comment(raw_line)
        stripped = raw_code.strip()
        if not stripped:
            continue
        indent = len(raw_code) - len(raw_code.lstrip(" "))

        if not in_containers:
            if re.match(r"^containers\s*:\s*$", stripped):
                in_containers = True
                containers_indent = indent
                container_item_indent = None
                container_key_indent = None
                current = None
            continue

        if indent < containers_indent or (indent == containers_indent and not stripped.startswith("-")):
            if current is not None:
                containers.append(current)
                current = None
            in_containers = False
            if re.match(r"^containers\s*:\s*$", stripped):
                in_containers = True
                containers_indent = indent
                container_item_indent = None
                container_key_indent = None
            continue

        if stripped.startswith("-") and (container_item_indent is None or indent == container_item_indent):
            if container_item_indent is None:
                container_item_indent = indent
            if current is not None:
                containers.append(current)
            current = _ContainerSpec(start_line=line_no)
            after_dash = stripped[1:]
            lead_spaces = len(after_dash) - len(after_dash.lstrip(" "))
            container_key_indent = indent + 1 + lead_spaces
            first_item = after_dash.strip()
            current_top_section = None
            in_limits = False
            in_prestop = False
            in_sleep = False
            if ":" in first_item:
                k, v = first_item.split(":", 1)
                k = k.strip()
                current_top_section = k
                _apply_container_kv(current, k, v.strip())
            continue

        if current is None or container_key_indent is None:
            continue

        if indent == container_key_indent and not stripped.startswith("-"):
            in_limits = False
            in_prestop = False
            in_sleep = False
            if ":" in stripped:
                k, v = stripped.split(":", 1)
                k = k.strip()
                current_top_section = k
                _apply_container_kv(current, k, v.strip())
            else:
                current_top_section = None
            continue

        if current_top_section == "resources":
            if in_limits and indent <= limits_indent:
                in_limits = False
            if not in_limits:
                m_lim = re.match(r"^limits\s*:\s*(.*)$", stripped)
                if m_lim:
                    rest = m_lim.group(1).strip()
                    if rest.startswith("{") and rest.endswith("}"):
                        inner = rest[1:-1]
                        m_c = re.search(r"\bcpu\s*:\s*([^,\s}]+)", inner)
                        m_m = re.search(r"\bmemory\s*:\s*([^,\s}]+)", inner)
                        if m_c:
                            current.limits_cpu = _unquote(m_c.group(1))
                        if m_m:
                            current.limits_memory = _unquote(m_m.group(1))
                    else:
                        in_limits = True
                        limits_indent = indent
            else:
                m_c = re.match(r"^cpu\s*:\s*(.+)$", stripped)
                if m_c:
                    val_c = _unquote(m_c.group(1))
                    if val_c:
                        current.limits_cpu = val_c
                m_m = re.match(r"^memory\s*:\s*(.+)$", stripped)
                if m_m:
                    val_m = _unquote(m_m.group(1))
                    if val_m:
                        current.limits_memory = val_m

        elif current_top_section == "lifecycle":
            if in_sleep and indent <= sleep_indent:
                in_sleep = False
            if in_prestop and indent <= prestop_indent:
                in_prestop = False
                in_sleep = False

            if not in_prestop:
                m_ps = re.match(r"^preStop\s*:\s*(.*)$", stripped)
                if m_ps:
                    current.has_prestop = True
                    rest = m_ps.group(1).strip()
                    m_flow_sec = re.search(r"\bsleep\s*:\s*\{\s*seconds\s*:\s*([^,\s}]+)", rest)
                    if m_flow_sec:
                        current.prestop_sleep_seconds = (line_no, _unquote(m_flow_sec.group(1)))
                    else:
                        in_prestop = True
                        prestop_indent = indent
            elif not in_sleep:
                m_sl = re.match(r"^sleep\s*:\s*(.*)$", stripped)
                if m_sl:
                    rest = m_sl.group(1).strip()
                    m_flow_sec = re.search(r"\bseconds\s*:\s*([^,\s}]+)", rest)
                    if m_flow_sec:
                        current.prestop_sleep_seconds = (line_no, _unquote(m_flow_sec.group(1)))
                    else:
                        in_sleep = True
                        sleep_indent = indent
            else:
                m_sec = re.match(r"^seconds\s*:\s*(.*)$", stripped)
                if m_sec:
                    current.prestop_sleep_seconds = (line_no, _unquote(m_sec.group(1)))

    if current is not None:
        containers.append(current)

    return containers


def _find_om_core_container(containers: Sequence[_ContainerSpec]) -> Optional[_ContainerSpec]:
    for c in containers:
        if c.name in ("core", "om-core"):
            return c
    for c in containers:
        if c.image and "om-core" in c.image:
            return c
    return None


def _audit_knative_service_yaml(
    display_path: str,
    lines: Sequence[str],
    findings: List[Finding],
) -> None:
    """Audits a Cloud Run Knative Service manifest (e.g. deploy/cloud_run/service.yaml)."""
    cpu_throttling: Optional[Tuple[int, str]] = None
    exec_env: Optional[Tuple[int, str]] = None
    min_scale: Optional[Tuple[int, str]] = None
    ingress: Optional[Tuple[int, str]] = None
    timeout_seconds: Optional[Tuple[int, str]] = None
    logging_levels: List[Tuple[int, str]] = []
    pending_env_name: Optional[str] = None

    for line_no, raw_line in enumerate(lines, start=1):
        code = _strip_inline_comment(raw_line).strip()
        if not code:
            continue

        m_cpu = re.match(r"^['\"]?run\.googleapis\.com/cpu-throttling['\"]?\s*:\s*(.*)$", code)
        if m_cpu:
            cpu_throttling = (line_no, _unquote(m_cpu.group(1)))
            continue

        m_env = re.match(r"^['\"]?run\.googleapis\.com/execution-environment['\"]?\s*:\s*(.*)$", code)
        if m_env:
            exec_env = (line_no, _unquote(m_env.group(1)))
            continue

        m_min = re.match(r"^['\"]?autoscaling\.knative\.dev/minScale['\"]?\s*:\s*(.*)$", code)
        if m_min:
            min_scale = (line_no, _unquote(m_min.group(1)))
            continue

        m_ing = re.match(r"^['\"]?run\.googleapis\.com/ingress['\"]?\s*:\s*(.*)$", code)
        if m_ing:
            ingress = (line_no, _unquote(m_ing.group(1)))
            continue

        m_timeout = re.match(r"^timeoutSeconds\s*:\s*(.*)$", code)
        if m_timeout:
            timeout_seconds = (line_no, _unquote(m_timeout.group(1)))
            continue

        flow_name = re.search(r"\bname\s*:\s*(['\"]?[A-Za-z0-9_]+['\"]?)", code)
        flow_val = re.search(r"\bvalue\s*:\s*([^,}]+)", code)
        if flow_name and flow_val:
            if _unquote(flow_name.group(1)) == "OM_LOGGING_LEVEL":
                logging_levels.append((line_no, _unquote(flow_val.group(1))))
            pending_env_name = None
            continue

        m_ename = re.match(r"^(?:-\s*)?name\s*:\s*(.+)$", code)
        if m_ename:
            pending_env_name = _unquote(m_ename.group(1))
            continue

        m_eval = re.match(r"^(?:-\s*)?value\s*:\s*(.*)$", code)
        if m_eval:
            if pending_env_name == "OM_LOGGING_LEVEL":
                logging_levels.append((line_no, _unquote(m_eval.group(1))))
            pending_env_name = None
            continue

        if code.startswith("- ") or ":" in code:
            pending_env_name = None

    if cpu_throttling is None:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                1,
                "Missing required annotation 'run.googleapis.com/cpu-throttling: \"false\"' "
                "(om-core requires always-on CPU for background Redis Stream replication).",
            )
        )
    elif cpu_throttling[1].lower() != "false":
        findings.append(
            Finding(
                "ERROR",
                display_path,
                cpu_throttling[0],
                f"Annotation 'run.googleapis.com/cpu-throttling' must be 'false', got '{cpu_throttling[1]}'.",
            )
        )

    if exec_env is None:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                1,
                "Missing required annotation 'run.googleapis.com/execution-environment: gen2'.",
            )
        )
    elif exec_env[1] != "gen2":
        findings.append(
            Finding(
                "ERROR",
                display_path,
                exec_env[0],
                f"Annotation 'run.googleapis.com/execution-environment' must be 'gen2', got '{exec_env[1]}'.",
            )
        )

    if min_scale is None:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                1,
                "Missing required annotation 'autoscaling.knative.dev/minScale' (must be >= 1, recommended >= 2).",
            )
        )
    else:
        try:
            min_val = int(min_scale[1], 10)
            if min_val < 1:
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        min_scale[0],
                        f"Annotation 'autoscaling.knative.dev/minScale' must be >= 1, got {min_val}.",
                    )
                )
            elif min_val < 2:
                findings.append(
                    Finding(
                        "WARN",
                        display_path,
                        min_scale[0],
                        f"Annotation 'autoscaling.knative.dev/minScale' is {min_val}; at least 2 is recommended for HA.",
                    )
                )
        except ValueError:
            findings.append(
                Finding(
                    "ERROR",
                    display_path,
                    min_scale[0],
                    f"Annotation 'autoscaling.knative.dev/minScale' must be an integer >= 1, got '{min_scale[1]}'.",
                )
            )

    if ingress is None:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                1,
                "Missing required annotation 'run.googleapis.com/ingress'.",
            )
        )
    elif ingress[1] not in VALID_INGRESS_VALUES:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                ingress[0],
                f"Annotation 'run.googleapis.com/ingress' has invalid value '{ingress[1]}'.",
            )
        )

    if timeout_seconds is None:
        findings.append(
            Finding(
                "WARN",
                display_path,
                1,
                f"Missing 'timeoutSeconds' on Cloud Run revision template. {CLOUD_RUN_TIMEOUT_RULE}",
            )
        )
    else:
        t_line, t_raw = timeout_seconds
        if not re.match(r"^[+-]?\d+$", t_raw):
            findings.append(
                Finding(
                    "ERROR",
                    display_path,
                    t_line,
                    f"Cloud Run 'timeoutSeconds' must be an integer, got '{t_raw}'. {CLOUD_RUN_TIMEOUT_RULE}",
                )
            )
        else:
            t_val = int(t_raw, 10)
            if t_val <= 0 or t_val > MAX_CLOUD_RUN_TIMEOUT_SECONDS:
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        t_line,
                        f"Cloud Run 'timeoutSeconds' must be between 1 and {MAX_CLOUD_RUN_TIMEOUT_SECONDS}, got {t_val}. {CLOUD_RUN_TIMEOUT_RULE}",
                    )
                )
            elif t_val < MIN_CLOUD_RUN_TIMEOUT_SECONDS:
                findings.append(
                    Finding(
                        "WARN",
                        display_path,
                        t_line,
                        f"Cloud Run 'timeoutSeconds' is {t_val} s, below minimum {MIN_CLOUD_RUN_TIMEOUT_SECONDS} s. {CLOUD_RUN_TIMEOUT_RULE}",
                    )
                )

    for lvl_line, lvl_val in logging_levels:
        if lvl_val.lower() in ("debug", "trace"):
            findings.append(
                Finding(
                    "WARN",
                    display_path,
                    lvl_line,
                    f"Environment variable 'OM_LOGGING_LEVEL' is '{lvl_val}'; use 'info' "
                    "(the default in internal/config/config.go) or higher in production.",
                )
            )

    containers = _parse_k8s_containers(lines)
    om_core_container = _find_om_core_container(containers)
    if om_core_container is not None:
        missing_limits: List[str] = []
        if not om_core_container.limits_cpu:
            missing_limits.append("cpu")
        if not om_core_container.limits_memory:
            missing_limits.append("memory")
        if missing_limits:
            cname = om_core_container.name or "om-core"
            findings.append(
                Finding(
                    "WARN",
                    display_path,
                    om_core_container.start_line,
                    f"Cloud Run container '{cname}' is missing resource limit(s): "
                    f"{', '.join(missing_limits)} under 'resources.limits' "
                    "(recommended cpu: '2' and memory: 1Gi in deploy/cloud_run/service.yaml).",
                )
            )


def _audit_otel_collector_config(
    display_path: str,
    lines: Sequence[str],
    findings: List[Finding],
) -> None:
    """Audits an OpenTelemetry Collector sidecar config.yaml for googlemanagedprometheus."""
    has_gmp = any(
        "googlemanagedprometheus" in _strip_inline_comment(line)
        for line in lines
    )
    if not has_gmp:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                1,
                "OTel Collector configuration is missing 'googlemanagedprometheus' exporter.",
            )
        )


def _audit_cloudbuild_yaml(
    display_path: str,
    lines: Sequence[str],
    substitutions: Dict[str, Tuple[int, str]],
    findings: List[Finding],
) -> None:
    """Audits Cloud Build gcloud run deploy steps for CPU, gen2, min-instances, auth, ingress, and timeout flags."""
    # Group args tokens per Cloud Build step so flags from later steps cannot bleed in.
    step_token_groups: List[List[Tuple[int, str]]] = []
    current_tokens: List[Tuple[int, str]] = []
    in_steps = False
    steps_indent = 0
    step_item_indent: Optional[int] = None

    for line_no, raw_line in enumerate(lines, start=1):
        raw_code = _strip_inline_comment(raw_line)
        code = raw_code.strip()
        if not code:
            continue

        indent = len(raw_code) - len(raw_code.lstrip(" "))
        if re.match(r"^steps\s*:\s*$", code):
            in_steps = True
            steps_indent = indent
            step_item_indent = None
            continue

        if in_steps and indent <= steps_indent and not code.startswith("-"):
            if current_tokens:
                step_token_groups.append(current_tokens)
                current_tokens = []
            in_steps = False
            step_item_indent = None

        if in_steps and code.startswith("- "):
            item_after_dash = code[2:].strip()
            is_step_header = bool(re.match(r"^(?:name|id|entrypoint|waitFor|env|dir|secretEnv)\s*:", item_after_dash))
            if is_step_header and (step_item_indent is None or indent == step_item_indent):
                step_item_indent = indent
                if current_tokens:
                    step_token_groups.append(current_tokens)
                    current_tokens = []
                continue

        # Flow array args: args: ['run', 'deploy', ...]
        flow_args = re.match(r"^args\s*:\s*\[(.*)\]\s*$", code)
        if flow_args:
            inner = flow_args.group(1)
            for part in inner.split(","):
                cleaned = _unquote(part)
                if cleaned:
                    current_tokens.append((line_no, cleaned))
            continue

        list_item = re.match(r"^-\s+(.+)$", code)
        if list_item:
            item_val = _unquote(list_item.group(1))
            if not item_val or (":" in item_val and not item_val.startswith("-") and "=" not in item_val):
                continue
            # Support multi-token list items such as "- --execution-environment gen2"
            if item_val.startswith("--") and " " in item_val and "=" not in item_val:
                try:
                    for sub_tok in shlex.split(item_val):
                        current_tokens.append((line_no, sub_tok))
                except ValueError:
                    current_tokens.append((line_no, item_val))
            else:
                current_tokens.append((line_no, item_val))

    if current_tokens:
        step_token_groups.append(current_tokens)

    for group in step_token_groups:
        deploy_indices: List[int] = []
        for idx in range(len(group) - 1):
            if group[idx][1] == "run" and group[idx + 1][1] == "deploy":
                deploy_indices.append(idx)

        for step_idx, start_pos in enumerate(deploy_indices):
            end_pos = deploy_indices[step_idx + 1] if step_idx + 1 < len(deploy_indices) else len(group)
            step_tokens = group[start_pos:end_pos]
            step_line = step_tokens[0][0]

            has_no_cpu_throttling = False
            has_cpu_throttling_enabled = False
            cpu_line = step_line

            exec_env_val: Optional[str] = None
            exec_env_line = step_line
            has_exec_env_flag = False

            min_instances_val: Optional[str] = None
            min_instances_line = step_line
            has_min_instances_flag = False

            has_no_allow_unauth = False
            has_allow_unauth = False
            auth_line = step_line

            has_ingress_flag = False
            ingress_val: Optional[str] = None
            ingress_line = step_line

            seen_container_flag = False
            service_timeout: Optional[Tuple[int, str]] = None
            container_scoped_timeout_line: Optional[int] = None

            i = 0
            while i < len(step_tokens):
                line_no, tok = step_tokens[i]

                if tok == "--container":
                    seen_container_flag = True
                    if i + 1 < len(step_tokens) and not step_tokens[i + 1][1].startswith("-"):
                        i += 1
                    i += 1
                    continue
                elif tok.startswith("--container="):
                    seen_container_flag = True
                    i += 1
                    continue

                if tok == "--timeout":
                    if i + 1 < len(step_tokens) and not step_tokens[i + 1][1].startswith("-"):
                        t_raw = _unquote(step_tokens[i + 1][1])
                        i += 1
                    else:
                        t_raw = ""
                    if seen_container_flag:
                        container_scoped_timeout_line = line_no
                    else:
                        service_timeout = (line_no, t_raw)
                    i += 1
                    continue
                elif tok.startswith("--timeout="):
                    t_raw = _unquote(tok.split("=", 1)[1])
                    if seen_container_flag:
                        container_scoped_timeout_line = line_no
                    else:
                        service_timeout = (line_no, t_raw)
                    i += 1
                    continue

                if tok == "--no-cpu-throttling":
                    has_no_cpu_throttling = True
                    cpu_line = line_no
                elif tok == "--cpu-throttling":
                    has_cpu_throttling_enabled = True
                    cpu_line = line_no
                elif tok.startswith("--cpu-throttling="):
                    val = _unquote(tok.split("=", 1)[1]).lower()
                    cpu_line = line_no
                    if val in ("false", "0", "no"):
                        has_no_cpu_throttling = True
                    else:
                        has_cpu_throttling_enabled = True

                elif tok == "--execution-environment":
                    has_exec_env_flag = True
                    exec_env_line = line_no
                    if i + 1 < len(step_tokens) and not step_tokens[i + 1][1].startswith("-"):
                        exec_env_val = _unquote(step_tokens[i + 1][1])
                        i += 1
                    else:
                        exec_env_val = ""
                elif tok.startswith("--execution-environment="):
                    has_exec_env_flag = True
                    exec_env_line = line_no
                    exec_env_val = _unquote(tok.split("=", 1)[1])

                elif tok == "--min-instances":
                    has_min_instances_flag = True
                    min_instances_line = line_no
                    if i + 1 < len(step_tokens) and not step_tokens[i + 1][1].startswith("-"):
                        min_instances_val = _unquote(step_tokens[i + 1][1])
                        i += 1
                    else:
                        min_instances_val = ""
                elif tok.startswith("--min-instances="):
                    has_min_instances_flag = True
                    min_instances_line = line_no
                    min_instances_val = _unquote(tok.split("=", 1)[1])

                elif tok == "--no-allow-unauthenticated":
                    has_no_allow_unauth = True
                    auth_line = line_no
                elif tok == "--allow-unauthenticated":
                    has_allow_unauth = True
                    auth_line = line_no

                elif tok == "--ingress":
                    has_ingress_flag = True
                    ingress_line = line_no
                    if i + 1 < len(step_tokens) and not step_tokens[i + 1][1].startswith("-"):
                        ingress_val = _unquote(step_tokens[i + 1][1])
                        i += 1
                    else:
                        ingress_val = ""
                elif tok.startswith("--ingress="):
                    has_ingress_flag = True
                    ingress_line = line_no
                    ingress_val = _unquote(tok.split("=", 1)[1])

                i += 1

            if has_cpu_throttling_enabled or not has_no_cpu_throttling:
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        cpu_line,
                        "gcloud run deploy step must include '--no-cpu-throttling' "
                        "(and must not enable '--cpu-throttling').",
                    )
                )

            if not has_exec_env_flag:
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        step_line,
                        "gcloud run deploy step is missing '--execution-environment gen2'.",
                    )
                )
            elif exec_env_val != "gen2":
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        exec_env_line,
                        f"gcloud run deploy '--execution-environment' must be 'gen2', got '{exec_env_val}'.",
                    )
                )

            if not has_min_instances_flag:
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        step_line,
                        "gcloud run deploy step is missing '--min-instances' (must be >= 1, recommended >= 2).",
                    )
                )
            else:
                try:
                    min_int = int(min_instances_val or "", 10)
                    if min_int < 1:
                        findings.append(
                            Finding(
                                "ERROR",
                                display_path,
                                min_instances_line,
                                f"gcloud run deploy '--min-instances' must be >= 1, got {min_int}.",
                            )
                        )
                    elif min_int < 2:
                        findings.append(
                            Finding(
                                "WARN",
                                display_path,
                                min_instances_line,
                                f"gcloud run deploy '--min-instances' is {min_int}; at least 2 is recommended for HA.",
                            )
                        )
                except ValueError:
                    findings.append(
                        Finding(
                            "ERROR",
                            display_path,
                            min_instances_line,
                            f"gcloud run deploy '--min-instances' must be an integer >= 1, got '{min_instances_val}'.",
                        )
                    )

            if has_allow_unauth or not has_no_allow_unauth:
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        auth_line,
                        "gcloud run deploy step must include '--no-allow-unauthenticated' "
                        "(and must not enable '--allow-unauthenticated').",
                    )
                )

            if not has_ingress_flag:
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        step_line,
                        "gcloud run deploy step is missing '--ingress'.",
                    )
                )
            elif ingress_val not in VALID_INGRESS_VALUES:
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        ingress_line,
                        f"gcloud run deploy '--ingress' must be one of {sorted(VALID_INGRESS_VALUES)}, got '{ingress_val}'.",
                    )
                )

            if container_scoped_timeout_line is not None and service_timeout is None:
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        container_scoped_timeout_line,
                        f"gcloud run deploy '--timeout' must be placed among service-level flags before the first '--container' flag. {CLOUD_RUN_TIMEOUT_RULE}",
                    )
                )
            elif service_timeout is None:
                findings.append(
                    Finding(
                        "WARN",
                        display_path,
                        step_line,
                        f"gcloud run deploy step is missing '--timeout'. {CLOUD_RUN_TIMEOUT_RULE}",
                    )
                )
            else:
                if container_scoped_timeout_line is not None:
                    findings.append(
                        Finding(
                            "ERROR",
                            display_path,
                            container_scoped_timeout_line,
                            f"gcloud run deploy '--timeout' must be placed before the first '--container' flag, not inside a container section. {CLOUD_RUN_TIMEOUT_RULE}",
                        )
                    )
                t_line, t_val_str = service_timeout
                sub_match = re.match(r"^\$\{([A-Za-z0-9_]+)\}$", t_val_str)
                if sub_match:
                    sub_key = sub_match.group(1)
                    if sub_key in substitutions:
                        t_line, t_val_str = substitutions[sub_key]
                        t_val_str = _unquote(t_val_str)
                if not t_val_str.startswith("$"):
                    numeric_str = (
                        t_val_str[:-1]
                        if (len(t_val_str) > 1 and t_val_str.endswith("s"))
                        else t_val_str
                    )
                    if not re.match(r"^[+-]?\d+$", numeric_str):
                        findings.append(
                            Finding(
                                "ERROR",
                                display_path,
                                t_line,
                                f"gcloud run deploy '--timeout' must be an integer (with optional trailing 's'), got '{t_val_str}'. {CLOUD_RUN_TIMEOUT_RULE}",
                            )
                        )
                    else:
                        t_parsed = int(numeric_str, 10)
                        if t_parsed <= 0 or t_parsed > MAX_CLOUD_RUN_TIMEOUT_SECONDS:
                            findings.append(
                                Finding(
                                    "ERROR",
                                    display_path,
                                    t_line,
                                    f"gcloud run deploy '--timeout' must be between 1 and {MAX_CLOUD_RUN_TIMEOUT_SECONDS} s, got {t_parsed}. {CLOUD_RUN_TIMEOUT_RULE}",
                                )
                            )
                        elif t_parsed < MIN_CLOUD_RUN_TIMEOUT_SECONDS:
                            findings.append(
                                Finding(
                                    "WARN",
                                    display_path,
                                    t_line,
                                    f"gcloud run deploy '--timeout' is {t_parsed} s, below minimum {MIN_CLOUD_RUN_TIMEOUT_SECONDS} s. {CLOUD_RUN_TIMEOUT_RULE}",
                                )
                            )


def _audit_gke_manifest(
    display_path: str,
    lines: Sequence[str],
    findings: List[Finding],
) -> None:
    """Audits Kubernetes / GKE Deployment manifests (e.g. deploy/gke/om.yaml)."""
    docs: List[Tuple[int, List[str]]] = []
    cur_start = 0
    cur_lines: List[str] = []
    for idx, raw_line in enumerate(lines):
        if re.match(r"^---\s*$", _strip_inline_comment(raw_line).strip()):
            if cur_lines:
                docs.append((cur_start, cur_lines))
            cur_start = idx + 1
            cur_lines = []
        else:
            cur_lines.append(raw_line)
    if cur_lines:
        docs.append((cur_start, cur_lines))

    for doc_offset, doc_lines in docs:
        deploy_line: Optional[int] = None
        for rel_idx, raw_line in enumerate(doc_lines):
            if re.match(r"^kind\s*:\s*Deployment\b", _strip_inline_comment(raw_line).strip()):
                deploy_line = doc_offset + rel_idx + 1
                break
        if deploy_line is None:
            continue

        grace_period: Optional[Tuple[int, str]] = None
        for rel_idx, raw_line in enumerate(doc_lines):
            line_no = doc_offset + rel_idx + 1
            code = _strip_inline_comment(raw_line).strip()
            if not code:
                continue
            m_rep = re.match(r"^replicas\s*:\s*(\S+)$", code)
            if m_rep:
                raw_rep = _unquote(m_rep.group(1))
                try:
                    rep_val = int(raw_rep, 10)
                    if rep_val < 1:
                        findings.append(
                            Finding(
                                "ERROR",
                                display_path,
                                line_no,
                                f"GKE Deployment 'replicas' must be >= 1, got {rep_val}.",
                            )
                        )
                    elif rep_val < 2:
                        findings.append(
                            Finding(
                                "WARN",
                                display_path,
                                line_no,
                                f"GKE Deployment 'replicas' is {rep_val}; at least 2 replicas are recommended for production HA.",
                            )
                        )
                except ValueError:
                    findings.append(
                        Finding(
                            "ERROR",
                            display_path,
                            line_no,
                            f"GKE Deployment 'replicas' must be an integer >= 1, got '{raw_rep}'.",
                        )
                    )
                continue

            m_grace = re.match(r"^terminationGracePeriodSeconds\s*:\s*(.*)$", code)
            if m_grace:
                grace_period = (line_no, _unquote(m_grace.group(1)))

        containers = _parse_k8s_containers(doc_lines, start_line_offset=doc_offset)
        om_core = _find_om_core_container(containers)
        prestop_seconds_int: Optional[int] = None

        if om_core is None:
            findings.append(
                Finding(
                    "WARN",
                    display_path,
                    deploy_line,
                    "GKE Deployment has no 'om-core' container with 'lifecycle.preStop.sleep.seconds' configured "
                    "(native sleep action requires Kubernetes 1.30 or later and lets the pod leave Service endpoints before SIGTERM).",
                )
            )
        elif om_core.prestop_sleep_seconds is None:
            cname = om_core.name or "om-core"
            findings.append(
                Finding(
                    "WARN",
                    display_path,
                    om_core.start_line,
                    f"GKE Deployment container '{cname}' has no 'lifecycle.preStop.sleep.seconds' configured "
                    "(native sleep action requires Kubernetes 1.30 or later and lets the pod leave Service endpoints before SIGTERM).",
                )
            )
        else:
            ps_line, ps_raw = om_core.prestop_sleep_seconds
            cname = om_core.name or "om-core"
            if not re.match(r"^[+-]?\d+$", ps_raw):
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        ps_line,
                        f"GKE Deployment container '{cname}' 'lifecycle.preStop.sleep.seconds' must be an integer, got '{ps_raw}'.",
                    )
                )
            else:
                ps_val = int(ps_raw, 10)
                if ps_val <= 0:
                    findings.append(
                        Finding(
                            "ERROR",
                            display_path,
                            ps_line,
                            f"GKE Deployment container '{cname}' 'lifecycle.preStop.sleep.seconds' must be > 0, got {ps_val}.",
                        )
                    )
                else:
                    prestop_seconds_int = ps_val

        effective_prestop = prestop_seconds_int if prestop_seconds_int is not None else 0
        min_grace = effective_prestop + DEFAULT_SHUTDOWN_BUDGET_SECONDS

        if grace_period is None:
            findings.append(
                Finding(
                    "WARN",
                    display_path,
                    deploy_line,
                    f"GKE Deployment is missing 'terminationGracePeriodSeconds'; must be at least "
                    f"lifecycle.preStop.sleep.seconds + {DEFAULT_SHUTDOWN_BUDGET_SECONDS} s "
                    "(covering the 5 s server drain default in start in server.go and the 3 s replication stop default in main.go).",
                )
            )
        else:
            g_line, g_raw = grace_period
            if not re.match(r"^[+-]?\d+$", g_raw):
                findings.append(
                    Finding(
                        "ERROR",
                        display_path,
                        g_line,
                        f"GKE Deployment 'terminationGracePeriodSeconds' must be an integer, got '{g_raw}' "
                        "(must cover lifecycle.preStop.sleep.seconds plus the 5 s server drain default in start in server.go and the 3 s replication stop default in main.go).",
                    )
                )
            else:
                g_val = int(g_raw, 10)
                if g_val < 0:
                    findings.append(
                        Finding(
                            "ERROR",
                            display_path,
                            g_line,
                            f"GKE Deployment 'terminationGracePeriodSeconds' must be >= 0, got {g_val}.",
                        )
                    )
                elif g_val < min_grace:
                    findings.append(
                        Finding(
                            "WARN",
                            display_path,
                            g_line,
                            f"GKE Deployment 'terminationGracePeriodSeconds' is {g_val} s, shorter than "
                            f"preStop sleep ({effective_prestop} s) + {DEFAULT_SHUTDOWN_BUDGET_SECONDS} s ({min_grace} s), "
                            "covering the 5 s server drain default in start in server.go and the 3 s replication stop default in main.go.",
                        )
                    )


def audit_file(
    file_path: Path,
    display_path: str,
    strict_templates: bool,
) -> List[Finding]:
    findings: List[Finding] = []
    content = file_path.read_text(encoding="utf-8")
    lines = content.splitlines()

    _check_common_text_rules(file_path, display_path, lines, strict_templates, findings)

    is_dockerfile = (
        file_path.name == "Dockerfile"
        or file_path.name.endswith(".Dockerfile")
        or any(
            _strip_inline_comment(line).strip().upper().startswith("FROM ")
            for line in lines
        )
    )
    if is_dockerfile:
        _audit_otel_dockerfile(display_path, lines, findings)
        return findings

    is_otel_collector_cfg = (
        not any(re.match(r"^apiVersion\s*:", _strip_inline_comment(line)) for line in lines)
        and any(re.match(r"^receivers\s*:", _strip_inline_comment(line)) for line in lines)
        and any(re.match(r"^exporters\s*:", _strip_inline_comment(line)) for line in lines)
    )
    if is_otel_collector_cfg:
        _audit_otel_collector_config(display_path, lines, findings)
        return findings

    substitutions = _extract_substitutions(lines)
    _audit_env_vars(display_path, lines, substitutions, findings)

    has_knative_service = any(
        "serving.knative.dev" in _strip_inline_comment(line)
        or "run.googleapis.com/" in _strip_inline_comment(line)
        for line in lines
    )
    if has_knative_service:
        _audit_knative_service_yaml(display_path, lines, findings)

    has_cloudbuild_steps = any(
        re.match(r"^steps\s*:", _strip_inline_comment(line).strip()) for line in lines
    )
    if has_cloudbuild_steps:
        _audit_cloudbuild_yaml(display_path, lines, substitutions, findings)

    has_k8s_deployment = any(
        re.match(r"^kind\s*:\s*Deployment\b", _strip_inline_comment(line).strip())
        for line in lines
    )
    if has_k8s_deployment:
        _audit_gke_manifest(display_path, lines, findings)

    return findings


def _resolve_target_paths(raw_paths: Sequence[str]) -> Tuple[List[Tuple[Path, str]], List[str]]:
    resolved: List[Tuple[Path, str]] = []
    missing_errors: List[str] = []

    if raw_paths:
        for raw in raw_paths:
            p = Path(raw)
            if not p.exists():
                missing_errors.append(f"[ERROR] {raw}: file does not exist.")
            elif p.is_dir():
                missing_errors.append(f"[ERROR] {raw}: path is a directory, expected a file.")
            elif not is_readable(p):
                missing_errors.append(f"[ERROR] {raw}: file is not readable.")
            else:
                resolved.append((p, raw))
        return resolved, missing_errors

    repo = _repo_root()
    for rel in DEFAULT_RELATIVE_PATHS:
        cwd_candidate = Path.cwd() / rel
        repo_candidate = repo / rel
        if cwd_candidate.exists() and cwd_candidate.is_file() and is_readable(cwd_candidate):
            resolved.append((cwd_candidate, rel))
        elif repo_candidate.exists() and repo_candidate.is_file() and is_readable(repo_candidate):
            resolved.append((repo_candidate, rel))
        else:
            missing_errors.append(f"[ERROR] {rel}: default manifest file is missing or unreadable.")
    return resolved, missing_errors


def is_readable(path: Path) -> bool:
    try:
        with path.open("rb") as f:
            f.read(1)
        return True
    except OSError:
        return False


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        description="Validate Open Match 2 Cloud Run, Cloud Build, OTel sidecar, and GKE deployment manifests."
    )
    parser.add_argument(
        "--strict-templates",
        action="store_true",
        help="Treat unreplaced template placeholders ($SERVICE_ACCOUNT, $PRIMARY_ENDPOINT, etc.) "
        "and sample template identifiers as errors (exit code 1) instead of warnings.",
    )
    parser.add_argument(
        "paths",
        nargs="*",
        help="Deployment YAML or Dockerfile paths to validate (defaults to repository manifests).",
    )
    args = parser.parse_args(argv)

    targets, missing_errors = _resolve_target_paths(args.paths)
    if missing_errors:
        for err_msg in missing_errors:
            print(err_msg, file=sys.stderr)
        return 2

    all_findings: List[Finding] = []
    for file_path, display_path in targets:
        try:
            file_findings = audit_file(file_path, display_path, args.strict_templates)
            all_findings.extend(file_findings)
        except OSError as exc:
            print(f"[ERROR] {display_path}: failed to read file: {exc}", file=sys.stderr)
            return 2

    errors = [f for f in all_findings if f.severity == "ERROR"]
    warnings = [f for f in all_findings if f.severity == "WARN"]

    for finding in all_findings:
        stream = sys.stderr if finding.severity == "ERROR" else sys.stdout
        print(finding.format(), file=stream)

    if errors:
        print(
            f"Validation FAILED: {len(errors)} error(s), {len(warnings)} warning(s) across {len(targets)} file(s).",
            file=sys.stderr,
        )
        return 1

    print(
        f"Validation PASSED: 0 errors, {len(warnings)} warning(s) across {len(targets)} file(s)."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
