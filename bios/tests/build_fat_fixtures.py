#!/usr/bin/env python3
"""
Hand-builds real, spec-correct FAT32 disk images for fat.c's own host
test harness (fat_host_test.c) - no mkfs.vfat/mtools dependency, since
neither is available in every environment this project's own CI/dev
sandboxes run in (confirmed missing in at least one real dev sandbox
this project has used). Every byte written here follows the FAT32
spec directly (BPB layout, FAT table encoding, 8.3 directory entries)
- this is a second, independent implementation of "how FAT32 is laid
out" from fat.c's own reading of it, which is exactly the point: a
bug shared between both would not be a real bug in fat.c, so building
the fixture a genuinely different way (write path in Python, read
path in C) is what makes this a real test rather than a round-trip
tautology.

Produces (in the directory given as argv[1]):
  valid.img            - EFI/ALPINE/{KERNEL,INITRD,CMDLINE}, KERNEL
                          deliberately fragmented (non-contiguous,
                          out-of-numeric-order clusters) to prove the
                          reader doesn't assume contiguity, CMDLINE
                          ending in a trailing newline (the reader
                          must strip it).
  bad_sig.img           - valid.img with the 0x55AA boot signature
                          corrupted.
  bad_sector_size.img    - bytes_per_sector set to 128 (invalid).
  bad_cluster_size.img   - sectors_per_cluster set to 3 (not a power
                          of two).
  fat16_disguise.img     - root_entry_count/fat_size_16 nonzero, as a
                          real FAT16 volume would have - must be
                          rejected outright, not misread as FAT32.
  truncated_chain.img     - KERNEL's FAT chain hits EOC before
                          covering its own declared file_size.
  cyclic_chain.img        - KERNEL's FAT chain loops back on itself.
"""
import struct
import sys
import os

SECTOR = 512
RESERVED_SECTORS = 32
NUM_FATS = 2
ROOT_CLUSTER = 2
FAT_SIZE_SECTORS = 600  # covers well past the cluster range used below
DATA_CLUSTERS = 70000   # >= FAT32's own 65525-cluster spec floor
SECTORS_PER_CLUSTER = 1  # 512-byte clusters - keeps file->cluster math simple to hand-verify

FAT_START = RESERVED_SECTORS
DATA_START = FAT_START + NUM_FATS * FAT_SIZE_SECTORS
TOTAL_SECTORS = DATA_START + DATA_CLUSTERS * SECTORS_PER_CLUSTER

EOC = 0x0FFFFFFF


def cluster_lba(cluster):
    return DATA_START + (cluster - 2) * SECTORS_PER_CLUSTER


