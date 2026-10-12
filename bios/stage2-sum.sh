#!/bin/sh
# stage2-sum.sh FILE - fills in stage2.bin's header words (see stage2_entry.S):
# offset 4 must already hold the 0x5331 marker; offset 6 = image length in
# 16-bit words, offset 8 = the value that makes the 16-bit sum of all those
# words zero. stage1.S checks exactly that before it
# jumps into stage2. Pads FILE with one zero byte first if its size is odd.
# Plain POSIX sh + od/awk/dd/printf: no new build dependency.
set -eu
f="$1"

size=$(wc -c < "$f")
if [ $((size % 2)) -ne 0 ]; then
    printf '\000' >> "$f"
    size=$((size + 1))
fi
words=$((size / 2))
if [ "$words" -eq 0 ] || [ "$words" -gt 16384 ]; then
    echo "stage2-sum.sh: $f is $size bytes, outside 2..32768" >&2
    exit 1
fi

put16() { # put16 OFFSET VALUE - little-endian
    printf "\\$(printf '%03o' $(($2 & 0xff)))\\$(printf '%03o' $((($2 >> 8) & 0xff)))" |
        dd of="$f" bs=1 seek="$1" conv=notrunc 2>/dev/null
}

marker=$(od -An -tu1 -j 4 -N 2 "$f" | awk '{ print $1 + $2 * 256 }')
[ "$marker" -eq $((0x5331)) ] || { echo "stage2-sum.sh: $f has no 0x5331 marker at offset 4 (stage2_entry.S out of step?)" >&2; exit 1; }
put16 6 "$words"
put16 8 0
# Bytes, combined little-endian here, so the host's own byte order never matters.
sum16() {
    od -An -v -tu1 "$f" | awk '{ for (i = 1; i <= NF; i++) { s = (s + (n % 2 ? $i * 256 : $i)) % 65536; n++ } } END { print s + 0 }'
}
sum=$(sum16)
put16 8 $(((65536 - sum) % 65536))

check=$(sum16)
[ "$check" -eq 0 ] || { echo "stage2-sum.sh: checksum did not come out zero ($check)" >&2; exit 1; }
