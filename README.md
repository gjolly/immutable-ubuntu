# Immutable Ubuntu

Build an immutable Ubuntu image with a dm-verity rootfs.

## Usage

Before creating the image, customize the VM as you like and run:

```bash
immutable-ubuntu prepare
```

To create an immutable Ubuntu image from an existing image that is mounted, run the following command:

```bash
immutable-ubuntu freeze \
  --config /path/to/image-metadata.yaml \
  --output /path/to/output.img \
  [--volatile-dirs var,etc]
```

## High-Level Overview

`prepare` is a command that prepares the system for creating an immutable image. It performs the following steps:
 * Cleanup the system by removing unnecessary files and packages:
   * Run `apt clean` to remove cached package files.
   * Remove log files from `/var/log` to free up space.
   * Set an empty /etc/machine-id to avoid issues with duplicate machine IDs when the image is cloned.
 * Configure `/etc/fstab` to mount the root filesystem with dm-verity as read-only,
 * Add an initramfs hook to parse the kernel command line and mount the root filesystem with dm-verity. This hook will be included in the initramfs when the image is created.
 * Regenerate the initramfs to include the new hook (with `initramfs-tools`).
 * Collect image metadata and write it to `/etc/immutable-ubuntu/image-metadata.yaml`:
   * Read the kernel command line from `/proc/cmdline`.
   * Detect the PARTUUID for the root filesystem, EFI system partition, and boot partition (if present) using `lsblk`.
   * Record whether a dedicated `/boot` partition exists.

On `freeze`, the tool performs the following steps:
 * Creates a dm-verity hash tree for the root filesystem.
 * Creates a new image with the root filesystem, EFI system partition, and boot partition and verity data partition.
 * Generate a UKI (Unified Kernel Image) that includes:
   * The kernel present on the boot partition (or in `/boot` on the root filesystem).
   * The initramfs present on the boot partition (or in `/boot` on the root filesystem).
   * The kernel command line from `image-metadata.yaml`, with the verity root hash and root filesystem UUID appended, and optionally `immutable-ubuntu.overlay=<dirs>` if `--volatile-dirs` is specified.
 * Move the UKI in `/EFI/BOOT/BOOTX64.EFI` on the EFI system partition.

## Notes

 * `/run` and `/tmp` will be mounted as tmpfs by systemd.
 * Writable overlays are opt-in: pass `--volatile-dirs var,etc` (or any comma-separated list) to `freeze` to make those directories writable at runtime. The initramfs `local-bottom` script sets up a tmpfs-backed overlayfs for each listed directory before `switch_root`, leaving the rest of the verity root truly read-only.
 * The `machine-id` file will be automatically bind-mounted from `/run` by systemd.
 * The rootfs passed to `freeze` must be an offline snapshot of the system on which `prepare` was run, since `freeze` relies on the initramfs hook and `/etc/immutable-ubuntu/image-metadata.yaml` written by `prepare`.

## AWS Attestable AMIs

[AWS attestable AMIs](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/attestable-ami.html)
allow EC2 instances to cryptographically prove their integrity via NitroTPM. The image produced
by `immutable-ubuntu` is well-suited for attestation: the UKI embeds the dm-verity root hash in
the kernel command line, so any modification to the rootfs changes PCR12 and invalidates the
attestation.

### AMI registration requirements

When registering the output image as an AMI, enable:
 * **NitroTPM**
 * **UEFI boot mode**

### Generating PCR reference measurements

After `freeze`, the UKI at `EFI/BOOT/BOOTX64.EFI` on the ESP is used to compute reference
[TPM PCR measurements](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/create-pcr-compute.html)
using the `nitro-tpm-pcr-compute` utility (provided by the `aws-nitro-tpm-tools` package on
Amazon Linux 2023).

If `nitro-tpm-pcr-compute` is found in `PATH` when `freeze` runs, the measurements are
automatically written to `<output>.pcr.json`:

```json
{
  "Measurements": {
    "HashAlgorithm": "SHA384 { ... }",
    "PCR4": "PCR4_measurement",
    "PCR7": "PCR7_measurement",
    "PCR12": "PCR12_measurement"
  }
}
```

To run it manually after the fact:

```bash
LOOP=$(losetup --find --partscan --show /path/to/output.img)
mkdir -p /tmp/esp
mount "${LOOP}p1" /tmp/esp
nitro-tpm-pcr-compute --image /tmp/esp/EFI/BOOT/BOOTX64.EFI
umount /tmp/esp
losetup -d "$LOOP"
```

Store the reference measurements alongside your AMI; use them to validate that instances are
running the exact image you built.

