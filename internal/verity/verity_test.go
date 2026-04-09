package verity

import (
	"testing"
)

func TestParseRootHash(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name: "typical veritysetup output",
			input: `VERITY header information for verity.img
UUID:            	xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
Hash type:       	1
Data blocks:     	16384
Data block size: 	4096
Hash block size: 	4096
Hash algorithm:  	sha256
Salt:            	aabbccddeeff
Root hash:      	deadbeef1234abcd5678ef90deadbeef1234abcd5678ef90deadbeef1234abcd`,
			want: "deadbeef1234abcd5678ef90deadbeef1234abcd5678ef90deadbeef1234abcd",
		},
		{
			name:    "missing root hash line",
			input:   "Hash algorithm:  sha256\nSalt: aabb\n",
			wantErr: true,
		},
		{
			name:    "root hash line too short",
			input:   "Root hash:\n",
			wantErr: true,
		},
		{
			name: "extra whitespace around hash",
			input: `Hash algorithm:  sha256
Root hash:       abc123def456`,
			want: "abc123def456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRootHash([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseRootHash() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseRootHash() = %q, want %q", got, tt.want)
			}
		})
	}
}
