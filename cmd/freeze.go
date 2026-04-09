package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gjolly/immutable-ubuntu/internal/image"
	"github.com/gjolly/immutable-ubuntu/internal/metadata"
	"github.com/gjolly/immutable-ubuntu/internal/uki"
	"github.com/gjolly/immutable-ubuntu/internal/verity"
	"github.com/spf13/cobra"
)

var freezeCmd = &cobra.Command{
	Use:   "freeze",
	Short: "Assemble an immutable disk image from a prepared source disk",
	RunE:  runFreeze,
}

var (
	freezeConfig string
	freezeOutput string
)

func init() {
	freezeCmd.Flags().StringVar(&freezeConfig, "config", "", "path to image-metadata.yaml (required)")
	freezeCmd.Flags().StringVar(&freezeOutput, "output", "", "path for output image file (required)")
	_ = freezeCmd.MarkFlagRequired("config")
	_ = freezeCmd.MarkFlagRequired("output")
	rootCmd.AddCommand(freezeCmd)
}

func runFreeze(cmd *cobra.Command, args []string) error {
	if _, err := os.Stat(freezeOutput); err == nil {
		return fmt.Errorf("freeze: output file already exists: %s", freezeOutput)
	}

	fmt.Printf("Loading metadata from %s...\n", freezeConfig)
	m, err := metadata.Load(freezeConfig)
	if err != nil {
		return fmt.Errorf("freeze: load metadata: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "immutable-ubuntu-*")
	if err != nil {
		return fmt.Errorf("freeze: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Resolve PARTUUIDs to block device paths.
	espDev, err := partitionPath(m.ESPPartUUID)
	if err != nil {
		return fmt.Errorf("freeze: resolve ESP partition: %w", err)
	}
	rootDev, err := partitionPath(m.RootPARTUUID)
	if err != nil {
		return fmt.Errorf("freeze: resolve root partition: %w", err)
	}
	var bootDev string
	if m.HasBootPartition {
		bootDev, err = partitionPath(m.BootPARTUUID)
		if err != nil {
			return fmt.Errorf("freeze: resolve boot partition: %w", err)
		}
	}

	// Dump partitions to temp files.
	espImg := filepath.Join(tmpDir, "esp.img")
	fmt.Println("Dumping ESP partition...")
	if err := dumpPartition(espDev, espImg); err != nil {
		return fmt.Errorf("freeze: dump ESP: %w", err)
	}

	rootfsImg := filepath.Join(tmpDir, "rootfs.img")
	fmt.Println("Dumping rootfs partition...")
	if err := dumpPartition(rootDev, rootfsImg); err != nil {
		return fmt.Errorf("freeze: dump rootfs: %w", err)
	}

	var bootImg string
	if m.HasBootPartition {
		bootImg = filepath.Join(tmpDir, "boot.img")
		fmt.Println("Dumping boot partition...")
		if err := dumpPartition(bootDev, bootImg); err != nil {
			return fmt.Errorf("freeze: dump boot: %w", err)
		}
	}

	// Compute dm-verity hash.
	verityImg := filepath.Join(tmpDir, "verity.img")
	fmt.Println("Computing dm-verity hash...")
	result, err := verity.ComputeHash(rootfsImg, verityImg)
	if err != nil {
		return fmt.Errorf("freeze: verity: %w", err)
	}
	fmt.Printf("Root hash: %s\n", result.RootHash)

	// Build final cmdline with verity args.
	cmdline := metadata.AppendVerity(m, result.RootHash)

	// Find kernel and initramfs from the boot/rootfs partition image.
	fmt.Println("Locating kernel and initramfs...")
	bootPartImg := rootfsImg
	if m.HasBootPartition {
		bootPartImg = bootImg
	}
	kernel, initrd, err := uki.FindKernel(bootPartImg, tmpDir)
	if err != nil {
		return fmt.Errorf("freeze: find kernel: %w", err)
	}

	// Build UKI.
	ukiPath := filepath.Join(tmpDir, "boot.efi")
	fmt.Println("Building UKI...")
	if err := uki.Build(uki.UKIConfig{
		Kernel:    kernel,
		Initramfs: initrd,
		Cmdline:   cmdline,
		Output:    ukiPath,
	}); err != nil {
		return fmt.Errorf("freeze: uki build: %w", err)
	}

	// Install UKI into ESP partition image.
	fmt.Println("Installing UKI into ESP...")
	if err := uki.Install(ukiPath, espImg); err != nil {
		return fmt.Errorf("freeze: uki install: %w", err)
	}

	// Assemble final GPT image.
	partitions := []image.Partition{
		{Label: "ESP", TypeCode: "ef00", Source: espImg},
	}
	if m.HasBootPartition {
		partitions = append(partitions, image.Partition{Label: "boot", TypeCode: "8300", Source: bootImg})
	}
	partitions = append(partitions,
		image.Partition{Label: "rootfs", TypeCode: "8300", Source: rootfsImg},
		image.Partition{Label: "verity", TypeCode: "8300", Source: verityImg},
	)

	fmt.Printf("Assembling output image at %s...\n", freezeOutput)
	if err := image.Assemble(freezeOutput, partitions); err != nil {
		return fmt.Errorf("freeze: image: %w", err)
	}

	fmt.Println("freeze: done")
	return nil
}

// partitionPath resolves a PARTUUID to its block device path via /dev/disk/by-partuuid/.
func partitionPath(partuuid string) (string, error) {
	p := filepath.Join("/dev/disk/by-partuuid", strings.ToLower(partuuid))
	if _, err := os.Lstat(p); err != nil {
		return "", fmt.Errorf("PARTUUID=%s: %w", partuuid, err)
	}
	return p, nil
}

// dumpPartition copies a block device to a file using cp --sparse=always.
func dumpPartition(src, dest string) error {
	cmd := exec.Command("cp", "--sparse=always", src, dest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp --sparse=always %s %s: %w\n%s", src, dest, err, out)
	}
	return nil
}
