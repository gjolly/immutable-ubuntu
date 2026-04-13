package image

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Partition describes one partition to be written into the output image.
type Partition struct {
	Label    string // GPT partition name: "ESP", "boot", "rootfs", "verity"
	TypeCode string // sgdisk hex type code: "ef00" (EFI System), "8300" (Linux)
	Source   string // path to raw partition image file or block device
}

// Assemble creates a new GPT disk image at output containing the given partitions in order.
// output may be a regular file path (created fresh) or a block device (written in place).
// On any error involving a regular file, the partial output is removed before returning.
func Assemble(output string, partitions []Partition) error {
	// Detect whether output is a block device so we can adjust behaviour below.
	isBlock := false
	if info, err := os.Stat(output); err == nil {
		isBlock = info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0
	}

	success := false
	defer func() {
		if !success && !isBlock {
			os.Remove(output)
		}
	}()

	// Compute partition sizes; handle both regular files and block devices.
	sizeMiBs := make([]int64, len(partitions))
	totalMiB := int64(2) // 2 MiB for GPT header + backup table
	for i, p := range partitions {
		sz, err := deviceSize(p.Source)
		if err != nil {
			return fmt.Errorf("size of partition %q source: %w", p.Label, err)
		}
		mib := (sz + (1<<20 - 1)) >> 20
		sizeMiBs[i] = mib
		totalMiB += mib
	}

	if isBlock {
		// Validate that the block device is large enough; truncate is not applicable.
		devBytes, err := deviceSize(output)
		if err != nil {
			return fmt.Errorf("get block device size: %w", err)
		}
		if needed := totalMiB << 20; devBytes < needed {
			return fmt.Errorf("block device %s too small: need %d MiB, have %d MiB",
				output, totalMiB, devBytes>>20)
		}
	} else {
		// Create sparse output file sized to hold all partitions.
		if err := runCmd("truncate", fmt.Sprintf("--size=%dM", totalMiB), output); err != nil {
			return fmt.Errorf("create image file: %w", err)
		}
	}

	// Initialise GPT.
	if err := runCmd("sgdisk", "--clear", output); err != nil {
		return fmt.Errorf("create GPT table: %w", err)
	}

	// Add partitions.
	for i, p := range partitions {
		n := strconv.Itoa(i + 1)
		arg := fmt.Sprintf("--new=%s:0:+%dM", n, sizeMiBs[i])
		tc := fmt.Sprintf("--typecode=%s:%s", n, p.TypeCode)
		name := fmt.Sprintf("--change-name=%s:%s", n, p.Label)
		if err := runCmd("sgdisk", arg, tc, name, output); err != nil {
			return fmt.Errorf("add partition %d (%s): %w", i+1, p.Label, err)
		}
	}

	// Write partition data at the correct sector offset.
	for i, p := range partitions {
		n := strconv.Itoa(i + 1)
		out, err := runCmdOutput("sgdisk", fmt.Sprintf("--info=%s", n), output)
		if err != nil {
			return fmt.Errorf("get offset of partition %d (%s): %w", i+1, p.Label, err)
		}
		sector, err := parseSgdiskFirstSector(out)
		if err != nil {
			return fmt.Errorf("parse offset of partition %d (%s): %w", i+1, p.Label, err)
		}
		if err := runCmd("dd",
			"if="+p.Source,
			"of="+output,
			"bs=4M",
			fmt.Sprintf("seek=%d", sector*512),
			"oflag=seek_bytes",
			"conv=notrunc",
			"iflag=fullblock",
		); err != nil {
			return fmt.Errorf("write partition %d (%s) data: %w", i+1, p.Label, err)
		}
	}

	success = true
	return nil
}

// GetPartitionGUID returns the unique GUID of partition partNum (1-based) in image.
func GetPartitionGUID(image string, partNum int) (string, error) {
	out, err := runCmdOutput("sgdisk", fmt.Sprintf("--info=%d", partNum), image)
	if err != nil {
		return "", fmt.Errorf("sgdisk --info=%d: %w", partNum, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Partition unique GUID:") {
			continue
		}
		// "Partition unique GUID: 550E8400-E29B-41D4-A716-446655440000"
		fields := strings.Fields(line)
		if len(fields) < 4 {
			return "", fmt.Errorf("unexpected sgdisk --info line: %q", line)
		}
		return strings.ToLower(fields[3]), nil
	}
	return "", fmt.Errorf("partition unique GUID not found for partition %d", partNum)
}

// deviceSize returns the byte size of path, which may be a regular file or a block device.
func deviceSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0 {
		out, err := runCmdOutput("blockdev", "--getsize64", path)
		if err != nil {
			return 0, fmt.Errorf("blockdev --getsize64: %w", err)
		}
		return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	}
	return info.Size(), nil
}

// parseSgdiskFirstSector extracts the first sector number from `sgdisk --info` output.
func parseSgdiskFirstSector(output []byte) (int64, error) {
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "First sector:") {
			continue
		}
		// "First sector: 2048 (at 1.0 MiB)"
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return 0, fmt.Errorf("unexpected sgdisk --info line: %q", line)
		}
		return strconv.ParseInt(fields[2], 10, 64)
	}
	return 0, fmt.Errorf("\"First sector:\" line not found in sgdisk --info output")
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return out, nil
}