class Image:
    def __init__(self):
        self.data = bytearray(TOTAL_SECTORS * SECTOR)
        self.fat = [0] * (DATA_CLUSTERS + 2)
        self.fat[0] = 0x0FFFFFF8
        self.fat[1] = 0x0FFFFFFF
        self.next_free_cluster = 3  # cluster 2 is reserved for the root dir below

    def write_bpb(self):
        bpb = bytearray(90)
        bpb[0:3] = b"\xeb\x3c\x90"
        bpb[3:11] = b"ZFSBOOT1"
        struct.pack_into("<H", bpb, 11, SECTOR)
        bpb[13] = SECTORS_PER_CLUSTER
        struct.pack_into("<H", bpb, 14, RESERVED_SECTORS)
        bpb[16] = NUM_FATS
        struct.pack_into("<H", bpb, 17, 0)  # root_entry_count - 0 on FAT32
        struct.pack_into("<H", bpb, 19, 0)  # total_sectors_16 - 0, using total_sectors_32
        bpb[21] = 0xF8
        struct.pack_into("<H", bpb, 22, 0)  # fat_size_16 - 0 on FAT32
        struct.pack_into("<H", bpb, 24, 63)
        struct.pack_into("<H", bpb, 26, 255)
        struct.pack_into("<I", bpb, 28, 0)
        struct.pack_into("<I", bpb, 32, TOTAL_SECTORS)
        struct.pack_into("<I", bpb, 36, FAT_SIZE_SECTORS)
        struct.pack_into("<H", bpb, 40, 0)
        struct.pack_into("<H", bpb, 42, 0)
        struct.pack_into("<I", bpb, 44, ROOT_CLUSTER)
        struct.pack_into("<H", bpb, 48, 1)
        struct.pack_into("<H", bpb, 50, 6)
        bpb[64] = 0x80
        bpb[66] = 0x29
        struct.pack_into("<I", bpb, 67, 0x12345678)
        bpb[71:82] = b"NO NAME    "
        bpb[82:90] = b"FAT32   "
        assert len(bpb) == 90
        self.data[0:90] = bpb
        self.data[510] = 0x55
        self.data[511] = 0xAA

    def set_fat_entry(self, cluster, value):
        self.fat[cluster] = value & 0x0FFFFFFF

    def flush_fats(self):
        for fat_index in range(NUM_FATS):
            base = (FAT_START + fat_index * FAT_SIZE_SECTORS) * SECTOR
            for c in range(DATA_CLUSTERS + 2):
                struct.pack_into("<I", self.data, base + c * 4, self.fat[c] & 0x0FFFFFFF)

    def write_cluster(self, cluster, content):
        assert len(content) <= SECTORS_PER_CLUSTER * SECTOR
        off = cluster_lba(cluster) * SECTOR
        self.data[off:off + len(content)] = content

    def alloc_chain(self, byte_length):
        """Allocates a (deliberately non-contiguous) chain of clusters
        covering byte_length bytes, returns (first_cluster, [clusters
        in order]). Clusters are taken out of numeric order on purpose
        - see module docstring."""
        cluster_bytes = SECTORS_PER_CLUSTER * SECTOR
        n = (byte_length + cluster_bytes - 1) // cluster_bytes
        if n == 0:
            n = 1
        clusters = []
        # Interleave with a skip pattern so consecutive clusters in the
        # chain are never numerically adjacent on disk.
        pool_start = self.next_free_cluster
        for i in range(n):
            clusters.append(pool_start + i * 2)
        self.next_free_cluster = pool_start + max(n * 2, 1) + 4
        for i, c in enumerate(clusters):
            self.set_fat_entry(c, clusters[i + 1] if i + 1 < len(clusters) else EOC)
        return clusters[0], clusters

    def alloc_chain_contiguous(self, byte_length):
        """Allocates a genuinely contiguous (consecutive cluster
        numbers, hence contiguous LBAs too - see cluster_to_lba() in
        fat.c) chain - the opposite of alloc_chain()'s own deliberate
        fragmentation. Exercises fat_read_range()'s own batched-read
        path (FAT_IO_BATCH_SECTORS in fat.c) for real, across multiple
        cluster boundaries and multiple full batches for a large
        enough file - every OTHER fixture in this module is
        deliberately fragmented and never exercises batching beyond a
        single sector at a time."""
        cluster_bytes = SECTORS_PER_CLUSTER * SECTOR
        n = (byte_length + cluster_bytes - 1) // cluster_bytes
        if n == 0:
            n = 1
        first = self.next_free_cluster
        clusters = list(range(first, first + n))
        self.next_free_cluster = first + n + 4
        for i, c in enumerate(clusters):
            self.set_fat_entry(c, clusters[i + 1] if i + 1 < len(clusters) else EOC)
        return clusters[0], clusters

    def alloc_chain_mixed(self, byte_length, run_length=20):
        """Alternating contiguous runs of `run_length` clusters, each
        run separated by a real gap (so consecutive runs are NOT
        adjacent to each other) - stresses fat_read_range()'s own
        run-extension-then-stop logic at real run boundaries, not just
        the "always contiguous" or "never contiguous" extremes the
        other two allocators cover."""
        cluster_bytes = SECTORS_PER_CLUSTER * SECTOR
        n = (byte_length + cluster_bytes - 1) // cluster_bytes
        if n == 0:
            n = 1
        clusters = []
        run_base = self.next_free_cluster
        while len(clusters) < n:
            run_len = min(run_length, n - len(clusters))
            clusters.extend(range(run_base, run_base + run_len))
            run_base += run_len + 4  # gap before the next run - breaks contiguity on purpose
        self.next_free_cluster = run_base
        for i, c in enumerate(clusters):
            self.set_fat_entry(c, clusters[i + 1] if i + 1 < len(clusters) else EOC)
        return clusters[0], clusters

    def _write_file_clusters(self, content, first, clusters):
        cluster_bytes = SECTORS_PER_CLUSTER * SECTOR
        for i, c in enumerate(clusters):
            chunk = content[i * cluster_bytes:(i + 1) * cluster_bytes]
            self.write_cluster(c, chunk)
        return first, len(content)

    def write_file(self, content):
        first, clusters = self.alloc_chain(len(content))
        return self._write_file_clusters(content, first, clusters)

    def write_file_contiguous(self, content):
        first, clusters = self.alloc_chain_contiguous(len(content))
        return self._write_file_clusters(content, first, clusters)

    def write_file_mixed(self, content, run_length=20):
        first, clusters = self.alloc_chain_mixed(len(content), run_length)
        return self._write_file_clusters(content, first, clusters)

    def dirent(self, name11, attr, first_cluster, size):
        ent = bytearray(32)
        ent[0:11] = name11
        ent[11] = attr
        struct.pack_into("<H", ent, 20, (first_cluster >> 16) & 0xFFFF)
        struct.pack_into("<H", ent, 26, first_cluster & 0xFFFF)
        struct.pack_into("<I", ent, 28, size)
        return bytes(ent)

    def pack_name(self, name):
        n = name.encode("ascii")
        assert len(n) <= 8
        return n.ljust(8) + b"   "

    def write_dir(self, cluster, entries):
        buf = bytearray()
        for name11, attr, first_cluster, size in entries:
            buf += self.dirent(name11, attr, first_cluster, size)
        buf += b"\x00" * (SECTORS_PER_CLUSTER * SECTOR - len(buf))
        self.write_cluster(cluster, buf)

    def save(self, path):
        self.flush_fats()
        with open(path, "wb") as f:
            f.write(self.data)


