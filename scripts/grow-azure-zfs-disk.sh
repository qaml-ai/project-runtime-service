#!/usr/bin/env bash
# Grow an Azure managed disk backing a whole-disk ZFS pool when headroom is low.
#
# Required config is loaded from /etc/qaml-project-runtime/disk-grow.env by default:
#   AZURE_SUBSCRIPTION_ID
#   AZURE_RESOURCE_GROUP
#   AZURE_DISK_NAME
#   AZURE_DISK_LUN
#   ZPOOL_NAME
#
# The VM's managed identity must be able to read/update AZURE_DISK_NAME.
set -euo pipefail

CONFIG_FILE="${CONFIG_FILE:-/etc/qaml-project-runtime/disk-grow.env}"
. "$CONFIG_FILE"

ZPOOL_NAME="${ZPOOL_NAME:-projectruntime}"
MIN_FREE_BYTES="${MIN_FREE_BYTES:-107374182400}"
GROW_AT_CAP_PERCENT="${GROW_AT_CAP_PERCENT:-85}"
GROW_INCREMENT_GB="${GROW_INCREMENT_GB:-1024}"
MAX_DISK_GB="${MAX_DISK_GB:-10240}"
AZURE_DISK_LUN="${AZURE_DISK_LUN:-1}"

read -r _pool_size pool_free pool_cap < <(zpool list -Hp -o size,free,capacity "$ZPOOL_NAME")
pool_cap="${pool_cap%%%}"

if (( pool_free >= MIN_FREE_BYTES && pool_cap < GROW_AT_CAP_PERCENT )); then
  echo "disk grow not needed: free=${pool_free} cap=${pool_cap}%"
  exit 0
fi

if ! az account show >/dev/null 2>&1; then
  az login --identity --allow-no-subscriptions >/dev/null
fi
az account set --subscription "$AZURE_SUBSCRIPTION_ID" >/dev/null

current_gb="$(az disk show --resource-group "$AZURE_RESOURCE_GROUP" --name "$AZURE_DISK_NAME" --query diskSizeGB -o tsv)"
if (( current_gb >= MAX_DISK_GB )); then
  echo "disk already at max: current=${current_gb}GB max=${MAX_DISK_GB}GB free=${pool_free} cap=${pool_cap}%"
  exit 0
fi

next_gb=$((current_gb + GROW_INCREMENT_GB))
if (( next_gb > MAX_DISK_GB )); then
  next_gb=$MAX_DISK_GB
fi

echo "growing ${AZURE_DISK_NAME}: ${current_gb}GB -> ${next_gb}GB (free=${pool_free}, cap=${pool_cap}%)"
az disk update --resource-group "$AZURE_RESOURCE_GROUP" --name "$AZURE_DISK_NAME" --size-gb "$next_gb" -o none

for _ in $(seq 1 30); do
  observed="$(az disk show --resource-group "$AZURE_RESOURCE_GROUP" --name "$AZURE_DISK_NAME" --query diskSizeGB -o tsv)"
  [ "$observed" = "$next_gb" ] && break
  sleep 2
done

device="/dev/disk/azure/data/by-lun/${AZURE_DISK_LUN}"
resolved="$(readlink -f "$device")"
block="$(basename "$resolved")"
if [ -e "/sys/class/block/${block}/device/rescan" ]; then
  echo 1 > "/sys/class/block/${block}/device/rescan" || true
fi
blockdev --rereadpt "$resolved" 2>/dev/null || true
sleep 2

zpool online -e "$ZPOOL_NAME" "$device" || zpool online -e "$ZPOOL_NAME" "$resolved" || true
zpool list -o name,size,free,cap,autoexpand "$ZPOOL_NAME"
