package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchFstabLine(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "root entry gets ro, x-systemd.verity, and pass=0",
			input: "UUID=abc123\t/\text4\tdefaults\t0\t1",
			want:  "UUID=abc123\t/\text4\tdefaults,ro,x-systemd.verity\t0\t0",
		},
		{
			name:  "root entry with existing ro gets x-systemd.verity and pass=0",
			input: "UUID=abc123\t/\text4\tro\t0\t1",
			want:  "UUID=abc123\t/\text4\tro,x-systemd.verity\t0\t0",
		},
		{
			name:  "root entry with existing x-systemd.verity gets ro and pass=0",
			input: "UUID=abc123\t/\text4\tx-systemd.verity\t0\t1",
			want:  "UUID=abc123\t/\text4\tx-systemd.verity,ro\t0\t0",
		},
		{
			name:  "non-root entry is unchanged",
			input: "UUID=def456\t/boot\tvfat\tdefaults\t0\t1",
			want:  "UUID=def456\t/boot\tvfat\tdefaults\t0\t1",
		},
		{
			name:  "comment line is unchanged",
			input: "# /etc/fstab",
			want:  "# /etc/fstab",
		},
		{
			name:  "blank line is unchanged",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := patchFstabLine(tt.input)
			if got != tt.want {
				t.Errorf("patchFstabLine(%q)\n got  %q\n want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestConfigureFstab_AddsTmpTmpfs(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "etc", "fstab")
	os.MkdirAll(filepath.Dir(fstab), 0755)

	content := "UUID=abc123\t/\text4\tdefaults\t0\t1\n"
	os.WriteFile(fstab, []byte(content), 0644)

	if err := ConfigureFstab(dir); err != nil {
		t.Fatalf("ConfigureFstab: %v", err)
	}

	data, _ := os.ReadFile(fstab)
	if !strings.Contains(string(data), "tmpfs\t/tmp\ttmpfs") {
		t.Error("expected /tmp tmpfs entry in fstab")
	}
}

func TestConfigureFstab_DoesNotDuplicateTmpTmpfs(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "etc", "fstab")
	os.MkdirAll(filepath.Dir(fstab), 0755)

	content := "UUID=abc123\t/\text4\tdefaults\t0\t1\ntmpfs\t/tmp\ttmpfs\tdefaults\t0\t0\n"
	os.WriteFile(fstab, []byte(content), 0644)

	if err := ConfigureFstab(dir); err != nil {
		t.Fatalf("ConfigureFstab: %v", err)
	}

	data, _ := os.ReadFile(fstab)
	if strings.Count(string(data), "/tmp") != 1 {
		t.Error("expected exactly one /tmp entry in fstab")
	}
}
