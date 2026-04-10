# AWS Attestable AMIs

[AWS attestable AMIs](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/attestable-ami.html)
allow EC2 instances to cryptographically prove their integrity via NitroTPM. The image produced
by `immutable-ubuntu` is well-suited for attestation: the UKI embeds the dm-verity root hash in
the kernel command line, so any modification to the rootfs changes PCR12 and invalidates the
attestation.

This document walks through the complete workflow, from a running EC2 instance to a registered
attestable AMI.

## Prerequisites

- An EC2 instance running Ubuntu (the "source VM") with `immutable-ubuntu` installed
- A second EC2 instance (the "build host") with `immutable-ubuntu` and `nitro-tpm-pcr-compute`
  installed along with `systemd-boot-efi` and `systemd-ukify` (for UKI generation)
- AWS CLI configured with permissions to: `ec2:CreateSnapshot`, `ec2:ImportSnapshot`,
  `ec2:RegisterImage`, `ec2:DescribeImportSnapshotTasks`, `s3:PutObject`
- An S3 bucket for staging the disk image

## Step 1 — Prepare the source VM

On the source EC2 instance, run `prepare` to configure the system for immutable boot:

```bash
sudo immutable-ubuntu prepare
```

This command:
- Cleans up cached packages and log files
- Sets an empty `/etc/machine-id`
- Configures `/etc/fstab` for a read-only dm-verity root
- Installs and regenerates the initramfs with the verity mount hook
- Writes `/etc/immutable-ubuntu/image-metadata.yaml` with partition UUIDs and the kernel
  command line

Before stopping the instance, copy the metadata file to the build host — it will be needed
by `freeze`:

```bash
scp /etc/immutable-ubuntu/image-metadata.yaml <build-host>:/tmp/image-metadata.yaml
```

Then **stop the instance** — do not reboot it. The next step requires the root volume to be
offline.

```bash
aws ec2 stop-instances --instance-ids <source-instance-id>
aws ec2 wait instance-stopped --instance-ids <source-instance-id>
```

## Step 2 — Snapshot the root EBS volume

Find the root volume of the stopped source instance and create a snapshot:

```bash
ROOT_VOLUME=$(aws ec2 describe-instances \
  --instance-ids <source-instance-id> \
  --query 'Reservations[0].Instances[0].BlockDeviceMappings[?DeviceName==`/dev/sda1`].Ebs.VolumeId' \
  --output text)

SNAPSHOT_ID=$(aws ec2 create-snapshot \
  --volume-id "$ROOT_VOLUME" \
  --description "immutable-ubuntu source snapshot" \
  --query 'SnapshotId' --output text)

aws ec2 wait snapshot-completed --snapshot-ids "$SNAPSHOT_ID"
echo "Snapshot ready: $SNAPSHOT_ID"
```

## Step 3 — Attach the snapshot to the build host

Create a volume from the snapshot and attach it to the build host:

```bash
AZ=$(aws ec2 describe-instances \
  --instance-ids <build-host-instance-id> \
  --query 'Reservations[0].Instances[0].Placement.AvailabilityZone' \
  --output text)

VOLUME_ID=$(aws ec2 create-volume \
  --snapshot-id "$SNAPSHOT_ID" \
  --availability-zone "$AZ" \
  --query 'VolumeId' --output text)

aws ec2 wait volume-available --volume-ids "$VOLUME_ID"

aws ec2 attach-volume \
  --volume-id "$VOLUME_ID" \
  --instance-id <build-host-instance-id> \
  --device /dev/sdf
```

## Step 4 — Run freeze

On the build host, run `freeze` against the attached volume using the metadata file copied
in step 1:

```bash
sudo immutable-ubuntu freeze \
  --config /tmp/image-metadata.yaml \
  --output /tmp/immutable.img
```

`immutable-ubuntu freeze` automatically calls `nitro-tpm-pcr-compute` when it is found in
`PATH`. When `freeze` completes you will have:

| File | Description |
|---|---|
| `/tmp/immutable.img` | GPT disk image with ESP, rootfs, and verity partitions |
| `/tmp/immutable.img.pcr.json` | TPM PCR reference measurements |

To make selected directories writable at runtime (e.g. `/var` and `/etc`), add
`--volatile-dirs var,etc`.

## Step 5 — Upload the image to S3

```bash
BUCKET=my-ami-staging-bucket
KEY=immutable-ubuntu/immutable.img

aws s3 cp /tmp/immutable.img "s3://${BUCKET}/${KEY}"
```

## Step 6 — Import the image as an EBS snapshot

```bash
IMPORT_TASK=$(aws ec2 import-snapshot \
  --description "immutable-ubuntu" \
  --disk-container "Format=RAW,UserBucket={S3Bucket=${BUCKET},S3Key=${KEY}}" \
  --query 'ImportTaskId' --output text)

echo "Import task: $IMPORT_TASK"

# Poll until complete (can take several minutes)
while true; do
  STATUS=$(aws ec2 describe-import-snapshot-tasks \
    --import-task-ids "$IMPORT_TASK" \
    --query 'ImportSnapshotTasks[0].SnapshotTaskDetail.Status' \
    --output text)
  echo "Status: $STATUS"
  [ "$STATUS" = "completed" ] && break
  sleep 30
done

AMI_SNAPSHOT=$(aws ec2 describe-import-snapshot-tasks \
  --import-task-ids "$IMPORT_TASK" \
  --query 'ImportSnapshotTasks[0].SnapshotTaskDetail.SnapshotId' \
  --output text)

echo "Snapshot ID: $AMI_SNAPSHOT"
```

