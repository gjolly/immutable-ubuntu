# AGENT.md — immutable-ubuntu

## What this project does

`immutable-ubuntu` is a two-step CLI tool that converts a standard Ubuntu VM into an immutable, dm-verity-protected disk image:

1. **`prepare`** — runs inside the source VM. Cleans up the system, installs initramfs hooks, regenerates the initramfs, and writes partition metadata to `/etc/immutable-ubuntu/image-metadata.yaml`.
2. **`freeze`** — runs on the host (requires root). Reads the metadata file, computes a dm-verity hash of the rootfs block device, assembles a new GPT image with an appended verity partition, and builds a UKI (Unified Kernel Image) with the verity arguments and cmdline embedded. The result is a self-contained disk image that boots with a cryptographically verified read-only root.

At runtime, the frozen image boots via the UKI's embedded cmdline. The initramfs hook opens the dm-verity device, and optionally mounts per-directory writable overlays (backed by tmpfs) for directories like `/var` and `/etc`.

## Repository layout

```
cmd/
  root.go          # cobra root command
  prepare.go       # prepare subcommand
  freeze.go        # freeze subcommand — CLI flags, orchestration
internal/
  image/           # GPT image assembly (sgdisk + dd)
  initramfs/       # Hook and scripts embedded into the initramfs
    hooks/immutable-ubuntu          # Copies binaries/modules into initramfs
    scripts/local-premount/immutable-ubuntu   # Opens dm-verity device
    scripts/local-bottom/immutable-ubuntu     # Mounts per-dir volatile overlays
    initramfs.go   # Installs hook+scripts into a rootfs, runs update-initramfs
  metadata/        # ImageMetadata struct, Collect/Save/Load, AppendVerity
  system/          # Cleanup, fstab patching (chroot helpers)
  uki/             # ukify wrapper, kernel/initrd finder
  verity/          # veritysetup wrapper
main.go
integration-tests.sh   # End-to-end test using LXD VMs
```

## Build and test

```bash
# Build
go build ./...

# Unit tests (image and verity packages only — others require root/hardware)
go test ./...

# Integration test (requires LXD, root, and sgdisk/ukify/veritysetup on PATH)
sudo bash integration-tests.sh
```

There are no unit tests for `cmd/`, `initramfs/`, `metadata/`, `system/`, or `uki/` — these all shell out to system tools or require block devices.

## Key design decisions

**No systemd in the initrd.** Ubuntu 24.04 uses a busybox-based initramfs. `systemd.volatile=overlay` does nothing here because `systemd-volatile-root.service` only runs inside a systemd-based initrd. All verity setup and volatile overlay mounting is done by the project's own initramfs scripts.

**Volatile overlays are per-directory, not root-wide.** The `--volatile-dirs` flag on `freeze` controls which directories (e.g. `var,etc`) get a writable tmpfs overlay. This keeps `/usr`, `/bin`, `/lib` etc. truly read-only. The kernel cmdline parameter is `immutable-ubuntu.overlay=var,etc` — parsed by the `local-bottom` initramfs script.

**UKI with embedded cmdline.** The kernel cmdline is baked into the UKI at freeze time (not read from a bootloader config). This means the verity root hash and partition UUIDs are signed along with the kernel and initramfs.

**`prepare` must run on the exact system being frozen.** It reads `/proc/cmdline` and uses `lsblk` to discover partition UUIDs. The resulting `image-metadata.yaml` is consumed by `freeze` on the host.

## External tool dependencies

`prepare` requires (inside the target VM):
- `apt-get`, `chroot`, `update-initramfs`, `lsblk`
- `veritysetup`, `findfs` (copied into initramfs by the hook)

`freeze` requires (on the host, as root):
- `veritysetup` — dm-verity hash computation
- `sgdisk` — GPT assembly and GUID queries
- `dd` — raw partition writing
- `ukify` — UKI assembly
- `losetup`, `mount`, `umount` — kernel/initrd extraction from the source disk

## Adding new initramfs scripts

Scripts under `internal/initramfs/scripts/` are embedded via `//go:embed hooks scripts` in `initramfs.go`. Adding a new script type (e.g. `local-top`) requires:
1. Creating the directory and script file under `internal/initramfs/scripts/local-top/`
2. Adding a corresponding `installEmbedded(...)` call in `InstallHook` in `initramfs.go`

## Cmdline flow

```
/proc/cmdline (live system)
  → metadata.Collect()       strips BOOT_IMAGE=
  → image-metadata.yaml      stored as m.Cmdline
  → metadata.AppendVerity()  appends: root=/dev/mapper/root roothash=<hash>
                                       root_hash_dev=PARTUUID=<uuid>
                                       root_data_dev=PARTUUID=<uuid> ro
                                       [immutable-ubuntu.overlay=var,etc]
  → uki.Build()              bakes final cmdline into UKI binary
```