ATTR_DIR = 0x10


def build_base(kernel_content, initrd_content, cmdline_content, alloc_mode="fragmented"):
    img = Image()
    img.write_bpb()

    if alloc_mode == "contiguous":
        wf = img.write_file_contiguous
    elif alloc_mode == "mixed":
        wf = img.write_file_mixed
    else:
        wf = img.write_file

    kernel_first, kernel_size = wf(kernel_content)
    initrd_first, initrd_size = wf(initrd_content)
    cmdline_first, cmdline_size = wf(cmdline_content)

    alpine_cluster = img.next_free_cluster
    img.next_free_cluster += 1
    img.set_fat_entry(alpine_cluster, EOC)
    img.write_dir(alpine_cluster, [
        (img.pack_name("KERNEL"), 0, kernel_first, kernel_size),
        (img.pack_name("INITRD"), 0, initrd_first, initrd_size),
        (img.pack_name("CMDLINE"), 0, cmdline_first, cmdline_size),
    ])

    efi_cluster = img.next_free_cluster
    img.next_free_cluster += 1
    img.set_fat_entry(efi_cluster, EOC)
    img.write_dir(efi_cluster, [
        (img.pack_name("ALPINE"), ATTR_DIR, alpine_cluster, 0),
    ])

    img.set_fat_entry(ROOT_CLUSTER, EOC)
    img.write_dir(ROOT_CLUSTER, [
        (img.pack_name("EFI"), ATTR_DIR, efi_cluster, 0),
    ])

    return img, kernel_first


def build_high_cluster(content):
    """A dedicated, minimal image (its own EFI/ALPINE/KERNEL entry,
    same layout convention as build_base() - just not sharing that
    function's own code, so this doesn't touch what every other
    fixture already depends on) whose KERNEL file is placed at a
    cluster number >= 65536 - real, confirmed gap a full source
    audit's own mutation testing found: fat_open() reads a directory
    entry's first cluster as `(first_cluster_hi << 16) |
    first_cluster_lo` (fat.c), but every existing fixture's own files
    sit at low cluster numbers where first_cluster_hi is always 0 - a
    mutant that silently drops the high-word read entirely (returning
    just first_cluster_lo) would still pass every one of them. Real
    FAT32 volumes start at cluster 65525 by spec (this project's own
    fat_mount() already enforces that floor), so a first_cluster
    requiring the high word is not a theoretical case, just one no
    fixture had ever actually exercised.
    """
    img = Image()
    img.write_bpb()

    # Force the file's own first cluster comfortably past 65536 (the
    # low 16 bits would otherwise repeat/alias a small number - the
    # exact ambiguity first_cluster_hi exists to resolve). Contiguous
    # allocation - this test only cares about the cluster NUMBER being
    # correctly reassembled, not about exercising any particular
    # allocation pattern (already covered elsewhere).
    img.next_free_cluster = 65600
    kernel_first, kernel_clusters = img.alloc_chain_contiguous(len(content))
    assert kernel_first >= 65536, "fixture generator itself must place this above the 16-bit boundary"
    img._write_file_clusters(content, kernel_first, kernel_clusters)
    kernel_size = len(content)

    alpine_cluster = img.next_free_cluster
    img.next_free_cluster += 1
    img.set_fat_entry(alpine_cluster, EOC)
    img.write_dir(alpine_cluster, [
        (img.pack_name("KERNEL"), 0, kernel_first, kernel_size),
    ])

    efi_cluster = img.next_free_cluster
    img.next_free_cluster += 1
    img.set_fat_entry(efi_cluster, EOC)
    img.write_dir(efi_cluster, [
        (img.pack_name("ALPINE"), ATTR_DIR, alpine_cluster, 0),
    ])

    img.set_fat_entry(ROOT_CLUSTER, EOC)
    img.write_dir(ROOT_CLUSTER, [
        (img.pack_name("EFI"), ATTR_DIR, efi_cluster, 0),
    ])

    return img, kernel_first


