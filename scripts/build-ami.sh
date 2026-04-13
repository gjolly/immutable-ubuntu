#!/usr/bin/env bash
# build-ami.sh — Build an AWS attestable AMI from a fresh Ubuntu 24.04 instance.
#
# Usage:
#   ./scripts/build-ami.sh \
#     --immutable-ubuntu   ./immutable-ubuntu \
#     --nitro-tpm-pcr-compute ./nitro-tpm-pcr-compute \
#     --key-name           my-ec2-keypair \
#     --key-file           ~/.ssh/my-ec2-keypair.pem \
#     [--region            us-east-1] \
#     [--volatile-dirs     var,etc]
#
# The script prints two IDs at the end:
#   Snapshot ID : snap-...   (EBS snapshot of the frozen disk image)
#   AMI ID      : ami-...    (attestable AMI with NitroTPM + UEFI)

set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------
IMMUTABLE_UBUNTU_PATH=""
NITRO_TPM_PATH=""
KEY_NAME=""
KEY_FILE=""
REGION=""
VOLATILE_DIRS=""

# Resource IDs — populated as the script progresses; used by the cleanup trap.
SG_ID=""
SOURCE_INSTANCE=""
BUILD_INSTANCE=""
ROOT_VOLUME=""
OUTPUT_VOLUME=""

# Local temp files
METADATA_LOCAL="/tmp/immutable-ubuntu-metadata-$$.yaml"
PCR_LOCAL="/tmp/immutable-ubuntu-image-$$.img.pcr.json"

# ---------------------------------------------------------------------------
# Cleanup trap
# ---------------------------------------------------------------------------
cleanup() {
    local exit_code=$?
    echo ""
    echo "--- cleanup ---"

    if [[ -n "$ROOT_VOLUME" ]]; then
        echo "Detaching and deleting root volume $ROOT_VOLUME..."
        aws ec2 detach-volume --volume-id "$ROOT_VOLUME" --region "$REGION" 2>/dev/null || true
        aws ec2 wait volume-available --volume-ids "$ROOT_VOLUME" --region "$REGION" 2>/dev/null || true
        aws ec2 delete-volume --volume-id "$ROOT_VOLUME" --region "$REGION" 2>/dev/null || true
    fi

    if [[ -n "$OUTPUT_VOLUME" ]]; then
        echo "Detaching and deleting output volume $OUTPUT_VOLUME..."
        aws ec2 detach-volume --volume-id "$OUTPUT_VOLUME" --region "$REGION" 2>/dev/null || true
        aws ec2 wait volume-available --volume-ids "$OUTPUT_VOLUME" --region "$REGION" 2>/dev/null || true
        aws ec2 delete-volume --volume-id "$OUTPUT_VOLUME" --region "$REGION" 2>/dev/null || true
    fi

    if [[ -n "$BUILD_INSTANCE" || -n "$SOURCE_INSTANCE" ]]; then
        local ids=""
        [[ -n "$SOURCE_INSTANCE" ]] && ids="$ids $SOURCE_INSTANCE"
        [[ -n "$BUILD_INSTANCE"  ]] && ids="$ids $BUILD_INSTANCE"
        echo "Terminating instances:$ids..."
        # shellcheck disable=SC2086
        aws ec2 terminate-instances --instance-ids $ids --region "$REGION" 2>/dev/null || true
        # shellcheck disable=SC2086
        aws ec2 wait instance-terminated --instance-ids $ids --region "$REGION" 2>/dev/null || true
    fi

    if [[ -n "$SG_ID" ]]; then
        echo "Deleting security group $SG_ID..."
        aws ec2 delete-security-group --group-id "$SG_ID" --region "$REGION" &>/dev/null || true
    fi

    #rm -f "$METADATA_LOCAL" "$PCR_LOCAL"

    if [[ $exit_code -ne 0 ]]; then
        echo "Script failed with exit code $exit_code."
    fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[$(date '+%H:%M:%S')] $*"; }

die() { echo "ERROR: $*" >&2; exit 1; }

usage() {
    cat <<EOF
Usage: $0 [OPTIONS]

Required:
  --immutable-ubuntu PATH         Local path to the immutable-ubuntu binary
  --nitro-tpm-pcr-compute PATH    Local path to the nitro-tpm-pcr-compute binary
  --key-name NAME                 EC2 key pair name (must already exist in your AWS account)
  --key-file PATH                 Local path to the matching private key (.pem)

Optional:
  --region REGION                 AWS region (default: aws configure get region)
  --volatile-dirs DIRS            Comma-separated dirs to make writable (e.g. var,etc)
  --help                          Show this help
EOF
    exit 0
}

wait_for_ssh() {
    local host="$1"
    local key="$2"
    local max_attempts=40
    local attempt=0

    log "Waiting for SSH on $host..."
    while (( attempt < max_attempts )); do
        if ssh -i "$key" \
               -o StrictHostKeyChecking=no \
               -o ConnectTimeout=5 \
               -o BatchMode=yes \
               "ubuntu@$host" "true" 2>/dev/null; then
            log "SSH ready on $host."
            return 0
        fi
        (( attempt++ ))
        sleep 10
    done
    die "Timed out waiting for SSH on $host."
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        --immutable-ubuntu)        IMMUTABLE_UBUNTU_PATH="$2"; shift 2 ;;
        --nitro-tpm-pcr-compute)   NITRO_TPM_PATH="$2";        shift 2 ;;
        --key-name)                KEY_NAME="$2";               shift 2 ;;
        --key-file)                KEY_FILE="$2";               shift 2 ;;
        --region)                  REGION="$2";                 shift 2 ;;
        --volatile-dirs)           VOLATILE_DIRS="$2";          shift 2 ;;
        --help|-h)                 usage ;;
        *) die "Unknown argument: $1. Run with --help for usage." ;;
    esac
