#!/bin/bash
set -euo pipefail

UBUNTU_RELEASE="24.04"
VM_NAME="immutable-ubuntu-test-$$"
BOOT_VM_NAME="immutable-ubuntu-boot-$$"
BINARY="./immutable-ubuntu"
METADATA_PATH="/etc/immutable-ubuntu/image-metadata.yaml"
OUTPUT_IMG="/tmp/${VM_NAME}-output.img"
OUTPUT_PCR="${OUTPUT_IMG}.pcr.json"
LOCAL_METADATA="/tmp/${VM_NAME}-metadata.yaml"
LOOP_DEV=""
KEEP_VM=0

# Detect container manager: prefer incus, fall back to lxc.
if command -v incus &>/dev/null; then
    CLI=incus
elif command -v lxc &>/dev/null; then
    CLI=lxc
else
    echo "Neither incus nor lxc found on PATH"
    exit 1
fi

# Resolve the image source — LXD and Incus use different remotes.
image_source() {
    if [ "$CLI" = "incus" ]; then
        echo "images:ubuntu/$UBUNTU_RELEASE/cloud"
    else
        echo "ubuntu:$UBUNTU_RELEASE"
    fi
}

# Find the root disk image for a VM by querying the daemon's storage directory.
find_vm_disk() {
    local vm_name=$1
    for base in /var/lib/incus /var/snap/lxd/common/lxd /var/lib/lxd; do
        local disk="$base/virtual-machines/$vm_name/root.img"
        if [ -f "$disk" ]; then
            echo "$disk"
            return 0
        fi
    done
    echo "Could not find root.img for VM $vm_name" >&2
    return 1
}

while [ $# -gt 0 ]; do
    case "$1" in
        --keep-vm)
          KEEP_VM=1
          ;;
        --release)
          UBUNTU_RELEASE="$2"
          shift
          ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
    shift
done

echo "Using $CLI as container manager"

cleanup() {
    if [ -n "$LOOP_DEV" ]; then
        echo "Detaching loop device $LOOP_DEV..."
        losetup -d "$LOOP_DEV" 2>/dev/null || true
    fi
    rm -f "$OUTPUT_IMG" "$OUTPUT_PCR" "$LOCAL_METADATA"
    echo "Deleting VMs..."
    $CLI delete --force "$VM_NAME" 2>/dev/null || true
    if [ "$KEEP_VM" = "1" ]; then
        echo "but keeping test VM for debugging: $BOOT_VM_NAME"
    else
        $CLI delete --force "$BOOT_VM_NAME" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Build
echo "Building..."
CGO_ENABLED=0 go build -o "$BINARY" .

# Launch VM
echo "Launching VM $VM_NAME..."
$CLI launch --vm "$(image_source)" "$VM_NAME" --device root,size=5GiB

echo "Waiting for $CLI agent..."
for i in $(seq 1 60); do
    $CLI exec "$VM_NAME" -- true 2>/dev/null && break
    sleep 2
done
$CLI exec "$VM_NAME" -- true 2>/dev/null || { echo "FAIL: VM agent did not become ready"; exit 1; }

echo "Waiting for cloud-init..."
$CLI exec "$VM_NAME" -- cloud-init status --wait >/dev/null

# Upload and run
echo "Uploading binary..."
$CLI file push "$BINARY" "$VM_NAME/usr/local/bin/immutable-ubuntu"

echo "Running prepare..."
$CLI exec "$VM_NAME" -- immutable-ubuntu prepare

# Read and verify metadata
echo "Verifying metadata..."
$CLI file pull "$VM_NAME$METADATA_PATH" "$LOCAL_METADATA"
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
$CLI stop "$VM_NAME"

# Attach the VM disk as a loop device with partition scanning.
DISK_IMG=$(find_vm_disk "$VM_NAME")
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

# Verify PCR measurements if nitro-tpm-pcr-compute was available during freeze.
if command -v nitro-tpm-pcr-compute >/dev/null 2>&1; then
    echo "Verifying PCR reference measurements file..."
    [ -f "$OUTPUT_PCR" ] || fail "PCR measurements file not found: $OUTPUT_PCR"
    echo "Expected PCR measurements:"
    cat "$OUTPUT_PCR"
fi

# Detach the source loop device — no longer needed.
losetup -d "$LOOP_DEV"
LOOP_DEV=""

# ── Boot verification ────────────────────────────────────────────────────────

# Create a new VM and replace its disk with the generated image before first boot.
echo "Creating boot VM $BOOT_VM_NAME..."
$CLI init --vm "$(image_source)" "$BOOT_VM_NAME" -c security.secureboot=false

echo "Installing generated image as boot VM disk..."
BOOT_DISK_IMG=$(find_vm_disk "$BOOT_VM_NAME")
cp "$OUTPUT_IMG" "$BOOT_DISK_IMG"

echo "Starting boot VM..."
$CLI start "$BOOT_VM_NAME"

echo "Waiting for $CLI agent in boot VM..."
for i in $(seq 1 90); do
    $CLI exec "$BOOT_VM_NAME" -- true 2>/dev/null && break
    sleep 2
done
$CLI exec "$BOOT_VM_NAME" -- true 2>/dev/null \
    || fail "boot VM agent did not become ready"

# Verify dm-verity device is present and in verified state.
echo "Verifying dm-verity..."
VERITY_STATUS=$($CLI exec "$BOOT_VM_NAME" -- dmsetup status root 2>&1)
echo "  dmsetup status root: $VERITY_STATUS"
echo "$VERITY_STATUS" | grep -q " verity " \
    || fail "dm-verity device 'root' not found in dmsetup output"
echo "$VERITY_STATUS" | grep -qE " V$" \
    || fail "dm-verity device is not in verified state (expected trailing 'V')"

# Verify the rootfs is mounted read-only.
echo "Verifying read-only rootfs..."
ROOT_OPTS=$($CLI exec "$BOOT_VM_NAME" -- findmnt --noheadings -o OPTIONS /)
echo "  root mount options: $ROOT_OPTS"
echo "$ROOT_OPTS" | grep -qE '(^|,)ro(,|$)' \
    || fail "rootfs is not mounted read-only"

# Confirm a write to the rootfs is rejected by the kernel.
echo "Verifying rootfs rejects writes..."
if $CLI exec "$BOOT_VM_NAME" -- sh -c 'echo test > /immutable-write-test' 2>/dev/null; then
    fail "write to rootfs succeeded but should have been rejected"
fi

echo "PASS"