def build_mid_chain_truncated(kernel_content, initrd_content, cmdline_content):
    """Like build_base(), but truncates the KERNEL chain to EOC at a
    cluster in the MIDDLE of its chain, not the first one - a real
    mutation-testing gap a full source audit found: the existing
    truncated_chain.img only ever corrupts the FIRST cluster (the
    degenerate, single-hop case: fat_read_range() never even gets to
    walk anywhere before hitting EOC) - a mutant in the mid-chain-walk/
    EOC-detection logic that only breaks on hop 2+ would be invisible
    to that fixture alone.
    """
    img = Image()
    img.write_bpb()

    kernel_first, kernel_clusters = img.alloc_chain(len(kernel_content))
    img._write_file_clusters(kernel_content, kernel_first, kernel_clusters)
    assert len(kernel_clusters) >= 3, "fixture generator needs a real multi-hop chain to truncate mid-chain"
    mid_index = len(kernel_clusters) // 2
    img.set_fat_entry(kernel_clusters[mid_index], EOC)

    initrd_first, initrd_size = img.write_file(initrd_content)
    cmdline_first, cmdline_size = img.write_file(cmdline_content)

    alpine_cluster = img.next_free_cluster
    img.next_free_cluster += 1
    img.set_fat_entry(alpine_cluster, EOC)
    img.write_dir(alpine_cluster, [
        (img.pack_name("KERNEL"), 0, kernel_first, len(kernel_content)),
        (img.pack_name("INITRD"), 0, initrd_first, initrd_size),
        (img.pack_name("CMDLINE"), 0, cmdline_first, cmdline_size),
    ])

    efi_cluster = img.next_free_cluster
    img.next_free_cluster += 1
    img.set_fat_entry(efi_cluster, EOC)
    img.write_dir(efi_cluster, [
        (img.pack_name("ALPINE"), ATTR_DIR, alpine_cluster, 0),
    ])

    img.set_fat_entry(ROOT_CLUSTER, EOC)
    img.write_dir(ROOT_CLUSTER, [
        (img.pack_name("EFI"), ATTR_DIR, efi_cluster, 0),
    ])

    return img, kernel_first