done

# ---------------------------------------------------------------------------
# Validate inputs
# ---------------------------------------------------------------------------
[[ -n "$IMMUTABLE_UBUNTU_PATH" ]]  || die "--immutable-ubuntu is required."
[[ -n "$NITRO_TPM_PATH" ]]         || die "--nitro-tpm-pcr-compute is required."
[[ -n "$KEY_NAME" ]]               || die "--key-name is required."
[[ -n "$KEY_FILE" ]]               || die "--key-file is required."

[[ -f "$IMMUTABLE_UBUNTU_PATH" ]]  || die "File not found: $IMMUTABLE_UBUNTU_PATH"
[[ -f "$NITRO_TPM_PATH" ]]         || die "File not found: $NITRO_TPM_PATH"
[[ -f "$KEY_FILE" ]]               || die "Key file not found: $KEY_FILE"

if [[ -z "$REGION" ]]; then
    REGION=$(aws configure get region 2>/dev/null) \
        || die "Could not determine AWS region. Set --region or configure the AWS CLI."
    [[ -n "$REGION" ]] || die "AWS region is empty. Set --region."
fi

VOLATILE_DIRS_FLAG=""
[[ -n "$VOLATILE_DIRS" ]] && VOLATILE_DIRS_FLAG="--volatile-dirs $VOLATILE_DIRS"

SSH_OPTS=(-i "$KEY_FILE" -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=10)

log "Region          : $REGION"
log "immutable-ubuntu: $IMMUTABLE_UBUNTU_PATH"
log "nitro-tpm       : $NITRO_TPM_PATH"
log "Key pair        : $KEY_NAME"
[[ -n "$VOLATILE_DIRS" ]] && log "Volatile dirs   : $VOLATILE_DIRS"

# ---------------------------------------------------------------------------
# Step 1 — Resolve Ubuntu 24.04 AMI via SSM
# ---------------------------------------------------------------------------
log "Fetching Ubuntu 24.04 AMI from SSM..."
UBUNTU_AMI=$(aws ssm get-parameter \
    --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id \
    --region "$REGION" \
    --query 'Parameter.Value' \
    --output text)
log "Ubuntu 24.04 AMI: $UBUNTU_AMI"

# ---------------------------------------------------------------------------
# Step 2 — Default VPC + subnet + security group
# ---------------------------------------------------------------------------
log "Locating default VPC and subnet..."
DEFAULT_VPC=$(aws ec2 describe-vpcs \
    --filters Name=isDefault,Values=true \
    --region "$REGION" \
    --query 'Vpcs[0].VpcId' \
    --output text)
