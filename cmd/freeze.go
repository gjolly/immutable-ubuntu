package cmd

import (
	"fmt"
	"os"
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
	freezeConfig      string
	freezeOutput      string
	freezeVolatileDirs string
)

func init() {
	freezeCmd.Flags().StringVar(&freezeConfig, "config", "", "path to image-metadata.yaml (required)")
	freezeCmd.Flags().StringVar(&freezeOutput, "output", "", "path for output image file (required)")
	freezeCmd.Flags().StringVar(&freezeVolatileDirs, "volatile-dirs", "", "comma-separated list of directories to make writable via tmpfs overlay (e.g. \"var,etc\")")
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

	// Compute dm-verity hash directly from the rootfs block device.
	verityImg := filepath.Join(tmpDir, "verity.img")
	fmt.Println("Computing dm-verity hash...")
	result, err := verity.ComputeHash(rootDev, verityImg)
	if err != nil {
		return fmt.Errorf("freeze: verity: %w", err)
	}
	fmt.Printf("Root hash: %s\n", result.RootHash)

	// Assemble the GPT image from block devices and the verity hash file.
	partitions := []image.Partition{
		{Label: "ESP", TypeCode: "ef00", Source: espDev},
	}
	if m.HasBootPartition {
		partitions = append(partitions, image.Partition{Label: "boot", TypeCode: "8300", Source: bootDev})
	}
	partitions = append(partitions,
		image.Partition{Label: "rootfs", TypeCode: "8300", Source: rootDev},
		image.Partition{Label: "verity", TypeCode: "8300", Source: verityImg},
	)

	fmt.Printf("Assembling output image at %s...\n", freezeOutput)
	if err := image.Assemble(freezeOutput, partitions); err != nil {
		return fmt.Errorf("freeze: image: %w", err)
	}

	// Read the PARTUUID that sgdisk assigned to the verity partition (always last).
	verityHashNum := len(partitions)
	fmt.Println("Querying verity partition GUID...")
	verityHashUUID, err := image.GetPartitionGUID(freezeOutput, verityHashNum)
	if err != nil {
		return fmt.Errorf("freeze: get verity PARTUUID: %w", err)
	}
	fmt.Printf("Verity hash device PARTUUID: %s\n", verityHashUUID)

	verityDataNum := len(partitions) - 1
	fmt.Println("Querying verity partition GUID...")
	verityDataUUID, err := image.GetPartitionGUID(freezeOutput, verityDataNum)
	if err != nil {
		return fmt.Errorf("freeze: get verity PARTUUID: %w", err)
	}
	fmt.Printf("Verity PARTUUID: %s\n", verityDataUUID)

	// Parse --volatile-dirs into a slice, dropping empty entries.
	var volatileDirs []string
	for _, d := range strings.Split(freezeVolatileDirs, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			volatileDirs = append(volatileDirs, d)
		}
	}

	// Build final cmdline with verity args.
	cmdline := metadata.AppendVerity(m, result.RootHash, verityHashUUID, verityDataUUID, volatileDirs)

	// Find kernel and initramfs from the boot or rootfs block device.
	fmt.Println("Locating kernel and initramfs...")
	bootPartSrc := rootDev
	if m.HasBootPartition {
		bootPartSrc = bootDev
	}
	kernel, initrd, err := uki.FindKernel(bootPartSrc, tmpDir)
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

	// Install UKI into the ESP of the assembled disk image.
	fmt.Println("Installing UKI into ESP...")
	if err := uki.InstallToDisk(ukiPath, freezeOutput); err != nil {
		return fmt.Errorf("freeze: uki install: %w", err)
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
