package system

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Cleanup removes cached packages, log files, and empties the machine-id.
func Cleanup(rootfs string) error {
	if err := runInChroot(rootfs, "apt-get", "clean"); err != nil {
		return fmt.Errorf("apt clean: %w", err)
	}

	logDir := filepath.Join(rootfs, "var", "log")
	if err := removeFilesUnder(logDir); err != nil {
		return fmt.Errorf("remove logs: %w", err)
	}

	machineID := filepath.Join(rootfs, "etc", "machine-id")
	if err := os.WriteFile(machineID, []byte(""), 0444); err != nil {
		return fmt.Errorf("clear machine-id: %w", err)
	}

	return nil
}

// ConfigureFstab rewrites the rootfs fstab entry to add ro and x-systemd.verity mount options.
func ConfigureFstab(rootfs string) error {
	fstabPath := filepath.Join(rootfs, "etc", "fstab")

	data, err := os.ReadFile(fstabPath)
	if err != nil {
		return fmt.Errorf("read fstab: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	updated := make([]string, 0, len(lines))

	for _, line := range lines {
		updated = append(updated, patchFstabLine(line))
	}

	out := strings.Join(updated, "\n")
	if err := os.WriteFile(fstabPath, []byte(out), 0644); err != nil {
		return fmt.Errorf("write fstab: %w", err)
	}

	return nil
}

// patchFstabLine adds ro and x-systemd.verity to the options of the root (/) mount entry.
func patchFstabLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return line
	}

	fields := splitFstabFields(line)
	if len(fields) < 4 {
		return line
	}

	mountpoint := fields[1]
	if mountpoint != "/" {
		return line
	}

	opts := addMountOption(fields[3], "ro")
	opts = addMountOption(opts, "x-systemd.verity")

	fields[3] = opts

	// Disable fsck for the verity root: the kernel verifies integrity via dm-verity,
	// and fsck cannot open a read-only mapped device anyway (pass=0 skips it).
	if len(fields) >= 6 {
		fields[5] = "0"
	}

	return strings.Join(fields, "\t")
}

// splitFstabFields splits an fstab line preserving leading whitespace in fields.
func splitFstabFields(line string) []string {
	scanner := bufio.NewScanner(strings.NewReader(line))
	scanner.Split(bufio.ScanWords)
	var fields []string
	for scanner.Scan() {
		fields = append(fields, scanner.Text())
	}
	return fields
}

// addMountOption appends opt to the comma-separated options string if not already present.
func addMountOption(opts, opt string) string {
	for _, o := range strings.Split(opts, ",") {
		if o == opt {
			return opts
		}
	}
	return opts + "," + opt
}

// runInChroot runs a command chrooted into rootfs, capturing stderr for error messages.
func runInChroot(rootfs string, name string, args ...string) error {
	cmdArgs := append([]string{rootfs, name}, args...)
	cmd := exec.Command("chroot", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("chroot %s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return nil
}

// removeFilesUnder recursively removes all files under dir but leaves the directory itself.
func removeFilesUnder(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
