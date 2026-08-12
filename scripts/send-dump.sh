#!/usr/bin/env bash
# Copy a large dump to a remote host over several concurrent SSH streams.
#
# Why not just rsync/scp: the VPS and payments sit ~250 ms apart, and a single
# TCP stream over that RTT tops out around 130 KB/s regardless of the available
# bandwidth — the window drains faster than acknowledgements return. Measured on
# this pair, one stream needed roughly three hours for a 1.5 GB dump. Running N
# streams multiplies the in-flight window by N, and the cutover has to pay this
# cost a second time with the worker stopped, so the wall-clock matters.
#
# Each stream writes its own byte range straight into the destination file with
# dd seek=, so there are no part files to reassemble and nothing but the final
# artefact ever touches either disk. That also makes a re-run idempotent: rerun
# after a failure and the ranges are simply rewritten in place.
#
# Usage:
#   scripts/send-dump.sh <local-file> <user@host> <remote-path> [streams]
set -euo pipefail

src="${1:?usage: send-dump.sh <local-file> <user@host> <remote-path> [streams]}"
host="${2:?missing user@host}"
dst="${3:?missing remote-path}"
streams="${4:-8}"

[ -f "$src" ] || { echo "!! no such file: $src" >&2; exit 1; }

size="$(stat -c%s "$src" 2>/dev/null || stat -f%z "$src")"
bs=$((1024 * 1024))
total_blocks=$(( (size + bs - 1) / bs ))
per=$(( (total_blocks + streams - 1) / streams ))

echo ">> $src -> $host:$dst"
echo ">> ${size} bytes, ${total_blocks} MiB blocks, ${streams} streams x ${per} blocks"

# Preallocate remotely. Without this, N writers race to create the same file and
# the losers can truncate what the winner has already written.
ssh "$host" "mkdir -p \"\$(dirname '$dst')\" && : > '$dst' && truncate -s $size '$dst'"

pids=()
for i in $(seq 0 $((streams - 1))); do
  skip=$(( i * per ))
  [ "$skip" -lt "$total_blocks" ] || break
  (
    dd if="$src" bs="$bs" skip="$skip" count="$per" status=none \
      | ssh -o Compression=no "$host" \
          "dd of='$dst' bs=$bs seek=$skip conv=notrunc status=none"
  ) &
  pids+=($!)
done

failed=0
for p in "${pids[@]}"; do
  wait "$p" || failed=1
done
[ "$failed" -eq 0 ] || { echo "!! at least one stream failed; re-run to repair" >&2; exit 1; }

# Byte-for-byte proof. A dump that arrives subtly wrong restores without
# complaint and corrupts the migration silently, so this check is not optional.
echo ">> verifying sha256 (reads the whole file on both ends)"
local_sum="$(sha256sum "$src" | awk '{print $1}')"
remote_sum="$(ssh "$host" "shasum -a 256 '$dst' 2>/dev/null || sha256sum '$dst'" | awk '{print $1}')"

if [ "$local_sum" != "$remote_sum" ]; then
  echo "!! checksum mismatch" >&2
  echo "   local:  $local_sum" >&2
  echo "   remote: $remote_sum" >&2
  exit 1
fi

echo ">> ok: $local_sum"
