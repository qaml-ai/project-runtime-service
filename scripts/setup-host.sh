#!/usr/bin/env bash
#
# Azure VM provisioning script for Chiridion sandbox host.
# Storage model:
#   - Durable Azure Premium SSD v2 attached disk
#   - XFS mounted at /srv/sandboxes (with prjquota enabled)
#   - One directory per sandbox under /srv/sandboxes
#
# Usage: sudo bash setup-host.sh
#
set -euo pipefail

log() {
  echo "$@"
}

ensure_line_in_file() {
  local line="$1"
  local file="$2"
  if ! grep -Fqx "$line" "$file" 2>/dev/null; then
    echo "$line" >> "$file"
  fi
}

echo "=== Chiridion Sandbox Host Setup (XFS + Premium SSD v2) ==="

SANDBOXES_DIR="${SANDBOXES_DIR:-/srv/sandboxes}"
SANDBOX_DATA_DEVICE="${SANDBOX_DATA_DEVICE:-/dev/disk/azure/data/by-lun/0}"
DOCKER_DATA_ROOT="${SANDBOXES_DIR}/.docker"
SANDBOX_DEFAULT_BHARD="${SANDBOX_DEFAULT_BHARD:-100g}"
SANDBOX_DEFAULT_IHARD="${SANDBOX_DEFAULT_IHARD:-0}"

if [ -n "${ACR_LOGIN_SERVER:-}" ]; then
  SANDBOX_IMAGE="${ACR_LOGIN_SERVER}/chiridion-sandbox:latest"
else
  SANDBOX_IMAGE="chiridion-sandbox:latest"
fi

resolve_data_device() {
  if [ -b "$SANDBOX_DATA_DEVICE" ]; then
    echo "$SANDBOX_DATA_DEVICE"
    return 0
  fi

  # Try Azure symlinks (NVMe-attached managed disks use data/by-lun, SCSI use scsi1/lun)
  local lun_path
  for lun_path in "/dev/disk/azure/data/by-lun/0" "/dev/disk/azure/scsi1/lun0"; do
    if [ -b "$lun_path" ]; then
      echo "$lun_path"
      return 0
    fi
  done

  return 1
}

wait_for_data_device() {
  local attempt max_attempts=30
  for attempt in $(seq 1 "$max_attempts"); do
    if resolve_data_device >/dev/null 2>&1; then
      return 0
    fi
    log "  Waiting for data disk to appear (attempt ${attempt}/${max_attempts})..."
    sleep 5
  done
  return 1
}

setup_xfs_data_disk() {
  log "[1/7] Configuring durable data disk as XFS..."

  local data_device
  if ! data_device="$(resolve_data_device)"; then
    log "  ERROR: could not locate data disk. Set SANDBOX_DATA_DEVICE to your Premium SSD v2 device path."
    exit 1
  fi

  log "  Using data device: ${data_device}"

  local fs_type
  fs_type="$(blkid -o value -s TYPE "$data_device" 2>/dev/null || true)"
  if [ -z "$fs_type" ]; then
    log "  No filesystem found; formatting as XFS..."
    mkfs.xfs -f "$data_device"
    fs_type="xfs"
  fi

  if [ "$fs_type" != "xfs" ]; then
    log "  ERROR: expected XFS on ${data_device}, found ${fs_type}."
    exit 1
  fi

  local uuid
  uuid="$(blkid -o value -s UUID "$data_device")"
  local escaped_root
  escaped_root="$(printf '%s\n' "$SANDBOXES_DIR" | sed 's/[][\\/.^$*]/\\&/g')"

  mkdir -p "$SANDBOXES_DIR"
  sed -i "\\|[[:space:]]${escaped_root}[[:space:]]|d" /etc/fstab
  ensure_line_in_file "UUID=${uuid}  ${SANDBOXES_DIR}  xfs  defaults,noatime,prjquota,nofail  0  2" /etc/fstab

  if mountpoint -q "$SANDBOXES_DIR" 2>/dev/null; then
    log "  ${SANDBOXES_DIR} already mounted."
  else
    mount "$SANDBOXES_DIR"
    log "  Mounted ${SANDBOXES_DIR}"
  fi

  mkdir -p "${SANDBOXES_DIR}/.sandbox-host" "${SANDBOXES_DIR}/.docker"
  chmod 0755 "$SANDBOXES_DIR"
  chmod 0700 "${SANDBOXES_DIR}/.sandbox-host"
}

