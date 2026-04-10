# Immutable Ubuntu

`immutable-ubuntu` turns a running Ubuntu VM into a fully immutable, attestable disk image —
no custom build tooling or image pipeline required.

The workflow is simple: boot any Ubuntu VM, configure it exactly as you want using standard
tools (`apt`, config files, whatever), then run `immutable-ubuntu prepare` followed by
`immutable-ubuntu freeze`. The result is a GPT disk image where:

- The root filesystem is protected by **dm-verity**: any modification to the rootfs is
  detected at boot and causes the system to refuse to start.
- The kernel, initramfs, and kernel command line (which embeds the dm-verity root hash) are
  bundled into a **UKI (Unified Kernel Image)**. The UKI is the single artifact that ties
  the boot process to a specific, unmodified rootfs.
- When a TPM is present the UKI hash is measured into **TPM PCR4**, making the
  entire system cryptographically attestable: a remote party can verify that an
  instance is running exactly the image that was built, with no modification to
  the rootfs, kernel, or boot parameters.

This makes `immutable-ubuntu` well-suited for hardened workloads and environments that
require supply-chain integrity, such as AWS EC2 instances registered as
[attestable AMIs](docs/aws-attestable-ami.md).

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

See [docs/aws-attestable-ami.md](docs/aws-attestable-ami.md) for the complete end-to-end
workflow: from a running EC2 instance through `prepare`, `freeze`, S3 upload, snapshot import,
and AMI registration with NitroTPM and PCR reference measurements.