[[ "$DEFAULT_VPC" != "None" && -n "$DEFAULT_VPC" ]] \
    || die "No default VPC found in region $REGION."

SUBNET_JSON=$(aws ec2 describe-subnets \
    --filters "Name=vpc-id,Values=$DEFAULT_VPC" "Name=default-for-az,Values=true" \
    --region "$REGION" \
    --query 'Subnets[0].{SubnetId:SubnetId,AZ:AvailabilityZone}' \
    --output json)
DEFAULT_SUBNET=$(echo "$SUBNET_JSON" | grep -o '"SubnetId": *"[^"]*"' | awk -F'"' '{print $4}')
AZ=$(echo "$SUBNET_JSON" | grep -o '"AZ": *"[^"]*"' | awk -F'"' '{print $4}')
[[ -n "$DEFAULT_SUBNET" && -n "$AZ" ]] \
    || die "Could not find a default subnet in VPC $DEFAULT_VPC."
log "Default subnet  : $DEFAULT_SUBNET ($AZ)"

log "Detecting local public IP..."
MY_IP=$(curl -s --max-time 10 https://checkip.amazonaws.com) \
    || die "Could not determine public IP. Check your internet connection."
log "Local public IP : $MY_IP"

log "Creating temporary security group..."
SG_ID=$(aws ec2 create-security-group \
    --group-name "immutable-ubuntu-build-$$" \
    --description "Temporary SG for immutable-ubuntu AMI build (PID=$$)" \
    --vpc-id "$DEFAULT_VPC" \
    --region "$REGION" \
    --query 'GroupId' \
    --output text)
log "Security group  : $SG_ID"

aws ec2 authorize-security-group-ingress \
    --group-id "$SG_ID" \
    --protocol tcp \
    --port 22 \
    --cidr "${MY_IP}/32" \
    --region "$REGION" >/dev/null

# ---------------------------------------------------------------------------
# Step 3 — Launch source VM
# ---------------------------------------------------------------------------
log "Launching source VM (m5.large)..."
SOURCE_INSTANCE=$(aws ec2 run-instances \
    --image-id "$UBUNTU_AMI" \
    --instance-type m5.large \
    --key-name "$KEY_NAME" \
    --subnet-id "$DEFAULT_SUBNET" \
    --security-group-ids "$SG_ID" \
    --block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeType=gp3,Iops=4000,Throughput=1000}' \
    --region "$REGION" \
    --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=immutable-ubuntu-source}]' \
    --query 'Instances[0].InstanceId' \
    --output text)
log "Source instance : $SOURCE_INSTANCE"

log "Waiting for source VM to be running..."
aws ec2 wait instance-running --instance-ids "$SOURCE_INSTANCE" --region "$REGION"
SOURCE_IP=$(aws ec2 describe-instances \
    --instance-ids "$SOURCE_INSTANCE" \
    --region "$REGION" \
    --query 'Reservations[0].Instances[0].PublicIpAddress' \
    --output text)
log "Source VM IP    : $SOURCE_IP"

wait_for_ssh "$SOURCE_IP" "$KEY_FILE"

# ---------------------------------------------------------------------------
# Step 4 — Run immutable-ubuntu prepare on source VM
# ---------------------------------------------------------------------------
log "Uploading immutable-ubuntu to source VM..."
scp "${SSH_OPTS[@]}" "$IMMUTABLE_UBUNTU_PATH" "ubuntu@${SOURCE_IP}:/tmp/immutable-ubuntu"

log "Running 'immutable-ubuntu prepare' on source VM..."
ssh "${SSH_OPTS[@]}" "ubuntu@${SOURCE_IP}" \
    "chmod +x /tmp/immutable-ubuntu && sudo /tmp/immutable-ubuntu prepare"

log "Copying image-metadata.yaml from source VM..."
scp "${SSH_OPTS[@]}" "ubuntu@${SOURCE_IP}:/etc/immutable-ubuntu/image-metadata.yaml" \
    "$METADATA_LOCAL"

