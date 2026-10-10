#!/usr/bin/env python3
"""
fat_random_chains_test.py - random-but-valid FAT32 cluster chains through the real bios/fat.c.

The other FAT tests use hand-picked fixtures (contiguous, fragmented, mixed). A real Linux vfat
volume can scatter a file anywhere, so this builds N volumes whose KERNEL chain is contiguous,
fully scattered, runs of random length ascending or descending, every-second-cluster, or runs sized
around the 64-sector batch, with 1/2/4-sector clusters, and reads the file from several offsets (the
real loader reads the kernel from offset 5*512 and later ones) with both batch sizes (64, 36).
Every byte must match. Written while looking for the cause of a real-hardware "FAT read failed
loading kernel": on valid chains fat.c is correct, so that failure is not an ordinary
fragmentation bug.
"""
import random, struct, sys, subprocess, os

def build(path, expected_path, spc, size, strategy, rng):
    SEC = 512
    nclusters = max(65600, (size // (SEC * spc)) * 3)           # a FAT32-sized volume with room to scatter
    fat_sectors = ((nclusters + 2) * 4 + SEC - 1) // SEC
    reserved = 32
    data_start = reserved + 2 * fat_sectors
    total = data_start + nclusters * spc
    need = (size + SEC * spc - 1) // (SEC * spc)
    free = list(range(3, nclusters + 2))                         # cluster 2 = root dir
    if strategy == 'contiguous':
        start = rng.randrange(3, nclusters + 2 - need)
        chain = list(range(start, start + need))
    elif strategy == 'scattered':
        chain = rng.sample(free, need)
    elif strategy == 'runs':
        chain = []; used = set()
        while len(chain) < need:
            L = rng.randint(1, 300); s = rng.randrange(3, nclusters + 2 - L)
            run = [c for c in range(s, s + L) if c not in used][:need - len(chain)]
            if not run or any(c in used for c in run): continue
            for c in run: used.add(c)
            chain += run
    elif strategy == 'descending-runs':
        chain = []; used = set()
        while len(chain) < need:
            L = rng.randint(1, 200); s = rng.randrange(3 + L, nclusters + 2)
            run = [c for c in range(s, s - L, -1) if c not in used][:need - len(chain)]
            if not run: continue
            for c in run: used.add(c)
            chain += run
    elif strategy == 'gap1':
        start = rng.randrange(3, nclusters - 2 * need - 4)
        chain = [start + 2 * i for i in range(need)]
    else:                                                          # 'batch-edge': runs sized around the 64-sector batch
        chain = []; used = set()
        run_len = max(1, 64 // spc)
        while len(chain) < need:
            L = rng.choice([run_len - 1, run_len, run_len + 1, 2 * run_len]) or 1
            s = rng.randrange(3, nclusters + 2 - L)
            run = [c for c in range(s, s + L) if c not in used][:need - len(chain)]
            if not run: continue
            for c in run: used.add(c)
            chain += run
    content = bytes(rng.getrandbits(8) for _ in range(min(size, 4096))) * (size // 4096 + 1)
    content = bytearray(content[:size])
    for i in range(0, size, 4093): content[i] = (i // 4093) & 0xff  # position-dependent bytes
    open(expected_path, 'wb').write(content)
    with open(path, 'wb') as f:
        f.truncate(total * SEC)
        bpb = bytearray(512)
        bpb[0:3] = b'\xeb\x58\x90'; bpb[3:11] = b'MSWIN4.1'
        struct.pack_into('<HBHBHHBHHHII', bpb, 11, SEC, spc, reserved, 2, 0, 0, 0xF8, 0, 63, 255, 0, total)
        struct.pack_into('<IHHI', bpb, 36, fat_sectors, 0, 0, 2)
        struct.pack_into('<HH', bpb, 48, 1, 6)
        bpb[66] = 0x29; struct.pack_into('<I', bpb, 67, 0x1234ABCD); bpb[71:82] = b'EFI        '; bpb[82:90] = b'FAT32   '
        bpb[510] = 0x55; bpb[511] = 0xAA
        f.seek(0); f.write(bpb)
        fat = bytearray((nclusters + 2) * 4)
        struct.pack_into('<III', fat, 0, 0x0FFFFFF8, 0x0FFFFFFF, 0x0FFFFFFF)   # media, EOC, root dir (cluster 2) = 1 cluster
        for a, b in zip(chain, chain[1:] + [0x0FFFFFFF]): struct.pack_into('<I', fat, a * 4, b)
        for k in range(2): f.seek((reserved + k * fat_sectors) * SEC); f.write(fat)
        ent = bytearray(32); ent[0:11] = b'KERNEL     '; ent[11] = 0x20
        struct.pack_into('<H', ent, 20, chain[0] >> 16); struct.pack_into('<H', ent, 26, chain[0] & 0xFFFF); struct.pack_into('<I', ent, 28, size)
        f.seek(data_start * SEC); f.write(ent)
        for i, c in enumerate(chain):
            off = i * spc * SEC
            f.seek((data_start + (c - 2) * spc) * SEC); f.write(content[off:off + spc * SEC])
    return chain

if __name__ == '__main__':
    # usage: fat_random_chains_test.py <workdir> <seed> <cases> <driver64> <driver36>
    workdir, seed, n, drivers = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), {64: sys.argv[4], 36: sys.argv[5]}
    os.chdir(workdir); rng = random.Random(seed)
    bad = 0
    for i in range(n):
        spc = rng.choice([1, 1, 2, 4])
        size = rng.choice([700_000, 2_000_000, 5_000_000, 6_800_000])
        strategy = rng.choice(['contiguous', 'scattered', 'runs', 'descending-runs', 'gap1', 'batch-edge'])
        build('case.img', 'case.exp', spc, size, strategy, rng)
        offs = [0, 2560, 5 * 512, 17408 + 512 * rng.randrange(0, 9)]
        for off in offs:
            length = size - off
            for batch in (64, 36):
                r = subprocess.run([drivers[batch], 'case.img', 'case.exp', str(off), str(length)], capture_output=True, text=True)
                if r.stdout.strip() != 'OK':
                    bad += 1
                    print(f'FAIL seed={seed} case={i} spc={spc} size={size} strategy={strategy} off={off} batch={batch}: {r.stdout.strip()}')
    print('cases', n, 'failures', bad)
    sys.exit(1 if bad else 0)