install_docker_and_runtime() {
  local docker_data_root="$1"

  log "[2/7] Installing Docker CE..."
  if ! command -v docker >/dev/null 2>&1; then
    curl -fsSL https://get.docker.com | sh
    systemctl enable --now docker
  else
    log "  Docker already installed."
  fi

  log "[3/7] Installing gVisor runtime (runsc)..."
  if ! command -v runsc >/dev/null 2>&1; then
    curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" \
      > /etc/apt/sources.list.d/gvisor.list
    apt-get update -qq
    apt-get install -y -qq runsc
  else
    log "  gVisor already installed."
  fi

  log "[4/7] Configuring Docker runtime and data-root..."
  cat > /etc/docker/daemon.json <<__DOCKER_DAEMON__
{
  "runtimes": {
    "runsc": {
      "path": "/usr/bin/runsc"
    }
  },
  "data-root": "${docker_data_root}"
}
__DOCKER_DAEMON__
  systemctl restart docker
}

install_go_and_host_service() {
  log "[5/7] Installing Go + sandbox-host service..."

  local required_go_version="${REQUIRED_GO_VERSION:-1.24.0}"
  local install_go_version="${INSTALL_GO_VERSION:-1.25.7}"

  have_required_go() {
    if ! command -v go >/dev/null 2>&1; then
      return 1
    fi
    local current
    current="$(go version | awk '{print $3}' | sed 's/^go//')"
    [ "$(printf '%s\n' "$required_go_version" "$current" | sort -V | head -n1)" = "$required_go_version" ]
  }

  if have_required_go; then
    log "  Go $(go version | awk '{print $3}') already installed (>= ${required_go_version})."
  else
    local go_arch
    go_arch="$(uname -m)"
    case "$go_arch" in
      x86_64) go_arch="amd64" ;;
      aarch64|arm64) go_arch="arm64" ;;
      *)
        log "  ERROR: unsupported architecture for Go install: ${go_arch}"
        exit 1
        ;;
    esac

    local go_tarball="go${install_go_version}.linux-${go_arch}.tar.gz"
    local go_url="https://go.dev/dl/${go_tarball}"
    log "  Installing Go ${install_go_version} from ${go_url}..."
    curl -fsSL "$go_url" -o "/tmp/${go_tarball}"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/${go_tarball}"
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
    log "  Installed $(go version)"
  fi

  local script_dir service_dir host_binary_path data_proxy_binary_path
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  service_dir="$(cd "${script_dir}/.." && pwd)"
  host_binary_path="/usr/local/bin/chiridion-sandbox-host"
  data_proxy_binary_path="/usr/local/bin/chiridion-data-proxy"
  local quota_tool_path="/usr/local/bin/chiridion-xfs-project-quota"

  go build -C "${service_dir}" -o "${host_binary_path}" ./cmd/sandbox-host
  go build -C "${service_dir}" -o "${data_proxy_binary_path}" ./cmd/data-proxy
  chmod 0755 "${host_binary_path}"
  chmod 0755 "${data_proxy_binary_path}"
  install -m 0755 "${service_dir}/scripts/xfs-project-quota.sh" "${quota_tool_path}"

  cat > /etc/systemd/system/chiridion-data-proxy.service <<__DATA_PROXY_SERVICE__
[Unit]
Description=Chiridion SQL Data Proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${service_dir}
ExecStart=${data_proxy_binary_path}
Restart=always
RestartSec=2
Environment=DATA_PROXY_PORT=8090
Environment=DATA_PROXY_MAX_REQUEST_BYTES=1048576
EnvironmentFile=-/etc/chiridion/sandbox-host.env
NoNewPrivileges=true
PrivateTmp=true
ProtectControlGroups=true
ProtectKernelModules=true
ProtectKernelTunables=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
LockPersonality=true
MemoryHigh=768M
MemoryMax=1024M
CPUQuota=200%
TasksMax=256
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
__DATA_PROXY_SERVICE__

  cat > /etc/systemd/system/chiridion-sandbox-host.service <<__HOST_SERVICE__
[Unit]
Description=Chiridion Sandbox Host
After=local-fs.target docker.service chiridion-sandbox-firewall.service chiridion-data-proxy.service
Requires=docker.service
Wants=chiridion-sandbox-firewall.service chiridion-data-proxy.service