# ---------------------------------------------------------------------------
# Step 5 — Stop source VM and detach its root volume
# ---------------------------------------------------------------------------
log "Stopping source VM..."
aws ec2 stop-instances --instance-ids "$SOURCE_INSTANCE" --region "$REGION" >/dev/null
aws ec2 wait instance-stopped --instance-ids "$SOURCE_INSTANCE" --region "$REGION"
log "Source VM stopped."

log "Finding root EBS volume of source VM..."
# shellcheck disable=SC2016  # backticks inside the JMESPath query are not shell expansions
ROOT_VOLUME=$(aws ec2 describe-instances \
    --instance-ids "$SOURCE_INSTANCE" \
    --region "$REGION" \
    --query 'Reservations[0].Instances[0].BlockDeviceMappings[?DeviceName==`/dev/sda1`].Ebs.VolumeId' \
    --output text)
[[ -n "$ROOT_VOLUME" ]] || die "Could not find root volume of source instance."
log "Root volume     : $ROOT_VOLUME"

log "Detaching root volume from source VM..."
aws ec2 detach-volume --volume-id "$ROOT_VOLUME" --region "$REGION" >/dev/null
aws ec2 wait volume-available --volume-ids "$ROOT_VOLUME" --region "$REGION"
log "Root volume detached."

log "Terminating source VM..."
aws ec2 terminate-instances --instance-ids "$SOURCE_INSTANCE" --region "$REGION" >/dev/null
SOURCE_INSTANCE=""

# ---------------------------------------------------------------------------
# Step 6 — Launch build host (boot BEFORE attaching any extra volumes)
# ---------------------------------------------------------------------------
log "Launching build host (m5.large, same AZ: $AZ)..."
BUILD_INSTANCE=$(aws ec2 run-instances \
    --image-id "$UBUNTU_AMI" \
    --instance-type m5.large \
    --key-name "$KEY_NAME" \
    --subnet-id "$DEFAULT_SUBNET" \
    --security-group-ids "$SG_ID" \
    --placement "AvailabilityZone=$AZ" \
    --region "$REGION" \
    --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=immutable-ubuntu-builder}]' \
    --query 'Instances[0].InstanceId' \
    --output text)
log "Build host      : $BUILD_INSTANCE"

log "Waiting for build host to be running..."
aws ec2 wait instance-running --instance-ids "$BUILD_INSTANCE" --region "$REGION"
BUILD_IP=$(aws ec2 describe-instances \
    --instance-ids "$BUILD_INSTANCE" \
    --region "$REGION" \
    --query 'Reservations[0].Instances[0].PublicIpAddress' \
    --output text)
log "Build host IP   : $BUILD_IP"

# Wait for SSH to be ready — this ensures the instance has fully booted before
# we attach any volumes (booting from the wrong disk is prevented this way).
wait_for_ssh "$BUILD_IP" "$KEY_FILE"

log "Installing build dependencies on build host..."
ssh "${SSH_OPTS[@]}" "ubuntu@${BUILD_IP}" \
    "sudo apt-get update -qq && sudo apt-get install -y -qq gdisk cryptsetup-bin systemd-ukify systemd-boot-efi dosfstools"

# ---------------------------------------------------------------------------
# Step 7 — Attach source (root) volume and create + attach blank output volume
# ---------------------------------------------------------------------------
log "Attaching root volume to build host as /dev/sdf..."
aws ec2 attach-volume \
    --volume-id "$ROOT_VOLUME" \
    --instance-id "$BUILD_INSTANCE" \
    --device /dev/sdf \
    --region "$REGION" >/dev/null

log "Querying source volume size..."
SOURCE_SIZE_GIB=$(aws ec2 describe-volumes \
    --volume-ids "$ROOT_VOLUME" \
    --region "$REGION" \
    --query 'Volumes[0].Size' \
    --output text)
OUTPUT_SIZE_GIB=$(( SOURCE_SIZE_GIB + 2 ))   # +2 GiB headroom for ESP and verity
log "Output volume size: ${OUTPUT_SIZE_GIB} GiB (source ${SOURCE_SIZE_GIB} GiB + 2 GiB)"