def build_nontrivial_cycle(initrd_content, cmdline_content):
    """Like build_base(), but makes the KERNEL chain cyclic via a
    genuine multi-hop cycle (cluster N points back at an earlier
    cluster, not at itself), not the existing cyclic_chain.img's own
    degenerate immediate self-loop (cluster 0 points at itself) - a
    real mutation-testing gap a full source audit found: a mutant in
    the hare/tortoise cycle-DETECTION logic that only catches an
    IMMEDIATE self-loop (hare and tortoise meet on the very first
    comparison) but not a longer cycle (the hare has to lap the
    tortoise around several real hops first) would be invisible to
    that fixture alone.

    Uses its OWN large, dedicated KERNEL content (NOT the small, shared
    ~10-cluster kernel_content every other fixture uses) for a real,
    load-bearing reason found while building this fixture, not merely
    for variety: fat.c's own hare/tortoise needs roughly 2*L hops to
    detect a cycle of length L (see fat_chain_walk's own comment) - a
    SHORT file whose own declared size needs fewer total hops than that
    can satisfy its whole byte budget by re-reading a short cycle's
    clusters just a few times, completing "successfully" with silently
    WRONG (repeated) data before the tortoise ever catches up. Confirmed
    the hard way: an earlier version of this fixture used the shared
    10-cluster content with an 8-cluster cycle, needing only 9 total
    hops - well under the ~16 needed to detect that cycle length - and
    the test reported the read as succeeding, not failing. A file
    needing far more total hops than 2*cycle_length is what actually
    proves multi-hop cycle DETECTION works, as opposed to proving this
    same short-cycle-vs-short-budget interaction the hare/tortoise
    design doesn't fully cover.
    """
    img = Image()
    img.write_bpb()

    kernel_content = bytes((i * 43 + 17) % 233 for i in range(50 * 512))
    kernel_first, kernel_clusters = img.alloc_chain(len(kernel_content))
    img._write_file_clusters(kernel_content, kernel_first, kernel_clusters)
    assert len(kernel_clusters) >= 50, "fixture generator needs a genuinely large chain for this to actually prove anything"
    # Redirect cluster index 5 back to cluster index 1 - a real,
    # 4-cluster cycle (1,2,3,4,1,2,3,4,...) entered after only 5 real
    # hops, but the file's own declared size needs ~50 total hops to
    # satisfy - far more than the ~8 hops (2*4) the tortoise needs to
    # detect this specific cycle, so detection has plenty of runway
    # before the byte budget could ever be satisfied by repetition.
    img.set_fat_entry(kernel_clusters[5], kernel_clusters[1])

    initrd_first, initrd_size = img.write_file(initrd_content)
    cmdline_first, cmdline_size = img.write_file(cmdline_content)

    alpine_cluster = img.next_free_cluster
    img.next_free_cluster += 1
    img.set_fat_entry(alpine_cluster, EOC)
    img.write_dir(alpine_cluster, [
        (img.pack_name("KERNEL"), 0, kernel_first, len(kernel_content)),
        (img.pack_name("INITRD"), 0, initrd_first, initrd_size),
        (img.pack_name("CMDLINE"), 0, cmdline_first, cmdline_size),
    ])

    efi_cluster = img.next_free_cluster
    img.next_free_cluster += 1
    img.set_fat_entry(efi_cluster, EOC)
    img.write_dir(efi_cluster, [
        (img.pack_name("ALPINE"), ATTR_DIR, alpine_cluster, 0),
    ])

    img.set_fat_entry(ROOT_CLUSTER, EOC)
    img.write_dir(ROOT_CLUSTER, [
        (img.pack_name("EFI"), ATTR_DIR, efi_cluster, 0),
    ])

    return img, kernel_first


