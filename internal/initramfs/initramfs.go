package initramfs

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed hooks scripts
var assets embed.FS

// InstallHook copies the embedded hook and premount scripts into the rootfs.
func InstallHook(rootfs string) error {
	if err := installEmbedded(rootfs, "hooks", filepath.Join("etc", "initramfs-tools", "hooks"), 0755); err != nil {
		return fmt.Errorf("install hook: %w", err)
	}
	if err := installEmbedded(rootfs, "scripts/local-premount", filepath.Join("etc", "initramfs-tools", "scripts", "local-premount"), 0755); err != nil {
		return fmt.Errorf("install premount script: %w", err)
	}
	if err := installEmbedded(rootfs, "scripts/local-bottom", filepath.Join("etc", "initramfs-tools", "scripts", "local-bottom"), 0755); err != nil {
		return fmt.Errorf("install local-bottom script: %w", err)
	}
	return nil
}

// Regenerate runs update-initramfs inside the rootfs to rebuild the initramfs.
func Regenerate(rootfs string) error {
	cmd := exec.Command("chroot", rootfs, "update-initramfs", "-u", "-k", "all")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("update-initramfs: %w\n%s", err, out)
	}
	return nil
}

// installEmbedded copies all files from the embedded srcDir into destDir under rootfs.
func installEmbedded(rootfs, srcDir, destDir string, perm fs.FileMode) error {
	absDestDir := filepath.Join(rootfs, destDir)
	if err := os.MkdirAll(absDestDir, 0755); err != nil {
		return err
	}

	entries, err := assets.ReadDir(srcDir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := assets.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", e.Name(), err)
		}
		dest := filepath.Join(absDestDir, e.Name())
		if err := os.WriteFile(dest, data, perm); err != nil {
			return fmt.Errorf("write %s: %w", strings.TrimPrefix(dest, rootfs), err)
		}
	}
	return nil
}