log "Creating blank output volume (${OUTPUT_SIZE_GIB} GiB)..."
OUTPUT_VOLUME=$(aws ec2 create-volume \
    --size "$OUTPUT_SIZE_GIB" \
    --availability-zone "$AZ" \
    --volume-type gp3 \
    --iops 4000 \
    --throughput 1000 \
    --region "$REGION" \
    --tag-specifications 'ResourceType=volume,Tags=[{Key=Name,Value=immutable-ubuntu-output}]' \
    --query 'VolumeId' \
    --output text)
log "Output volume   : $OUTPUT_VOLUME"

aws ec2 wait volume-available --volume-ids "$OUTPUT_VOLUME" --region "$REGION"

log "Attaching output volume to build host as /dev/sdg..."
aws ec2 attach-volume \
    --volume-id "$OUTPUT_VOLUME" \
    --instance-id "$BUILD_INSTANCE" \
    --device /dev/sdg \
    --region "$REGION" >/dev/null

# Wait for udev to publish the source volume's PARTUUID symlinks (needed by freeze).
log "Waiting for udev to populate /dev/disk/by-partuuid/ on build host..."
for attempt in $(seq 1 30); do
    COUNT=$(ssh "${SSH_OPTS[@]}" "ubuntu@${BUILD_IP}" \
        "ls /dev/disk/by-partuuid/ 2>/dev/null | wc -l" 2>/dev/null || echo 0)
    if (( COUNT > 0 )); then
        log "udev ready ($COUNT PARTUUID symlinks found)."
        break
    fi
    if (( attempt == 30 )); then
        die "Timed out waiting for PARTUUID symlinks on build host."
    fi
    sleep 5
done

# Resolve the output volume's block device path via /dev/disk/by-id/.
# On NVMe instances (m5.large) EBS volumes appear as:
#   /dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_<vol-id-without-dashes>
OUTPUT_VOLUME_CLEAN="${OUTPUT_VOLUME//-/}"   # vol-0abc... -> vol0abc...
OUTPUT_BY_ID="/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_${OUTPUT_VOLUME_CLEAN}"

log "Waiting for output volume device to appear on build host..."
for attempt in $(seq 1 30); do
    # shellcheck disable=SC2029  # OUTPUT_BY_ID is intentionally expanded client-side
    if ssh "${SSH_OPTS[@]}" "ubuntu@${BUILD_IP}" \
           "test -L '${OUTPUT_BY_ID}'" 2>/dev/null; then
        break
    fi
    if (( attempt == 30 )); then
        die "Timed out waiting for output volume device on build host."
    fi
    sleep 5
done

# shellcheck disable=SC2029  # OUTPUT_BY_ID is intentionally expanded client-side
OUTPUT_DEV=$(ssh "${SSH_OPTS[@]}" "ubuntu@${BUILD_IP}" "readlink -f '${OUTPUT_BY_ID}'")
log "Output device   : $OUTPUT_DEV"

# ---------------------------------------------------------------------------
# Step 8 — Upload binaries and run freeze on build host
# ---------------------------------------------------------------------------
log "Uploading binaries and metadata to build host..."
scp "${SSH_OPTS[@]}" "$IMMUTABLE_UBUNTU_PATH" "ubuntu@${BUILD_IP}:/tmp/immutable-ubuntu"
scp "${SSH_OPTS[@]}" "$NITRO_TPM_PATH"        "ubuntu@${BUILD_IP}:/tmp/nitro-tpm-pcr-compute"
scp "${SSH_OPTS[@]}" "$METADATA_LOCAL"         "ubuntu@${BUILD_IP}:/tmp/image-metadata.yaml"

log "Running 'immutable-ubuntu freeze' on build host (output: $OUTPUT_DEV)..."
# shellcheck disable=SC2029
ssh "${SSH_OPTS[@]}" "ubuntu@${BUILD_IP}" \
    "chmod +x /tmp/immutable-ubuntu /tmp/nitro-tpm-pcr-compute
     sudo PATH=/tmp:\$PATH /tmp/immutable-ubuntu freeze \
       --config /tmp/image-metadata.yaml \
       --output ${OUTPUT_DEV} ${VOLATILE_DIRS_FLAG}"

