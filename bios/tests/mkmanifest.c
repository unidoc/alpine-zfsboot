/*
 * mkmanifest --kernel K --initrd I --out DIR [--integrity M] - writes DIR/CHECKSUM and
 * OUTDIR/BLKSUM for a kernel/initrd pair, like `alpine-zfsboot
 * payload-manifest` does. For tests that have no Go toolchain (the
 * QEMU/ISO tests in tests/, run by CI in alpine:<version>); the product
 * writer is the Go tool, and both match golden files.
 *
 * Build: gcc -I bios -o mkmanifest bios/tests/mkmanifest.c bios/payload_sum.c bios/blkverify.c
 */
#include "blksum_writer.h"

static uint8_t *slurp(const char *path, uint32_t *n)
{
	FILE *f = fopen(path, "rb");
	long sz;
	uint8_t *b;

	if (!f || fseek(f, 0, SEEK_END) != 0 || (sz = ftell(f)) < 0 || fseek(f, 0, SEEK_SET) != 0) {
		perror(path);
		exit(1);
	}
	b = malloc(sz ? (size_t)sz : 1);
	if (fread(b, 1, (size_t)sz, f) != (size_t)sz) {
		perror(path);
		exit(1);
	}
	fclose(f);
	*n = (uint32_t)sz;
	return b;
}

static void spit(const char *dir, const char *name, const void *p, size_t n)
{
	char path[4096];
	FILE *f;

	snprintf(path, sizeof(path), "%s/%s", dir, name);
	f = fopen(path, "wb");
	if (!f || fwrite(p, 1, n, f) != n || fclose(f) != 0) {
		perror(path);
		exit(1);
	}
}

int main(int argc, char **argv)
{
	uint32_t ks, is, koff, bs;
	uint8_t *k, *in, *blk;
	char sum[512];
	const char *kf = 0, *inf = 0, *out = 0, *mode = "warn";
	int n, i;

	/* The same flags as `alpine-zfsboot payload-manifest` (iso.sh runs either). */
	for (i = 1; i + 1 < argc; i += 2) {
		if (!strcmp(argv[i], "--kernel"))
			kf = argv[i + 1];
		else if (!strcmp(argv[i], "--initrd"))
			inf = argv[i + 1];
		else if (!strcmp(argv[i], "--out"))
			out = argv[i + 1];
		else if (!strcmp(argv[i], "--integrity"))
			mode = argv[i + 1];
		else
			break;
	}
	if (!kf || !inf || !out || i != argc) {
		fprintf(stderr, "usage: %s --kernel K --initrd I --out DIR [--integrity off|warn|enforce]\n", argv[0]);
		return 2;
	}
	k = slurp(kf, &ks);
	in = slurp(inf, &is);
	if (ks < 0x206 || k[0x1fe] != 0x55 || k[0x1ff] != 0xaa || memcmp(k + 0x202, "HdrS", 4) != 0) {
		fprintf(stderr, "%s: not a bzImage\n", kf);
		return 1;
	}
	koff = ((k[0x1f1] ? k[0x1f1] : 4) + 1) * 512u;
	if (koff >= ks) {
		fprintf(stderr, "%s: smaller than its real-mode part\n", kf);
		return 1;
	}
	blk = bw_blksum(k, ks, koff, in, is, &bs);
	spit(out, "BLKSUM", blk, bs);
	n = bw_checksum(sum, sizeof(sum), k, ks, koff, in, is, mode);
	spit(out, "CHECKSUM", sum, (size_t)n);
	return 0;
}
