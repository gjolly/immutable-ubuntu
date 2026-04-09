#!/bin/bash
set -euo pipefail

VM_NAME="immutable-ubuntu-test-$$"
BINARY="./immutable-ubuntu"
METADATA_PATH="/etc/immutable-ubuntu/image-metadata.yaml"

cleanup() {
    echo "Deleting VM $VM_NAME..."
    lxc delete --force "$VM_NAME" 2>/dev/null || true
}
trap cleanup EXIT

# Build
echo "Building..."
CGO_ENABLED=0 go build -o "$BINARY" .

# Launch VM
echo "Launching VM $VM_NAME..."
lxc launch --vm ubuntu:24.04 "$VM_NAME"

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
METADATA=$(lxc exec "$VM_NAME" -- cat "$METADATA_PATH")
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

echo "PASS"
