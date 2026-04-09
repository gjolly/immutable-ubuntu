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
  --output /path/to/output.img
```

## High-Level Overview

`prepare` is a command that prepares the system for creating an immutable image. It performs the following steps:
 * Cleanup the system by removing unnecessary files and packages:
   * Run `apt clean` to remove cached package files.
   * Remove log files from `/var/log` to free up space.
   * Set an empty /etc/machine-id to avoid issues with duplicate machine IDs when the image is cloned.
 * Install the necessary tools for creating an immutable image, such as `veritysetup`.
 * Configure `/etc/fstab` to mount the root filesystem with dm-verity as read-only,
 * Add an initramfs hook to parse the kernel command line and mount the root filesystem with dm-verity. This hook will be included in the initramfs when the image is created.
 * Regenerate the initramfs to include the new hook (with `initramfs-tools`).
 * Collect image metadata and write it to `/etc/immutable-ubuntu/image-metadata.yaml`:
   * Read the kernel command line from `/proc/cmdline`.
   * Detect the PARTUUID for the root filesystem, EFI system partition, and boot partition (if present) by parsing `/etc/fstab`.
   * Record whether a dedicated `/boot` partition exists.

On `freeze`, the tool performs the following steps:
 * Creates a dm-verity hash tree for the root filesystem.
 * Creates a new image with the root filesystem, EFI system partition, and boot partition and verity data partition.
 * Generate a UKI (Unified Kernel Image) that includes:
   * The kernel present on the boot partition (or in `/boot` on the root filesystem).
   * The initramfs present on the boot partition (or in `/boot` on the root filesystem).
   * The kernel command line from `image-metadata.yaml`, with the verity root hash and root filesystem UUID appended, along with `systemd.volatile=overlay`.
 * Move the UKI in `/EFI/BOOT/BOOTX64.EFI` on the EFI system partition.

## Notes

 * `/run` and `/tmp` will be mounted as tmpfs by systemd.
 * `/var` will be managed via `systemd.volatile=overlay`: systemd sets up an overlayfs with the read-only `/var` from the rootfs as the lower layer, so existing content (e.g. `/var/lib/dpkg`) remains visible while writes go to a tmpfs upper layer.
 * The `machine-id` file will be automatically bind-mounted from `/run` by systemd.
 * The rootfs passed to `freeze` must be an offline snapshot of the system on which `prepare` was run, since `freeze` relies on the initramfs hook and `/etc/immutable-ubuntu/cmdline` written by `prepare`.

