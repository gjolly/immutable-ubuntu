package image

import (
	"testing"
)

func TestParseSgdiskFirstSector(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{
			name: "typical output",
			input: `Partition GUID code: EBD0A0A2-B9E5-4433-87C0-68B6B72699C7 (Microsoft basic data)
Partition unique GUID: AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE
First sector: 2048 (at 1.0 MiB)
Last sector: 1050623 (at 513.0 MiB)
Partition size: 1048576 sectors (512.0 MiB)
Attribute flags: 0000000000000000
Partition name: 'ESP'`,
			want: 2048,
		},
		{
			name: "large sector number",
			input: `First sector: 4196352 (at 2.0 GiB)
Last sector: 8390655 (at 4.0 GiB)`,
			want: 4196352,
		},
		{
			name:    "missing first sector line",
			input:   "Partition name: 'rootfs'\nLast sector: 999\n",
			wantErr: true,
		},
		{
			name:    "malformed line too few fields",
			input:   "First sector:\n",
			wantErr: true,
		},
		{
			name:    "malformed non-numeric sector",
			input:   "First sector: abc (at 1.0 MiB)\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSgdiskFirstSector([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSgdiskFirstSector() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseSgdiskFirstSector() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSizeMiBRounding(t *testing.T) {
	tests := []struct {
		bytes   int64
		wantMiB int64
	}{
		{0, 0},
		{1, 1},
		{1 << 20, 1},           // exactly 1 MiB
		{(1 << 20) + 1, 2},     // 1 MiB + 1 byte → rounds up to 2 MiB
		{(1 << 20) - 1, 1},     // just under 1 MiB → 1 MiB
		{32 * (1 << 20), 32},   // 32 MiB exact
		{33*(1<<20) + 512, 34}, // 33 MiB + 512 bytes → 34 MiB
	}

	for _, tt := range tests {
		got := (tt.bytes + (1<<20 - 1)) >> 20
		if got != tt.wantMiB {
			t.Errorf("round(%d bytes) = %d MiB, want %d MiB", tt.bytes, got, tt.wantMiB)
		}
	}
}
