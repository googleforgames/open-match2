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
  6. Cloud Run Knative annotations and gcloud run deploy flags for always-on CPU
     (--no-cpu-throttling / run.googleapis.com/cpu-throttling: 'false'), Gen2
     execution environment (gen2), and warm minimum instances (>= 1).

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
    "deploy/cloud_run/service.yaml",
    "deploy/cloud_run/cloudbuild.yaml",
    "deploy/cloud_run/otel-collector-sidecar/Dockerfile",
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

    if ingress is not None and ingress[1] not in VALID_INGRESS_VALUES:
        findings.append(
            Finding(
                "ERROR",
                display_path,
                ingress[0],
                f"Annotation 'run.googleapis.com/ingress' has invalid value '{ingress[1]}'.",
            )
        )


def _audit_cloudbuild_yaml(
    display_path: str,
    lines: Sequence[str],
    findings: List[Finding],
) -> None:
    """Audits Cloud Build gcloud run deploy steps for CPU, gen2, and min-instances flags."""
    # Collect args tokens with their source line numbers.
    tokens: List[Tuple[int, str]] = []
    for line_no, raw_line in enumerate(lines, start=1):
        code = _strip_inline_comment(raw_line).strip()
        if not code:
            continue

        # Flow array args: args: ['run', 'deploy', ...]
        flow_args = re.match(r"^args\s*:\s*\[(.*)\]\s*$", code)
        if flow_args:
            inner = flow_args.group(1)
            for part in inner.split(","):
                cleaned = _unquote(part)
                if cleaned:
                    tokens.append((line_no, cleaned))
            continue

        list_item = re.match(r"^-\s+(.+)$", code)
        if list_item:
            item_val = _unquote(list_item.group(1))
            if not item_val or ":" in item_val and not item_val.startswith("-") and "=" not in item_val:
                continue
            # Support multi-token list items such as "- --execution-environment gen2"
            if item_val.startswith("--") and " " in item_val and "=" not in item_val:
                try:
                    for sub_tok in shlex.split(item_val):
                        tokens.append((line_no, sub_tok))
                except ValueError:
                    tokens.append((line_no, item_val))
            else:
                tokens.append((line_no, item_val))

    # Locate sequences corresponding to `gcloud run deploy` steps.
    deploy_indices: List[int] = []
    for idx in range(len(tokens) - 1):
        if tokens[idx][1] == "run" and tokens[idx + 1][1] == "deploy":
            deploy_indices.append(idx)

    if not deploy_indices:
        return

    for step_idx, start_pos in enumerate(deploy_indices):
        end_pos = deploy_indices[step_idx + 1] if step_idx + 1 < len(deploy_indices) else len(tokens)
        step_tokens = tokens[start_pos:end_pos]
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

        i = 0
        while i < len(step_tokens):
            line_no, tok = step_tokens[i]

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


def _audit_gke_manifest(
    display_path: str,
    lines: Sequence[str],
    findings: List[Finding],
) -> None:
    """Audits Kubernetes / GKE Deployment manifests (e.g. deploy/gke/om.yaml)."""
    for line_no, raw_line in enumerate(lines, start=1):
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
        _audit_cloudbuild_yaml(display_path, lines, findings)

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
