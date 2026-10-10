#!/usr/bin/env python3
"""fat_lba.py IMAGE PATH OFFSET - prints the 512-byte LBA (from the start of
IMAGE) holding byte OFFSET of PATH (e.g. EFI/ALPINE/KERNEL) on the image's
FAT32 ESP: an MBR partition of type 0x2E (alpine-zfsboot's msdos layout) or
the GPT EFI System Partition (installed disks, the ISO). For the fault-
injection boots (DISK_FI_LBA=...), which must damage one exact block."""
import struct
import sys

ESP_GUID = bytes([0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11, 0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B])


def esp_start(img):
    img.seek(512)
    hdr = img.read(512)
    if hdr[:8] == b"EFI PART":
        entries_lba, n, size = struct.unpack_from("<QII", hdr, 72)
        img.seek(entries_lba * 512)
        table = img.read(n * size)
        for i in range(n):
            e = table[i * size:(i + 1) * size]
            if e[:16] == ESP_GUID:
                return struct.unpack_from("<Q", e, 32)[0]
    img.seek(0)
    mbr = img.read(512)
    for i in range(4):
        e = mbr[446 + 16 * i:446 + 16 * (i + 1)]
        if e[4] == 0x2E:
            return struct.unpack_from("<I", e, 8)[0]
    sys.exit("no ESP found")


def main():
    path, name, offset = sys.argv[1], sys.argv[2], int(sys.argv[3], 0)
    with open(path, "rb") as img:
        part = esp_start(img)
        img.seek(part * 512)
        bs = img.read(512)
        bps, spc, reserved, nfats = struct.unpack_from("<HBHB", bs, 11)
        assert bps == 512, bps
        fatsz, = struct.unpack_from("<I", bs, 36)
        root, = struct.unpack_from("<I", bs, 44)
        fat_lba = part + reserved
        data_lba = fat_lba + nfats * fatsz

        def nxt(c):
            img.seek(fat_lba * 512 + c * 4)
            return struct.unpack("<I", img.read(4))[0] & 0x0FFFFFFF

        def chain(c):
            out = []
            while 2 <= c < 0x0FFFFFF8:
                out.append(c)
                c = nxt(c)
            return out

        def read_dir(c):
            data = b""
            for cl in chain(c):
                img.seek((data_lba + (cl - 2) * spc) * 512)
                data += img.read(spc * 512)
            return data

        cluster, size = root, None
        for comp in name.split("/"):
            want = comp.upper().ljust(11).encode()
            d = read_dir(cluster)
            for i in range(0, len(d), 32):
                e = d[i:i + 32]
                if e[0] == 0:
                    break
                if e[11] & 0x08 or e[0] == 0xE5:  # LFN entries and the volume label
                    continue
                if e[:11] == want:
                    cluster = (struct.unpack_from("<H", e, 20)[0] << 16) | struct.unpack_from("<H", e, 26)[0]
                    size = struct.unpack_from("<I", e, 28)[0]
                    break
            else:
                sys.exit(f"{comp} not found")
        assert offset < size, (offset, size)
        cl = chain(cluster)[offset // (spc * 512)]
        print(data_lba + (cl - 2) * spc + (offset % (spc * 512)) // 512)


main()