## Step 7 — Register the AMI

Register the snapshot as an AMI with **NitroTPM** and **UEFI boot mode** enabled — both are
required for attestation:

```bash
AMI_ID=$(aws ec2 register-image \
  --name "immutable-ubuntu-$(date +%Y%m%d-%H%M%S)" \
  --description "Immutable Ubuntu with dm-verity and NitroTPM attestation" \
  --architecture x86_64 \
  --boot-mode uefi \
  --tpm-support v2.0 \
  --ena-support \
  --root-device-name /dev/sda1 \
  --block-device-mappings "DeviceName=/dev/sda1,Ebs={SnapshotId=${AMI_SNAPSHOT},VolumeType=gp3}" \
  --query 'ImageId' --output text)

echo "AMI registered: $AMI_ID"
```

The AMI is now ready to launch. Instances booted from it will have NitroTPM measurements tied
to the exact UKI (and therefore the exact dm-verity root hash) baked into the image.

## Step 8 — Store the PCR reference measurements

The `.pcr.json` file produced by `freeze` contains the expected PCR values for instances
launched from this AMI. Store it alongside the AMI metadata so you can use it later for
attestation policy enforcement:

```bash
aws s3 cp /tmp/immutable.img.pcr.json \
  "s3://${BUCKET}/immutable-ubuntu/${AMI_ID}.pcr.json"
```

Example contents:

```json
{
  "Measurements": {
    "HashAlgorithm": "SHA384 { ... }",
    "PCR4": "<UKI binary hash>",
    "PCR7": "<UEFI Secure Boot policy>",
    "PCR12": "000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
  }
}
```

**PCR4 is the value that matters for immutable-ubuntu.** It is the hash of the UKI binary
itself. Because `freeze` embeds the dm-verity root hash directly into the UKI's kernel command
line section, any modification to the rootfs produces a different verity root hash, a different
UKI binary, and therefore a different PCR4 — invalidating the attestation.

**PCR12 will always be zero** in this setup. It measures the command line passed *externally*
to the UEFI boot binary at runtime. Since the command line is fully embedded inside the UKI,
there is no external command line to measure.

See the [AWS documentation on PCR compute](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/create-pcr-compute.html)
for details on how to build attestation policies around these values.

## Step 9 — Verify the attestation document

> **Note:** `nitro-tpm-attest` currently fails on burstable instance types (e.g. `t3.micro`)
> due to a hardcoded NV buffer size that exceeds the TPM limit on those instances. Use a
> non-burstable instance type such as `m5.large` or `c5.large` for attestation. See
> [aws/NitroTPM-Tools#7](https://github.com/aws/NitroTPM-Tools/issues/7) for details.

After retrieving an attestation document from a running instance with `nitro-tpm-attest`, decode
it and compare the PCR values against the reference measurements stored in S3.

The attestation document is CBOR+COSE encoded. Use the following Python script to extract the
PCR values:

```python
# /// script
# dependencies = ["cbor2"]
# ///
import cbor2, sys, json

with open(sys.argv[1], "rb") as f:
    data = f.read()

# Decode COSE_Sign1 outer wrapper, attestation document is the 3rd element
doc = cbor2.loads(cbor2.loads(data)[2])
actual_pcrs = {idx: val.hex() for idx, val in doc["nitrotpm_pcrs"].items()}

with open(sys.argv[2], "r") as f:
    reference = json.load(f)

# Reference keys are like "PCR4", values are hex strings
reference_pcrs = {
    int(k[3:]): v
    for k, v in reference["Measurements"].items()
    if k.startswith("PCR")
}

ok = True
for idx, expected in sorted(reference_pcrs.items()):
    actual = actual_pcrs.get(idx, "<missing>")
    match = actual == expected
    status = "OK" if match else "MISMATCH"
    print(f"PCR{idx}: {status}")
    if not match:
        print(f"  expected: {expected}")
        print(f"  actual:   {actual}")
        ok = False

sys.exit(0 if ok else 1)
```

Fetch the reference measurements and run the verification with [uv](https://docs.astral.sh/uv/):

```bash
aws s3 cp "s3://${BUCKET}/immutable-ubuntu/${AMI_ID}.pcr.json" /tmp/reference.pcr.json
uv run decode-attestation.py attestation.bin /tmp/reference.pcr.json
```

**PCR4 must match** the value in the reference file. Any difference means the UKI (and therefore
the dm-verity root hash) has changed since the image was built.

## Cleanup

After a successful AMI registration you can release the temporary resources:

```bash
# Detach and delete the volume created from the source snapshot
aws ec2 detach-volume --volume-id "$VOLUME_ID"
aws ec2 wait volume-available --volume-ids "$VOLUME_ID"
aws ec2 delete-volume --volume-id "$VOLUME_ID"

# Delete the staging S3 object
aws s3 rm "s3://${BUCKET}/${KEY}"
```

The source EBS snapshot (`$SNAPSHOT_ID`) can be kept as a record or deleted once you have
confirmed the AMI works correctly.
