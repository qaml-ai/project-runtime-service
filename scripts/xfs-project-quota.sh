#!/usr/bin/env bash
#
# Manage XFS project quotas for per-sandbox directories.
#
# Examples:
#   sudo ./xfs-project-quota.sh assign sandbox-123
#   sudo ./xfs-project-quota.sh set sandbox-123 20g 200k
#   sudo ./xfs-project-quota.sh show sandbox-123
#   sudo ./xfs-project-quota.sh clear sandbox-123
#
set -euo pipefail

MOUNT_ROOT="${MOUNT_ROOT:-/srv/sandboxes}"
PROJECTS_FILE="${PROJECTS_FILE:-/etc/projects}"
PROJID_FILE="${PROJID_FILE:-/etc/projid}"
PROJECT_ID_START="${PROJECT_ID_START:-10000}"

usage() {
  cat <<'USAGE'
Usage:
  xfs-project-quota.sh assign <sandbox_id>
  xfs-project-quota.sh set <sandbox_id> <bhard> <ihard>
  xfs-project-quota.sh show <sandbox_id>
  xfs-project-quota.sh clear <sandbox_id>

Notes:
  - mount must include prjquota (for example: defaults,noatime,prjquota).
  - <bhard> examples: 20g, 500m
  - <ihard> examples: 200k, 50000
USAGE
}

require_root() {
  if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    echo "ERROR: run as root."
    exit 1
  fi
}

require_tools() {
  command -v xfs_quota >/dev/null 2>&1 || {
    echo "ERROR: xfs_quota not found. Install xfsprogs."
    exit 1
  }
}

sanitize_project_name() {
  local sandbox_id="$1"
  local cleaned
  cleaned="$(printf '%s' "$sandbox_id" | tr -cs 'a-zA-Z0-9_-' '_')"
  echo "sb_${cleaned}"
}

sandbox_dir() {
  local sandbox_id="$1"
  echo "${MOUNT_ROOT}/${sandbox_id}"
}

ensure_files() {
  touch "$PROJECTS_FILE" "$PROJID_FILE"
}

get_project_id() {
  local project_name="$1"
  awk -F: -v name="$project_name" '$1 == name {print $2}' "$PROJID_FILE" | tail -n 1
}

next_project_id() {
  local max_id
  max_id="$(awk -F: -v start="$PROJECT_ID_START" '
    BEGIN { max = start - 1 }
    NF >= 2 {
      id = $2 + 0
      if (id > max) max = id
    }
    END { print max + 1 }
  ' "$PROJID_FILE")"
  echo "$max_id"
}

upsert_line_by_prefix() {
  local file="$1"
  local prefix="$2"
  local line="$3"
  local tmp
  tmp="$(mktemp)"
  awk -v prefix="$prefix" '$0 !~ ("^" prefix) { print $0 }' "$file" > "$tmp"
  echo "$line" >> "$tmp"
  cat "$tmp" > "$file"
  rm -f "$tmp"
}

remove_line_by_prefix() {
  local file="$1"
  local prefix="$2"
  local tmp
  tmp="$(mktemp)"
  awk -v prefix="$prefix" '$0 !~ ("^" prefix) { print $0 }' "$file" > "$tmp"
  cat "$tmp" > "$file"
  rm -f "$tmp"
}

assign_project() {
  local sandbox_id="$1"
  local project_name project_id dir
  project_name="$(sanitize_project_name "$sandbox_id")"
  dir="$(sandbox_dir "$sandbox_id")"

  mkdir -p "$dir"
  chmod 0700 "$dir"

  project_id="$(get_project_id "$project_name")"
  if [ -z "$project_id" ]; then
    project_id="$(next_project_id)"
  fi

  upsert_line_by_prefix "$PROJID_FILE" "${project_name}:" "${project_name}:${project_id}"
  upsert_line_by_prefix "$PROJECTS_FILE" "${project_id}:" "${project_id}:${dir}"

  xfs_quota -x -c "project -s ${project_name}" "$MOUNT_ROOT"
  echo "Assigned ${project_name} (${project_id}) -> ${dir}"
}

set_limits() {
  local sandbox_id="$1"
  local bhard="$2"
  local ihard="$3"
  local project_name
  project_name="$(sanitize_project_name "$sandbox_id")"

  assign_project "$sandbox_id"
  xfs_quota -x -c "limit -p bhard=${bhard} ihard=${ihard} ${project_name}" "$MOUNT_ROOT"
  echo "Applied limits for ${project_name}: bhard=${bhard}, ihard=${ihard}"
}

show_project() {
  local sandbox_id="$1"
  local project_name project_id
  project_name="$(sanitize_project_name "$sandbox_id")"
  project_id="$(get_project_id "$project_name")"
  if [ -z "$project_id" ]; then
    echo "No mapping found for ${project_name}"
    return 1
  fi
  echo "${project_name}:${project_id}"
  xfs_quota -x -c "quota -p ${project_name}" "$MOUNT_ROOT"
}

clear_project() {
  local sandbox_id="$1"
  local project_name
  project_name="$(sanitize_project_name "$sandbox_id")"

  # Clear limits first (best effort), then remove mappings.
  xfs_quota -x -c "limit -p bsoft=0 bhard=0 isoft=0 ihard=0 ${project_name}" "$MOUNT_ROOT" || true

  remove_line_by_prefix "$PROJID_FILE" "${project_name}:"
  # Remove any numeric mapping pointing to this sandbox path.
  local dir
  dir="$(sandbox_dir "$sandbox_id")"
  local tmp
  tmp="$(mktemp)"
  awk -F: -v d="$dir" '$2 != d { print $0 }' "$PROJECTS_FILE" > "$tmp"
  cat "$tmp" > "$PROJECTS_FILE"
  rm -f "$tmp"

  echo "Cleared quota mapping for ${project_name}"
}

main() {
  require_root
  require_tools
  ensure_files

  if [ "$#" -lt 2 ]; then
    usage
    exit 1
  fi

  local cmd="$1"
  shift

  case "$cmd" in
    assign)
      [ "$#" -eq 1 ] || { usage; exit 1; }
      assign_project "$1"
      ;;
    set)
      [ "$#" -eq 3 ] || { usage; exit 1; }
      set_limits "$1" "$2" "$3"
      ;;
    show)
      [ "$#" -eq 1 ] || { usage; exit 1; }
      show_project "$1"
      ;;
    clear)
      [ "$#" -eq 1 ] || { usage; exit 1; }
      clear_project "$1"
      ;;
    *)
      usage
      exit 1
      ;;
  esac
}

main "$@"