[Service]
Type=simple
WorkingDirectory=${service_dir}
ExecStart=${host_binary_path}
Restart=always
RestartSec=5
Environment=PORT=80
Environment=SANDBOX_DOCKER_PROXY_PORT=8081
Environment=WORKSPACES_ROOT=${SANDBOXES_DIR}
Environment=SANDBOX_HOST_USAGE_DB_DIR=${SANDBOXES_DIR}/.sandbox-host/usage
Environment=SANDBOX_IMAGE=${SANDBOX_IMAGE}
Environment=CONTAINER_RUNTIME=runsc
Environment=SANDBOX_ENABLE_PROJECT_QUOTA=1
Environment=SANDBOX_DEFAULT_BHARD=${SANDBOX_DEFAULT_BHARD}
Environment=SANDBOX_DEFAULT_IHARD=${SANDBOX_DEFAULT_IHARD}
Environment=DATA_PROXY_PORT=8090
Environment=DATA_PROXY_UPSTREAM_URL=http://127.0.0.1:8090
EnvironmentFile=-/etc/chiridion/sandbox-host.env
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
__HOST_SERVICE__
}

install_firewall_service() {
  log "[6/7] Installing firewall rules service..."

  cat > /usr/local/bin/chiridion-apply-firewall.sh <<'__FIREWALL_SCRIPT__'
#!/usr/bin/env bash
set -euo pipefail

CONTROL_PORT="${PORT:-80}"

if ! command -v iptables >/dev/null 2>&1; then
  echo "[firewall] iptables not available; skipping rules"
  exit 0
fi

ensure_rule() {
  local cmd_check="$1"
  local cmd_add="$2"
  if ! eval "$cmd_check" >/dev/null 2>&1; then
    eval "$cmd_add"
  fi
}

ensure_rule \
  "iptables -C INPUT -i docker0 -p tcp --dport ${CONTROL_PORT} -j DROP" \
  "iptables -I INPUT 1 -i docker0 -p tcp --dport ${CONTROL_PORT} -j DROP"

if command -v ip6tables >/dev/null 2>&1; then
  ensure_rule \
    "ip6tables -C INPUT -i docker0 -p tcp --dport ${CONTROL_PORT} -j DROP" \
    "ip6tables -I INPUT 1 -i docker0 -p tcp --dport ${CONTROL_PORT} -j DROP"
fi

echo "[firewall] applied docker0 policy: drop :${CONTROL_PORT}"

# --- China outbound block via ipset ---
if command -v ipset >/dev/null 2>&1; then
  ipset create china-block hash:net -exist
  if curl -sf --connect-timeout 10 https://www.ipdeny.com/ipblocks/data/aggregated/cn-aggregated.zone -o /tmp/cn-zones.txt; then
    ipset flush china-block
    while read -r cidr; do
      ipset add china-block "$cidr" -exist
    done < /tmp/cn-zones.txt
    rm -f /tmp/cn-zones.txt

    ensure_rule \
      "iptables -C OUTPUT -m set --match-set china-block dst -j DROP" \
      "iptables -A OUTPUT -m set --match-set china-block dst -j DROP"
    ensure_rule \
      "iptables -C FORWARD -m set --match-set china-block dst -j DROP" \
      "iptables -I FORWARD -m set --match-set china-block dst -j DROP"

    echo "[firewall] china outbound block: $(ipset list china-block | grep -c '^[0-9]') CIDR ranges loaded"
  else
    echo "[firewall] WARNING: failed to download CN IP ranges; china block not applied"
  fi
else
  echo "[firewall] WARNING: ipset not available; china block not applied"
fi
__FIREWALL_SCRIPT__
  chmod +x /usr/local/bin/chiridion-apply-firewall.sh

  cat > /etc/systemd/system/chiridion-sandbox-firewall.service <<'__FIREWALL_SERVICE__'
[Unit]
Description=Apply Chiridion sandbox-host firewall policy
After=docker.service network-online.target
Wants=docker.service network-online.target
Before=chiridion-sandbox-host.service

[Service]
Type=oneshot
EnvironmentFile=-/etc/chiridion/sandbox-host.env
ExecStart=/usr/local/bin/chiridion-apply-firewall.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
__FIREWALL_SERVICE__
}


install_r2_mount_support() {
  log "Installing R2 FUSE support..."

  mkdir -p /run/chiridion-r2-creds
  chmod 0700 /run/chiridion-r2-creds

  if [ -f /etc/fuse.conf ] && ! grep -q '^user_allow_other$' /etc/fuse.conf; then
    echo user_allow_other >> /etc/fuse.conf
  fi

  # New containers mount their own R2 prefixes inside the sandbox with
  # short-lived, prefix-scoped credentials generated by sandbox-host.
  # Leave any currently running legacy global mount alone so existing containers
  # can keep working, but keep it from coming back after the next reboot.
  systemctl disable chiridion-s3fs-r2 2>/dev/null || true
}