def main():
    outdir = sys.argv[1]
    os.makedirs(outdir, exist_ok=True)

    # A deliberately non-round byte pattern so an off-by-one in either
    # the offset or the length shows up as a real mismatch, not a
    # coincidentally-matching repeated byte.
    kernel_content = bytes((i * 7 + 3) % 251 for i in range(5000))
    initrd_content = bytes((i * 13 + 11) % 233 for i in range(1500))
    cmdline_content = b"root=ZFS=zroot/ROOT/alpine ro console=tty0\n"

    img, kernel_first = build_base(kernel_content, initrd_content, cmdline_content)
    img.save(os.path.join(outdir, "valid.img"))
    with open(os.path.join(outdir, "valid.kernel"), "wb") as f:
        f.write(kernel_content)
    with open(os.path.join(outdir, "valid.initrd"), "wb") as f:
        f.write(initrd_content)
    with open(os.path.join(outdir, "valid.cmdline"), "wb") as f:
        f.write(cmdline_content)

    # A large, genuinely contiguous KERNEL - spans several full
    # FAT_IO_BATCH_SECTORS=36-sector batches (100000 bytes / 512
    # bytes-per-cluster here = ~196 clusters, ~5.4 batches) - proves
    # fat_read_range()'s own multi-batch, cross-cluster-boundary
    # run-extension path is actually correct, not just "doesn't crash"
    # - every fixture above uses a deliberately fragmented allocation,
    # which never exercises batching beyond a single sector at a time.
    big_content = bytes((i * 31 + 7) % 241 for i in range(100000))
    img_contig, _ = build_base(big_content, initrd_content, cmdline_content, alloc_mode="contiguous")
    img_contig.save(os.path.join(outdir, "contiguous.img"))
    with open(os.path.join(outdir, "contiguous.kernel"), "wb") as f:
        f.write(big_content)

    # A MUCH larger contiguous KERNEL, deliberately at the scale a full
    # source audit's own mutation testing found the 196-cluster fixture
    # above was too small to catch: a plausible "cleaner" 2-way parity
    # cache (instead of fat.c's real dedicated hare/tortoise slots) can
    # pass the small fixture cheaply while still costing an order of
    # magnitude more real disk_read_lba() calls once the chain spans
    # enough DISTINCT FAT sectors for that narrower scheme to start
    # thrashing - each FAT32 entry is 4 bytes, so one 512-byte FAT
    # sector holds 128 of them; ~8000 clusters (~4.1MB at this fixture
    # generator's own 1-sector-per-cluster convention) spans ~62 FAT
    # sectors, comfortably past the single-FAT-sector (~128-cluster)
    # threshold where a too-narrow cache scheme's own thrashing would
    # actually show up, and past the 196-cluster fixture's own ~1.5
    # FAT-sector span (barely into "crosses a boundary at all",
    # nowhere near "thrashes repeatedly").
    huge_content = bytes((i * 37 + 5) % 239 for i in range(8000 * 512))
    img_huge, _ = build_base(huge_content, initrd_content, cmdline_content, alloc_mode="contiguous")
    img_huge.save(os.path.join(outdir, "large_contiguous.img"))
    with open(os.path.join(outdir, "large_contiguous.kernel"), "wb") as f:
        f.write(huge_content)

    # Alternating contiguous runs of 20 clusters (smaller than the
    # 36-sector batch cap) with real gaps between them - forces
    # fat_read_range() to actually STOP extending a run partway
    # through building several different batches, not just once at
    # the very end of the file - the boundary case build_base()'s own
    # default fragmented allocator (never contiguous even for two
    # consecutive clusters) and the new contiguous one above (always
    # contiguous) both skip entirely.
    mixed_content = bytes((i * 17 + 3) % 199 for i in range(50000))
    img_mixed, _ = build_base(mixed_content, initrd_content, cmdline_content, alloc_mode="mixed")
    img_mixed.save(os.path.join(outdir, "mixed.img"))
    with open(os.path.join(outdir, "mixed.kernel"), "wb") as f:
        f.write(mixed_content)

    high_cluster_content = bytes((i * 41 + 13) % 227 for i in range(3000))
    img_high, _ = build_high_cluster(high_cluster_content)
    img_high.save(os.path.join(outdir, "high_cluster.img"))
    with open(os.path.join(outdir, "high_cluster.kernel"), "wb") as f:
        f.write(high_cluster_content)

    img2, _ = build_base(kernel_content, initrd_content, cmdline_content)
    img2.data[510] = 0x00
    img2.data[511] = 0x00
    img2.save(os.path.join(outdir, "bad_sig.img"))

    img3, _ = build_base(kernel_content, initrd_content, cmdline_content)
    struct.pack_into("<H", img3.data, 11, 128)
    img3.save(os.path.join(outdir, "bad_sector_size.img"))

    img4, _ = build_base(kernel_content, initrd_content, cmdline_content)
    img4.data[13] = 3
    img4.save(os.path.join(outdir, "bad_cluster_size.img"))

    img5, _ = build_base(kernel_content, initrd_content, cmdline_content)
    struct.pack_into("<H", img5.data, 17, 512)  # root_entry_count - nonzero => FAT16-shaped
    struct.pack_into("<H", img5.data, 22, 200)  # fat_size_16 - nonzero => FAT16-shaped
    img5.save(os.path.join(outdir, "fat16_disguise.img"))

    # Regression fixtures for a real mutation-testing gap a full source
    # audit found: fat16_disguise.img above corrupts BOTH
    # root_entry_count and fat_size_16 at once, so a test against it
    # alone can't tell which of fat_mount()'s two separate checks
    # (`bpb->root_entry_count != 0`, `bpb->fat_size_16 != 0`) is the one
    # actually doing the rejecting - a mutant that silently deleted
    # either check on its own would still be caught by the OTHER one,
    # invisibly. These two fixtures corrupt exactly ONE of the two
    # fields each, independently proving both checks are load-bearing
    # on their own.
    img5a, _ = build_base(kernel_content, initrd_content, cmdline_content)
    struct.pack_into("<H", img5a.data, 17, 512)  # root_entry_count only
    img5a.save(os.path.join(outdir, "fat16_disguise_root_entry_count_only.img"))

    img5b, _ = build_base(kernel_content, initrd_content, cmdline_content)
    struct.pack_into("<H", img5b.data, 22, 200)  # fat_size_16 only
    img5b.save(os.path.join(outdir, "fat16_disguise_fat_size_16_only.img"))

    img6, kernel_first6 = build_base(kernel_content, initrd_content, cmdline_content)
    # Truncate KERNEL's chain: force its first cluster straight to EOC,
    # even though the directory entry still claims the full size.
    img6.set_fat_entry(kernel_first6, EOC)
    img6.save(os.path.join(outdir, "truncated_chain.img"))

    img7, kernel_first7 = build_base(kernel_content, initrd_content, cmdline_content)
    # Make KERNEL's chain cyclic: point its first cluster back at itself.
    img7.set_fat_entry(kernel_first7, kernel_first7)
    img7.save(os.path.join(outdir, "cyclic_chain.img"))

    img10, kernel_first10 = build_mid_chain_truncated(kernel_content, initrd_content, cmdline_content)
    img10.save(os.path.join(outdir, "mid_chain_truncated.img"))

    img11, kernel_first11 = build_nontrivial_cycle(initrd_content, cmdline_content)
    img11.save(os.path.join(outdir, "nontrivial_cycle.img"))

    # bad_geometry.img - total_sectors_32 inflated to imply far more
    # clusters than the REAL, unchanged fat_size_32 (600 sectors, same
    # as every other fixture here) can actually describe - a full
    # source audit found fat_mount() never cross-checked this at all.
    # 200000 data clusters needs ceil((200000+2)*4/512) = 1563 FAT
    # sectors; only 600 are actually there. Everything else about this
    # image (reserved_sector_count, num_fats, fat_size_32 itself) stays
    # exactly as build_base() already set it - only the ONE field this
    # check is supposed to catch a lie in gets changed, so a fixture
    # that accidentally passes for some OTHER reason (an unrelated
    # rejection elsewhere) isn't mistaken for proving this specific
    # check works.
    img8, _ = build_base(kernel_content, initrd_content, cmdline_content)
    inflated_total_sectors = DATA_START + 200000 * SECTORS_PER_CLUSTER
    struct.pack_into("<I", img8.data, 32, inflated_total_sectors)
    img8.save(os.path.join(outdir, "bad_geometry.img"))

    # oversized_bpb_vs_partition.img - a SELF-CONSISTENT BPB (unlike
    # bad_geometry.img above, total_sectors_32 AND fat_size_32 are
    # inflated TOGETHER, in proportion, so the fat_size-vs-cluster_count
    # check that catches bad_geometry.img does NOT catch this one) whose
    # claimed total_sectors_32 exceeds the REAL, physical size of this
    # file - a second, independent audit found fat_mount() had no way
    # to catch this at all: it only ever checked the BPB against
    # itself, never against the real partition size GPT/MBR already
    # know and stage2_main.c was discarding (see fat_mount()'s own
    # partition_sectors parameter and comment). The file this saves is
    # deliberately left at its normal, real byte size (img9.data is
    # never resized) - only the BPB's own claim grows - so
    # disk_sectors() in fat_host_test.c (the file's own real size)
    # stays the correct, smaller ground truth to check the inflated
    # claim against, exactly mirroring how stage2_main.c's real
    # esp_sectors (from GPT/MBR) relates to a volume's own BPB.
    img9, _ = build_base(kernel_content, initrd_content, cmdline_content)
    target_total_sectors = 3 * TOTAL_SECTORS
    fat_size_guess = FAT_SIZE_SECTORS * 3
    while True:
        reserved_plus_fats = RESERVED_SECTORS + NUM_FATS * fat_size_guess
        data_sectors = target_total_sectors - reserved_plus_fats
        cluster_count = data_sectors // SECTORS_PER_CLUSTER
        required_fat_sectors = ((cluster_count + 2) * 4 + SECTOR - 1) // SECTOR
        if fat_size_guess >= required_fat_sectors:
            break
        fat_size_guess = required_fat_sectors
    struct.pack_into("<I", img9.data, 32, target_total_sectors)
    struct.pack_into("<I", img9.data, 36, fat_size_guess)
    assert len(img9.data) == TOTAL_SECTORS * SECTOR, \
        "oversized_bpb_vs_partition.img's own real file size must stay unchanged - only the BPB's claim grows"
    img9.save(os.path.join(outdir, "oversized_bpb_vs_partition.img"))

    print("fixtures written to", outdir)


if __name__ == "__main__":
    main()
