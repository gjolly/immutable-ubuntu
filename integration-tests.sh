#!/bin/bash
set -euo pipefail

VM_NAME="immutable-ubuntu-test-$$"
BOOT_VM_NAME="immutable-ubuntu-boot-$$"
BINARY="./immutable-ubuntu"
METADATA_PATH="/etc/immutable-ubuntu/image-metadata.yaml"
LXD_VM_DIR="/var/snap/lxd/common/lxd/virtual-machines"
OUTPUT_IMG="/tmp/${VM_NAME}-output.img"
LOCAL_METADATA="/tmp/${VM_NAME}-metadata.yaml"
LOOP_DEV=""
KEEP_VM=0

while [ $# -gt 0 ]; do
    case "$1" in
        --keep-vm) KEEP_VM=1 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
    shift
done

cleanup() {
    if [ -n "$LOOP_DEV" ]; then
        echo "Detaching loop device $LOOP_DEV..."
        losetup -d "$LOOP_DEV" 2>/dev/null || true
    fi
    rm -f "$OUTPUT_IMG" "$LOCAL_METADATA"
    echo "Deleting VMs..."
    lxc delete --force "$VM_NAME" 2>/dev/null || true
    if [ "$KEEP_VM" = "1" ]; then
        echo "but keeping test VM for debugging: $BOOT_VM_NAME"
    else
        lxc delete --force "$BOOT_VM_NAME" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Build
echo "Building..."
CGO_ENABLED=0 go build -o "$BINARY" .

# Launch VM
echo "Launching VM $VM_NAME..."
lxc launch --vm ubuntu:24.04 "$VM_NAME" --device root,size=5GiB

echo "Waiting for LXD agent..."
for i in $(seq 1 60); do
    lxc exec "$VM_NAME" -- true 2>/dev/null && break
    sleep 2
done
lxc exec "$VM_NAME" -- true 2>/dev/null || { echo "FAIL: VM agent did not become ready"; exit 1; }

echo "Waiting for cloud-init..."
lxc exec "$VM_NAME" -- cloud-init status --wait >/dev/null

# Upload and run
echo "Uploading binary..."
lxc file push "$BINARY" "$VM_NAME/usr/local/bin/immutable-ubuntu"

echo "Running prepare..."
lxc exec "$VM_NAME" -- immutable-ubuntu prepare

# Read and verify metadata
echo "Verifying metadata..."
lxc file pull "$VM_NAME$METADATA_PATH" "$LOCAL_METADATA"
METADATA=$(cat "$LOCAL_METADATA")
echo "$METADATA"

fail() { echo "FAIL: $1"; exit 1; }

# Check each required field is present and non-empty
echo "$METADATA" | grep -qE '^cmdline: .+' \
    || fail "cmdline is empty"
echo "$METADATA" | grep -qE '^root_partuuid: .+' \
    || fail "root_partuuid is empty"
echo "$METADATA" | grep -qE '^esp_partuuid: .+' \
    || fail "esp_partuuid is empty"
echo "$METADATA" | grep -qE '^has_boot_partition:' \
    || fail "missing has_boot_partition field"

# Shut down VM so the disk is not in use.
echo "Stopping VM $VM_NAME..."
lxc stop "$VM_NAME"

# Attach the VM disk as a loop device with partition scanning.
DISK_IMG="$LXD_VM_DIR/$VM_NAME/root.img"
echo "Attaching $DISK_IMG as loop device..."
LOOP_DEV=$(losetup --find --partscan --show "$DISK_IMG")
echo "Loop device: $LOOP_DEV"

# Give udev a moment to create partition symlinks under /dev/disk/by-partuuid/.
udevadm settle --timeout=10

# Run freeze.
echo "Running freeze..."
"$BINARY" freeze \
  --config "$LOCAL_METADATA" \
  --volatile-dirs var,etc \
  --output "$OUTPUT_IMG"

echo "Verifying output image partition table..."
sgdisk -p "$OUTPUT_IMG"

# Detach the source loop device — no longer needed.
losetup -d "$LOOP_DEV"
LOOP_DEV=""

# ── Boot verification ────────────────────────────────────────────────────────

# Create a new VM and replace its disk with the generated image before first boot.
echo "Creating boot VM $BOOT_VM_NAME..."
lxc init --vm ubuntu:24.04 "$BOOT_VM_NAME" -c security.secureboot=false

echo "Installing generated image as boot VM disk..."
cp "$OUTPUT_IMG" "$LXD_VM_DIR/$BOOT_VM_NAME/root.img"

echo "Starting boot VM..."
lxc start "$BOOT_VM_NAME"

echo "Waiting for LXD agent in boot VM..."
for i in $(seq 1 90); do
    lxc exec "$BOOT_VM_NAME" -- true 2>/dev/null && break
    sleep 2
done
lxc exec "$BOOT_VM_NAME" -- true 2>/dev/null \
    || fail "boot VM agent did not become ready"

# Verify dm-verity device is present and in verified state.
echo "Verifying dm-verity..."
VERITY_STATUS=$(lxc exec "$BOOT_VM_NAME" -- dmsetup status root 2>&1)
echo "  dmsetup status root: $VERITY_STATUS"
echo "$VERITY_STATUS" | grep -q " verity " \
    || fail "dm-verity device 'root' not found in dmsetup output"
echo "$VERITY_STATUS" | grep -qE " V$" \
    || fail "dm-verity device is not in verified state (expected trailing 'V')"

# Verify the rootfs is mounted read-only.
echo "Verifying read-only rootfs..."
ROOT_OPTS=$(lxc exec "$BOOT_VM_NAME" -- findmnt --noheadings -o OPTIONS /)
echo "  root mount options: $ROOT_OPTS"
echo "$ROOT_OPTS" | grep -qE '(^|,)ro(,|$)' \
    || fail "rootfs is not mounted read-only"

# Confirm a write to the rootfs is rejected by the kernel.
echo "Verifying rootfs rejects writes..."
if lxc exec "$BOOT_VM_NAME" -- sh -c 'echo test > /immutable-write-test' 2>/dev/null; then
    fail "write to rootfs succeeded but should have been rejected"
fi

echo "PASS"
