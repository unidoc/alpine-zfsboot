/*
 * fat_random_chains_driver.c - one case of fat_random_chains_test.py: mounts a FAT32 image whose
 * /KERNEL has a randomly scattered (but valid) cluster chain, reads a range through the real
 * fat_read_range() (bios/fat.c, compiled for the host) and compares it with the expected bytes.
 * Prints OK, or why not (and fat_read_range()'s own failure reason).
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "../fat.h"
#include "../disk.h"
#include "../switch32.h"

static uint8_t *g_disk; static long g_size;
#define PHYS (16u*1024*1024)
static uint8_t *g_phys;
void disk_init(uint8_t d) { (void)d; }
int disk_read_lba(uint64_t lba, uint16_t count, void *buf) {
	long off = (long)(lba * 512), len = (long)count * 512;
	if (off < 0 || off + len > g_size) return -1;
	memcpy(buf, g_disk + off, len); return 0;
}
void unreal_copy(uint32_t dst, const void *src, uint32_t len) { if ((uint64_t)dst + len > PHYS) abort(); memcpy(g_phys + dst, src, len); }
static uint8_t *slurp(const char *p, long *n) { FILE *f = fopen(p, "rb"); if (!f) { perror(p); exit(3);} fseek(f,0,SEEK_END); *n=ftell(f); fseek(f,0,SEEK_SET); uint8_t *b=malloc(*n?*n:1); if (fread(b,1,*n,f)!=(size_t)*n) exit(3); fclose(f); return b; }
int main(int argc, char **argv) {
	/* argv: img expected offset length */
	long en; g_phys = malloc(PHYS);
	g_disk = slurp(argv[1], &g_size); uint8_t *exp = slurp(argv[2], &en);
	uint32_t off = (uint32_t)strtoul(argv[3], 0, 0), len = (uint32_t)strtoul(argv[4], 0, 0);
	struct fat_volume vol; struct fat_file f;
	if (fat_mount(&vol, 0, (uint64_t)(g_size / 512)) != 0) { puts("MOUNT-FAIL"); return 2; }
	if (fat_open(&vol, "/KERNEL", &f) != 0) { puts("OPEN-FAIL"); return 2; }
	memset(g_phys, 0xEE, PHYS);
	int rc = fat_read_range(&vol, &f, off, len, 0x1000, 0, 0);
	if (rc != 0) { printf("READ-FAIL reason=%d cluster=%u value=0x%x left=%u\n", g_fat_error.reason, g_fat_error.cluster, g_fat_error.value, g_fat_error.remaining); return 1; }
	if (memcmp(g_phys + 0x1000, exp + off, len) != 0) { puts("DATA-MISMATCH"); return 1; }
	puts("OK"); return 0;
}
