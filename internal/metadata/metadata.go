package metadata

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const metadataPath = "etc/immutable-ubuntu/image-metadata.yaml"

// ImageMetadata holds the partition and cmdline information collected from the live system.
type ImageMetadata struct {
	Cmdline          string `yaml:"cmdline"`
	RootPARTUUID     string `yaml:"root_partuuid"`
	ESPPartUUID      string `yaml:"esp_partuuid"`
	BootPARTUUID     string `yaml:"boot_partuuid,omitempty"`
	HasBootPartition bool   `yaml:"has_boot_partition"`
}

// Collect reads /proc/cmdline and uses lsblk to populate an ImageMetadata.
func Collect(rootfs string) (ImageMetadata, error) {
	var m ImageMetadata

	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return m, fmt.Errorf("read cmdline: %w", err)
	}
	m.Cmdline = stripCmdlineArgs(strings.TrimSpace(string(cmdline)), "BOOT_IMAGE", "root", "ro", "rw")

	partUUIDs, err := lsblkPARTUUIDs(rootfs)
	if err != nil {
		return m, fmt.Errorf("lsblk: %w", err)
	}

	m.RootPARTUUID = partUUIDs["/"]
	m.ESPPartUUID = partUUIDs["/boot/efi"]
	m.BootPARTUUID = partUUIDs["/boot"]
	m.HasBootPartition = m.BootPARTUUID != ""

	if m.RootPARTUUID == "" {
		return m, fmt.Errorf("root (/) PARTUUID not found via lsblk")
	}
	if m.ESPPartUUID == "" {
		return m, fmt.Errorf("ESP (/boot/efi) PARTUUID not found via lsblk")
	}

	return m, nil
}

// Save writes the metadata to /etc/immutable-ubuntu/image-metadata.yaml inside rootfs.
func Save(rootfs string, m ImageMetadata) error {
	dir := filepath.Join(rootfs, "etc", "immutable-ubuntu")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create metadata dir: %w", err)
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	dest := filepath.Join(rootfs, metadataPath)
	if err := os.WriteFile(dest, data, 0644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	return nil
}

// Load reads a metadata file from the given path.
func Load(path string) (ImageMetadata, error) {
	var m ImageMetadata
	data, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("read metadata file: %w", err)
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse metadata file: %w", err)
	}
	return m, nil
}

// AppendVerity builds the final kernel cmdline by appending dm-verity arguments.
// verityPartUUID is the PARTUUID of the verity hash partition in the assembled image.
// volatileDirs, if non-empty, adds an immutable-ubuntu.overlay=<dirs> parameter that
// instructs the initramfs to mount per-directory writable overlays over those paths.
func AppendVerity(m ImageMetadata, roothash, verityHashDevUUID, verityDataDevUUID string, volatileDirs []string) string {
	extra := fmt.Sprintf(
		// systemd.verity=no prevents systemd-veritysetup-generator from creating
		// a systemd-veritysetup@root.service unit that tries to set up dm-verity
		// in userspace. Without this, systemd waits ~90s for the verity partitions
		// (by PARTUUID) to appear as block devices, which never happens because
		// the initramfs has already assembled dm-verity before systemd starts.
		// We can't use systemd's native verity support because Ubuntu prior to
		// 26.04 ships with initramfs-tools by default, not a dracut/systemd-based
		// initramfs.
		"root=/dev/mapper/root roothash=%s root_hash_dev=PARTUUID=%s root_data_dev=PARTUUID=%s systemd.verity=no ro",
		roothash, verityHashDevUUID, verityDataDevUUID,
	)
	if len(volatileDirs) > 0 {
		extra += " immutable-ubuntu.overlay=" + strings.Join(volatileDirs, ",")
	}
	return strings.TrimSpace(m.Cmdline) + " " + extra
}

// stripCmdlineArgs removes all arguments whose key matches any of the given keys
// from a space-separated kernel command line. Both bare keys ("key") and key=value
// pairs ("key=value") are removed.
func stripCmdlineArgs(cmdline string, keys ...string) string {
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	var kept []string
	for _, arg := range strings.Fields(cmdline) {
		key := arg
		if i := strings.IndexByte(arg, '='); i >= 0 {
			key = arg[:i]
		}
		if !drop[key] {
			kept = append(kept, arg)
		}
	}
	return strings.Join(kept, " ")
}

// lsblkDevice mirrors the relevant fields from lsblk's JSON output.
type lsblkDevice struct {
	Mountpoints []interface{} `json:"mountpoints"`
	PARTUUID    *string       `json:"partuuid"`
	Children    []lsblkDevice `json:"children"`
}

type lsblkOutput struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

// lsblkPARTUUIDs returns a map of canonical mountpoint → PARTUUID by walking lsblk output.
// Mountpoints are normalised relative to rootfs (e.g. "/mnt/foo" → "/" when rootfs="/mnt/foo").
func lsblkPARTUUIDs(rootfs string) (map[string]string, error) {
	out, err := exec.Command("lsblk", "-J", "-o", "NAME,MOUNTPOINTS,PARTUUID").Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}

	var data lsblkOutput
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("parse lsblk output: %w", err)
	}

	// Normalise rootfs: strip trailing slash, but keep "/" as-is.
	root := filepath.Clean(rootfs)

	result := make(map[string]string)
	walkLsblk(data.BlockDevices, root, result)
	return result, nil
}

// walkLsblk recursively visits lsblk devices, recording PARTUUID for known mountpoints.
func walkLsblk(devs []lsblkDevice, rootfs string, out map[string]string) {
	for _, d := range devs {
		if d.PARTUUID != nil && *d.PARTUUID != "" {
			for _, mp := range d.Mountpoints {
				s, ok := mp.(string)
				if !ok {
					continue
				}
				canonical := toCanonical(s, rootfs)
				if canonical != "" {
					out[canonical] = *d.PARTUUID
				}
			}
		}
		if len(d.Children) > 0 {
			walkLsblk(d.Children, rootfs, out)
		}
	}
}

// toCanonical converts a host mountpoint to a canonical path relative to rootfs.
// Returns "" if the mountpoint is not under rootfs.
func toCanonical(hostMount, rootfs string) string {
	if rootfs == "/" {
		switch hostMount {
		case "/", "/boot/efi", "/boot":
			return hostMount
		}
		return ""
	}
	// rootfs is an absolute prefix like "/mnt/ubuntu"
	switch hostMount {
	case rootfs:
		return "/"
	case rootfs + "/boot/efi":
		return "/boot/efi"
	case rootfs + "/boot":
		return "/boot"
	}
	return ""
}
