package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/gjolly/immutable-ubuntu/internal/initramfs"
	"github.com/gjolly/immutable-ubuntu/internal/metadata"
	"github.com/gjolly/immutable-ubuntu/internal/system"
	"github.com/spf13/cobra"
)

var prepareCmd = &cobra.Command{
	Use:   "prepare",
	Short: "Prepare the live system for immutable image creation",
	RunE:  runPrepare,
}

var rootfs string

func init() {
	prepareCmd.Flags().StringVar(&rootfs, "rootfs", "/", "path to the root filesystem to prepare")
	rootCmd.AddCommand(prepareCmd)
}

func runPrepare(cmd *cobra.Command, args []string) error {
	fmt.Println("Cleaning up system...")
	if err := system.Cleanup(rootfs); err != nil {
		return fmt.Errorf("prepare: cleanup: %w", err)
	}

	fmt.Println("Configuring /etc/fstab...")
	if err := system.ConfigureFstab(rootfs); err != nil {
		return fmt.Errorf("prepare: fstab: %w", err)
	}

	fmt.Println("Ensuring required packages are installed...")
	if err := system.EnsureDeps(rootfs); err != nil {
		return fmt.Errorf("prepare: ensure deps: %w", err)
	}

	fmt.Println("Installing initramfs hook...")
	if err := initramfs.InstallHook(rootfs); err != nil {
		return fmt.Errorf("prepare: initramfs hook: %w", err)
	}

	fmt.Println("Regenerating initramfs...")
	if err := initramfs.Regenerate(rootfs); err != nil {
		return fmt.Errorf("prepare: initramfs regenerate: %w", err)
	}

	fmt.Println("Collecting image metadata...")
	m, err := metadata.Collect(rootfs)
	if err != nil {
		return fmt.Errorf("prepare: metadata collect: %w", err)
	}

	if err := metadata.Save(rootfs, m); err != nil {
		return fmt.Errorf("prepare: metadata save: %w", err)
	}

	fmt.Printf("Metadata written to %s\n", filepath.Join(rootfs, "etc", "immutable-ubuntu", "image-metadata.yaml"))
	fmt.Println("prepare: done")
	return nil
}
