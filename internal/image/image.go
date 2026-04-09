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
	Source   string // path to raw partition image file
}

// Assemble creates a new GPT disk image at output containing the given partitions in order.
// On any error, the partial output file is removed before returning.
func Assemble(output string, partitions []Partition) error {
	success := false
	defer func() {
		if !success {
			os.Remove(output)
		}
	}()

	// Compute partition sizes from source files.
	sizeMiBs := make([]int64, len(partitions))
	totalMiB := int64(2) // 2 MiB for GPT header + backup table
	for i, p := range partitions {
		info, err := os.Stat(p.Source)
		if err != nil {
			return fmt.Errorf("stat partition %q source: %w", p.Label, err)
		}
		mib := (info.Size() + (1<<20 - 1)) >> 20
		sizeMiBs[i] = mib
		totalMiB += mib
	}

	// Create sparse output file.
	if err := runCmd("truncate", fmt.Sprintf("--size=%dM", totalMiB), output); err != nil {
		return fmt.Errorf("create image file: %w", err)
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
			"bs=512",
			fmt.Sprintf("seek=%d", sector),
			"conv=notrunc",
			"iflag=fullblock",
		); err != nil {
			return fmt.Errorf("write partition %d (%s) data: %w", i+1, p.Label, err)
		}
	}

	success = true
	return nil
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