# ---------------------------------------------------------------------------
# Step 9 — Copy PCR measurements back and detach volumes
# ---------------------------------------------------------------------------
log "Copying PCR measurements from build host (if present)..."
# freeze writes PCR output to <output-path>.pcr.json
scp "${SSH_OPTS[@]}" "ubuntu@${BUILD_IP}:${OUTPUT_DEV}.pcr.json" "$PCR_LOCAL" 2>/dev/null \
    || log "No PCR measurements file found — skipping."

log "Detaching root volume and output volume from build host..."
aws ec2 detach-volume --volume-id "$ROOT_VOLUME"  --region "$REGION" >/dev/null
aws ec2 detach-volume --volume-id "$OUTPUT_VOLUME" --region "$REGION" >/dev/null
aws ec2 wait volume-available --volume-ids "$ROOT_VOLUME"  --region "$REGION"
aws ec2 wait volume-available --volume-ids "$OUTPUT_VOLUME" --region "$REGION"

log "Deleting root volume..."
aws ec2 delete-volume --volume-id "$ROOT_VOLUME" --region "$REGION" >/dev/null
ROOT_VOLUME=""  # prevent cleanup trap from double-deleting

log "Terminating build host..."
aws ec2 terminate-instances --instance-ids "$BUILD_INSTANCE" --region "$REGION" >/dev/null
BUILD_INSTANCE=""

# ---------------------------------------------------------------------------
# Step 10 — Snapshot the output volume directly
# ---------------------------------------------------------------------------
TIMESTAMP=$(date +%Y%m%d-%H%M%S)

log "Creating snapshot from output volume..."
SNAPSHOT_ID=$(aws ec2 create-snapshot \
    --volume-id "$OUTPUT_VOLUME" \
    --description "immutable-ubuntu ${TIMESTAMP}" \
    --region "$REGION" \
    --tag-specifications "ResourceType=snapshot,Tags=[{Key=Name,Value=immutable-ubuntu-${TIMESTAMP}}]" \
    --query 'SnapshotId' \
    --output text)
log "Snapshot        : $SNAPSHOT_ID"

log "Waiting for snapshot to complete (this may take a few minutes)..."
aws ec2 wait snapshot-completed --snapshot-ids "$SNAPSHOT_ID" --region "$REGION"
log "Snapshot completed."

log "Deleting output volume..."
aws ec2 delete-volume --volume-id "$OUTPUT_VOLUME" --region "$REGION" >/dev/null
OUTPUT_VOLUME=""  # prevent cleanup trap from double-deleting

# ---------------------------------------------------------------------------
# Step 11 — Register AMI
# ---------------------------------------------------------------------------
log "Registering AMI with NitroTPM and UEFI boot mode..."
AMI_ID=$(aws ec2 register-image \
    --name "immutable-ubuntu-${TIMESTAMP}" \
    --description "Immutable Ubuntu with dm-verity and NitroTPM attestation (${TIMESTAMP})" \
    --architecture x86_64 \
    --boot-mode uefi \
    --tpm-support v2.0 \
    --ena-support \
    --root-device-name /dev/sda1 \
    --block-device-mappings "DeviceName=/dev/sda1,Ebs={SnapshotId=${SNAPSHOT_ID},VolumeType=gp3}" \
    --region "$REGION" \
    --query 'ImageId' \
    --output text)
log "AMI registered  : $AMI_ID"

# ---------------------------------------------------------------------------
# Step 12 — Delete temp security group
# ---------------------------------------------------------------------------
log "Deleting temporary security group $SG_ID..."
aws ec2 delete-security-group --group-id "$SG_ID" --region "$REGION" &>/dev/null || true
SG_ID=""

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
echo ""
echo "========================================="
echo " Build complete"
echo "========================================="
echo " Snapshot ID : $SNAPSHOT_ID"
echo " AMI ID      : $AMI_ID"
[[ -f "$PCR_LOCAL" ]] && echo " PCR file    : $PCR_LOCAL"
echo "========================================="
