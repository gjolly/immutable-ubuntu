package verity

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// HashResult holds the output of a veritysetup format operation.
type HashResult struct {
	RootHash   string // hex root hash string
	HashDevice string // path to the generated hash image file
}

// ComputeHash runs veritysetup format on rootfsImage and writes the hash tree to hashImage.
// It returns the root hash and the path to the hash image.
func ComputeHash(rootfsImage, hashImage string) (HashResult, error) {
	out, err := runCmdOutput("veritysetup", "format", rootfsImage, hashImage)
	if err != nil {
		return HashResult{}, fmt.Errorf("veritysetup format: %w", err)
	}

	rootHash, err := parseRootHash(out)
	if err != nil {
		return HashResult{}, fmt.Errorf("veritysetup format: %w", err)
	}

	return HashResult{
		RootHash:   rootHash,
		HashDevice: hashImage,
	}, nil
}

// parseRootHash extracts the root hash from veritysetup format stdout.
func parseRootHash(output []byte) (string, error) {
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Root hash:") {
			continue
		}
		// "Root hash:      deadbeef1234..."
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return "", fmt.Errorf("unexpected veritysetup output line: %q", line)
		}
		return fields[2], nil
	}
	return "", fmt.Errorf("\"Root hash:\" line not found in veritysetup output")
}

// runCmdOutput executes a command and returns stdout. On failure the error includes stderr.
func runCmdOutput(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// veritysetup writes its header info to stdout, progress/errors to stderr
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return out, nil
}
