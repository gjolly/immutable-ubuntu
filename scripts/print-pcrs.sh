#!/usr/bin/env bash
# Print TPM PCR4 and PCR7 values (SHA-384) by reading directly from sysfs.
set -euo pipefail

TPM_DIR="/sys/class/tpm/tpm0"

if [[ ! -d "$TPM_DIR" ]]; then
    echo "error: no TPM found at $TPM_DIR" >&2
    exit 1
fi

PCR_DIR="$TPM_DIR/pcr-sha384"
if [[ ! -d "$PCR_DIR" ]]; then
    echo "error: sha384 PCR bank not found at $PCR_DIR" >&2
    exit 1
fi

for idx in 4 7 12; do
    pcr_file="$PCR_DIR/$idx"
    if [[ -r "$pcr_file" ]]; then
        value=$(tr '[:upper:]' '[:lower:]' < "$pcr_file")
        echo "PCR${idx} (sha384): ${value}"
    else
        echo "error: PCR${idx} not readable at $pcr_file" >&2
        exit 1
    fi
done
