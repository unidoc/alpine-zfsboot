#include "disk.h"

/*
 * How a read request becomes BIOS calls (the one place that decides).
 *
 * The first real-hardware failure of this loader, "FAT read failed loading
 * kernel" a few MiB into the kernel on a physical host, could not be
 * explained: the FAT layer asked for up to FAT_IO_BATCH_SECTORS (then 64) sectors
 * in ONE INT 13h AH=42h call, and nothing guarantees every BIOS or option
 * ROM accepts a transfer that large. Whether that was the cause or not, no
 * BIOS disk I/O should depend on it. So:
 *
 *  - a request is split into chunks of at most g_chunk sectors, starting at
 *    the configured cap (DISK_MAX_CHUNK_SECTORS, or alpine-zfsboot.int13chunk
 *    via disk_set_chunk()) - the FAT layer asks for up to 32 at a time;
 *  - when a call fails, the controller is reset and the SAME sectors are
 *    retried with half the chunk size, down to a single sector; the smaller
 *    size is kept, because a BIOS that refused 64 will refuse it again;
 *  - a failure at ONE sector gets a single retry after a reset (the
 *    transient case) and is then final;
 *  - every shrink is reported (disk_notify_shrink), every failed call is
 *    handed to disk_notify_failure (printed only in diagnostic mode), and
 *    the last one is kept in g_disk_last_error for the fatal message, so a
 *    failure says what the BIOS returned and where.
 *
 * Plain C with no BIOS or console dependency of its own, so
 * bios/tests/disk_policy_host_test.c can run it against a fake INT 13h.
 */

static uint16_t g_chunk = DISK_MAX_CHUNK_SECTORS;
struct disk_stats g_disk_stats = { 0, DISK_MAX_CHUNK_SECTORS, DISK_MAX_CHUNK_SECTORS, 0, 0 };
uint16_t g_disk_bios_limit = DISK_CHUNK_LIMIT;

void disk_set_chunk(uint16_t sectors)
{
	if (sectors < 1 || sectors > DISK_CHUNK_LIMIT)
		return;
	if (sectors > g_disk_bios_limit)
		sectors = g_disk_bios_limit;
	g_chunk = sectors;
	g_disk_stats.cap = sectors;
	g_disk_stats.chunk = sectors;
}

uint16_t disk_get_chunk(void)
{
	return g_chunk;
}

int disk_lba_to_chs(uint64_t lba, uint16_t heads, uint16_t spt, uint16_t cyls,
                    uint16_t *c, uint8_t *h, uint8_t *s, uint16_t *left)
{
	uint32_t l, cyl;

	if (heads == 0 || spt == 0 || spt > 63 || lba > 0xffffffffULL)
		return -1;
	l = (uint32_t)lba;
	cyl = l / ((uint32_t)heads * spt);
	if (cyl >= cyls || cyl > 1023)
		return -1;
	*c = (uint16_t)cyl;
	*h = (uint8_t)((l / spt) % heads);
	*s = (uint8_t)(l % spt + 1);
	*left = (uint16_t)(spt - (l % spt));
	return 0;
}

#ifdef DISK_POLICY_HOST_TEST
void disk_policy_reset_for_test(void)
{
	g_chunk = DISK_MAX_CHUNK_SECTORS;
	g_disk_bios_limit = DISK_CHUNK_LIMIT;
	g_disk_stats.failures = 0;
	g_disk_stats.calls = 0;
	g_disk_stats.cap = g_disk_stats.chunk = DISK_MAX_CHUNK_SECTORS;
	g_disk_stats.largest = 0;
}
#endif

static void note_failure(int status, uint64_t lba, uint16_t count)
{
	g_disk_last_error.status = (uint16_t)status;
	g_disk_last_error.lba = lba;
	g_disk_last_error.count = count;
	g_disk_last_error.chunk = g_chunk;
}

int disk_int13_status(uint8_t failed, uint16_t ax, uint16_t dap_count, uint16_t count)
{
	/*
	 * On failure AH holds the BIOS's own status byte (INT 13h AH=42h):
	 * 0x09 data boundary error, 0x20 controller failure, 0x40 seek
	 * failure, 0xAA drive not ready, ... A BIOS that sets carry but
	 * leaves AH zero is reported as 0xFF.
	 */
	if (failed)
		return (ax >> 8) ? (int)(ax >> 8) : 0xFF;
	/*
	 * Not every BIOS updates the DAP's sector count after the call, but
	 * one that does and reports fewer sectors than asked for, with carry
	 * clear, has NOT delivered the whole request (see disk.c).
	 */
	if (dap_count != count)
		return DISK_ERR_SHORT;
	return 0;
}

int disk_read_lba(uint64_t lba, uint16_t count, void *buf)
{
	uint8_t *p = (uint8_t *)buf;
	uint8_t attempt = 0;

	while (count > 0) {
		uint16_t n = count < g_chunk ? count : g_chunk;
		int st = disk_int13_read(lba, n, p);

		g_disk_stats.calls++;
		if (st != 0) {
			note_failure(st, lba, n);
			g_disk_stats.failures++;
			if (attempt < 255)
				attempt++;
			disk_notify_failure(&g_disk_last_error, attempt);
			disk_int13_reset();
#ifdef DISK_POLICY_NO_SHRINK
			/* Diagnostic build only (-DDISK_POLICY_NO_SHRINK): report the failure and
			 * stop, without the workaround, so a failing machine shows what the BIOS
			 * returned instead of being papered over. */
			return -1;
#endif
			if (n > 1) {
				g_chunk = (uint16_t)(n / 2);
				g_disk_stats.chunk = g_chunk;
				g_disk_last_error.chunk = g_chunk;
				disk_notify_shrink(&g_disk_last_error, g_chunk);
				continue; /* the same sectors, in smaller pieces */
			}
			st = disk_int13_read(lba, 1, p);
			g_disk_stats.calls++;
			if (st != 0) {
				note_failure(st, lba, 1);
				if (attempt < 255)
					attempt++;
				disk_notify_failure(&g_disk_last_error, attempt);
				return -1;
			}
		}
		if (n > g_disk_stats.largest)
			g_disk_stats.largest = n;
		lba += n;
		p += (uint32_t)n * DISK_SECTOR_SIZE;
		count -= n;
	}
	return 0;
}
