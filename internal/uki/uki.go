package uki

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// UKIConfig holds the inputs needed to build a Unified Kernel Image.
type UKIConfig struct {
	Kernel    string // path to vmlinuz
	Initramfs string // path to initrd.img
	Cmdline   string // final kernel command line
	Output    string // destination path for the generated .efi file
}

// Build runs ukify to produce a UKI from the given config.
func Build(cfg UKIConfig) error {
	if err := runCmd("ukify", "build",
		"--linux="+cfg.Kernel,
		"--initrd="+cfg.Initramfs,
		"--cmdline="+cfg.Cmdline,
		"--output="+cfg.Output,
	); err != nil {
		return fmt.Errorf("ukify build: %w", err)
	}
	return nil
}

// InstallToDisk mounts the ESP of diskImage, places the UKI at EFI/BOOT/BOOTX64.EFI,
// then unmounts.
//
// For regular image files the ESP is reached via a loop device. For block devices
// the kernel partition table is refreshed with partprobe and the partition node is
// used directly, avoiding a needless extra layer of indirection.
//
// In both cases the ESP is located by its partition type GUID rather than by number.
func InstallToDisk(ukiPath, diskImage string) error {
	info, err := os.Stat(diskImage)
	if err != nil {
		return fmt.Errorf("stat disk image: %w", err)
	}
	isBlock := info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0

	var topDev string
	if isBlock {
		// Refresh the kernel's in-memory partition table after sgdisk/dd have written
		// to the device, then wait for udev to create the partition device nodes.
		if err := runCmd("partprobe", diskImage); err != nil {
			return fmt.Errorf("refresh partition table: %w", err)
		}
		topDev = diskImage
	} else {
		out, err := runCmdOutput("losetup", "--find", "--partscan", "--show", diskImage)
		if err != nil {
			return fmt.Errorf("attach disk image: %w", err)
		}
		topDev = strings.TrimSpace(string(out))
		defer func() { _ = runCmd("losetup", "-d", topDev) }()
	}

	// Wait for udev to create partition device nodes regardless of path taken above.
	_ = runCmd("udevadm", "settle", "--timeout=5")

	espDev, err := findESP(topDev)
	if err != nil {
		return err
	}

	mountDir, err := os.MkdirTemp("", "immutable-ubuntu-esp-*")
	if err != nil {
		return fmt.Errorf("create ESP mount dir: %w", err)
	}
	defer os.RemoveAll(mountDir)

	if err := runCmd("mount", espDev, mountDir); err != nil {
		return fmt.Errorf("mount ESP: %w", err)
	}
	defer func() { _ = runCmd("umount", mountDir) }()

	efiDir := filepath.Join(mountDir, "EFI", "BOOT")
	if err := os.MkdirAll(efiDir, 0755); err != nil {
		return fmt.Errorf("create EFI/BOOT directory: %w", err)
	}

	dest := filepath.Join(efiDir, "BOOTX64.EFI")
	if err := runCmd("cp", ukiPath, dest); err != nil {
		return fmt.Errorf("copy UKI: %w", err)
	}

	return nil
}

// findESP returns the block device path of the EFI System Partition on device by
// scanning partition types with lsblk.
func findESP(device string) (string, error) {
	const espTypeGUID = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"

	out, err := runCmdOutput("lsblk", "--noheadings", "--raw", "--output", "NAME,PARTTYPE", device)
	if err != nil {
		return "", fmt.Errorf("lsblk %s: %w", device, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.EqualFold(fields[1], espTypeGUID) {
			return "/dev/" + fields[0], nil
		}
	}
	return "", fmt.Errorf("no EFI System Partition found on %s", device)
}

// FindKernel mounts partitionImage read-only, locates vmlinuz and initrd.img, copies them
// to destDir, then unmounts. Returns the paths of the copied files.
func FindKernel(partitionImage, destDir string) (kernel, initramfs string, err error) {
	mountDir, err := os.MkdirTemp("", "immutable-ubuntu-boot-*")
	if err != nil {
		return "", "", fmt.Errorf("create boot mount dir: %w", err)
	}
	defer os.RemoveAll(mountDir)

	mountOpts, err := mountOptions(partitionImage)
	if err != nil {
		return "", "", fmt.Errorf("stat partition source: %w", err)
	}
	if err := runCmd("mount", "-o", mountOpts, partitionImage, mountDir); err != nil {
		return "", "", fmt.Errorf("mount partition image: %w", err)
	}
	defer func() { _ = runCmd("umount", mountDir) }()

	// Look for kernel and initramfs under mountDir directly (boot partition)
	// or under mountDir/boot (rootfs partition).
	kernelSrc, err := findFile(mountDir, "vmlinuz")
	if err != nil {
		return "", "", fmt.Errorf("find kernel: %w", err)
	}
	initrdSrc, err := findFile(mountDir, "initrd.img")
	if err != nil {
		return "", "", fmt.Errorf("find initramfs: %w", err)
	}

	kernelDest := filepath.Join(destDir, "vmlinuz")
	if err := runCmd("cp", kernelSrc, kernelDest); err != nil {
		return "", "", fmt.Errorf("copy kernel: %w", err)
	}
	initrdDest := filepath.Join(destDir, "initrd.img")
	if err := runCmd("cp", initrdSrc, initrdDest); err != nil {
		return "", "", fmt.Errorf("copy initramfs: %w", err)
	}

	return kernelDest, initrdDest, nil
}

// findFile looks for a file named exactly `name` or `name-*` under dir and dir/boot.
// Prefers an exact match (e.g. the `vmlinuz` symlink on Ubuntu) over versioned names.
func findFile(dir, name string) (string, error) {
	for _, searchDir := range []string{dir, filepath.Join(dir, "boot")} {
		// Prefer exact name (symlink on Ubuntu pointing to current kernel).
		exact := filepath.Join(searchDir, name)
		if _, err := os.Lstat(exact); err == nil {
			return exact, nil
		}
		// Fall back to versioned names, take the last (highest version).
		matches, err := filepath.Glob(filepath.Join(searchDir, name+"-*"))
		if err != nil {
			return "", err
		}
		if len(matches) > 0 {
			return matches[len(matches)-1], nil
		}
	}
	return "", fmt.Errorf("no %s found under %s", name, dir)
}

// runCmd executes a command and returns an error that includes captured output on failure.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return nil
}

// runCmdOutput executes a command and returns stdout. On failure the error includes stderr.
func runCmdOutput(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return out, nil
}

// mountOptions returns "-o ro" for block devices and "-o loop,ro" for image files.
func mountOptions(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0 {
		return "ro", nil
	}
	return "loop,ro", nil
}