install_cloudflared_and_acr() {
  log "[7/7] Installing cloudflared and pre-pulling sandbox image..."

  if ! command -v cloudflared >/dev/null 2>&1; then
    mkdir -p --mode=0755 /usr/share/keyrings
    curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
    echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared $(lsb_release -cs) main" \
      > /etc/apt/sources.list.d/cloudflared.list
    apt-get update -qq
    apt-get install -y -qq cloudflared >/dev/null 2>&1
  fi

  if [ -n "${CLOUDFLARED_TUNNEL_TOKEN:-}" ]; then
    cloudflared service install "$CLOUDFLARED_TUNNEL_TOKEN" 2>/dev/null || true
    systemctl enable --now cloudflared 2>/dev/null || true
  else
    log "  WARNING: CLOUDFLARED_TUNNEL_TOKEN not set, skipping tunnel setup."
  fi

  if [ -n "${ACR_LOGIN_SERVER:-}" ]; then
    if ! command -v az >/dev/null 2>&1; then
      curl -sL https://aka.ms/InstallAzureCLIDeb | bash >/dev/null 2>&1
    fi
    az acr login --name "${ACR_LOGIN_SERVER%%.*}" 2>/dev/null || true
    docker pull "$SANDBOX_IMAGE" 2>&1 || log "  WARNING: image pull failed; ensure the sandbox image exists in ACR."
  fi
}

apply_default_quotas() {
  local quota_tool="/usr/local/bin/chiridion-xfs-project-quota"
  if [ ! -x "$quota_tool" ]; then
    log "Quota helper not installed at ${quota_tool}; skipping quota backfill."
    return 0
  fi

  log "Applying default quota (${SANDBOX_DEFAULT_BHARD}, ihard=${SANDBOX_DEFAULT_IHARD}) to existing sandboxes..."
  local sandbox_dir sandbox_id
  for sandbox_dir in "${SANDBOXES_DIR}"/*; do
    [ -d "$sandbox_dir" ] || continue
    sandbox_id="$(basename "$sandbox_dir")"
    [ "$sandbox_id" = ".sandbox-host" ] && continue
    if ! MOUNT_ROOT="$SANDBOXES_DIR" "$quota_tool" set "$sandbox_id" "$SANDBOX_DEFAULT_BHARD" "$SANDBOX_DEFAULT_IHARD" >/dev/null 2>&1; then
      log "  WARNING: failed to apply quota for ${sandbox_id}"
    fi
  done
}

main() {
  apt-get update -qq
  apt-get install -y -qq xfsprogs curl ca-certificates gnupg lsb-release fuse3 ipset

  wait_for_data_device
  setup_xfs_data_disk
  install_docker_and_runtime "$DOCKER_DATA_ROOT"
  install_go_and_host_service
  install_firewall_service

  install_r2_mount_support
  install_cloudflared_and_acr
  apply_default_quotas

  systemctl daemon-reload
  systemctl enable --now chiridion-data-proxy 2>/dev/null || true
  systemctl enable --now chiridion-sandbox-firewall 2>/dev/null || true

  systemctl enable --now chiridion-sandbox-host 2>/dev/null || true

  echo ""
  echo "=== Setup complete ==="
  echo ""
  echo "Storage layout:"
  echo "  ${SANDBOXES_DIR}            - Durable Premium SSD v2 (XFS, prjquota enabled)"
  echo "  ${SANDBOXES_DIR}/<sandbox>  - Per-sandbox persistent root"
  echo "  Default per-sandbox quota   - ${SANDBOX_DEFAULT_BHARD} (ihard=${SANDBOX_DEFAULT_IHARD})"
  echo "  ${SANDBOXES_DIR}/.sandbox-host/usage - sandbox-host usage databases"
  echo "  ${DOCKER_DATA_ROOT}     - Docker data-root"
  echo ""
  echo "To verify:"
  echo "  findmnt ${SANDBOXES_DIR}"
  echo "  xfs_info ${SANDBOXES_DIR}"
  echo "  systemctl status chiridion-data-proxy"
  echo "  systemctl status chiridion-sandbox-host"
}

main "$@"
