#include "disk.h"
#include "fat.h"

static uint8_t g_drive_number;
struct disk_info g_disk_info;

static int disk_int13_read_lba(uint64_t lba, uint16_t count, void *buf);

/* INT 13h AH=41h: 0 (and the EDD version and API bits) when the extensions are present. */
static int int13_ext_check(uint8_t drive, uint8_t *ver, uint16_t *api)
{
	uint16_t ax = 0x4100, bx = 0x55aa, cx = 0;
	uint16_t dx = drive;
	uint8_t failed;

	__asm__ __volatile__(
		"int $0x13\n\t"
		"setc %0\n\t"
		: "=m"(failed), "+a"(ax), "+b"(bx), "+c"(cx), "+d"(dx)
		:
		: "esi", "edi", "ebp", "cc", "memory"
	);
	if (failed || bx != 0xaa55)
		return -1;
	*ver = (uint8_t)(ax >> 8);
	*api = cx;
	return 0;
}

/* The EDD drive parameter table AH=48h fills (first 0x1e bytes: EDD 2.x;
 * an EDD 1.1 BIOS fills the first 0x1a and says so in .size). The buffer is
 * as large as the largest EDD table (0x4a bytes, EDD 3.0 - what Linux's
 * edd.c passes too) although .size asks for 0x1e only: a BIOS that fills
 * its whole table regardless must not write past the buffer into this
 * stage's stack. */
struct edd_params {
	uint16_t size;
	uint16_t flags;
	uint32_t cyls, heads, spt;
	uint64_t sectors;
	uint16_t bytes_per_sector;
	uint32_t dpte;
	uint8_t edd30[0x4a - 0x1e];
} __attribute__((packed));
_Static_assert(sizeof(struct edd_params) == 0x4a, "EDD 3.0 parameter table size");

/* INT 13h AH=48h: 0 and the table, or -1. */
static int int13_get_params(uint8_t drive, struct edd_params *p)
{
	uint16_t ax = 0x4800, si = (uint16_t)(unsigned long)p;
	uint16_t dx = drive;
	uint8_t failed;
	uint8_t *z = (uint8_t *)p;
	unsigned i;

	for (i = 0; i < sizeof(*p); i++)
		z[i] = 0;
	p->size = 0x1e;
	__asm__ __volatile__(
		"int $0x13\n\t"
		"setc %0\n\t"
		: "=m"(failed), "+a"(ax), "+S"(si), "+d"(dx)
		:
		: "ebx", "ecx", "edi", "ebp", "cc", "memory"
	);
	return failed ? -1 : 0;
}

/* INT 13h AH=08h: the CHS geometry, for AH=02h. %es is preserved (in
 * %si - not on the stack: `failed` is a memory operand the compiler may
 * address relative to %esp, so nothing here may move the stack pointer):
 * some BIOSes point ES:DI at a floppy parameter table here. */
static int int13_get_geometry(uint8_t drive, uint16_t *heads, uint16_t *spt, uint16_t *cyls)
{
	uint16_t ax = 0x0800, cx = 0, dx = drive;
	uint8_t failed;

	__asm__ __volatile__(
		"movw %%es, %%si\n\t"
		"xorw %%di, %%di\n\t"
		"int $0x13\n\t"
		"setc %0\n\t"
		"movw %%si, %%es\n\t"
		: "=m"(failed), "+a"(ax), "+c"(cx), "+d"(dx)
		:
		: "ebx", "esi", "edi", "ebp", "cc", "memory"
	);
	if (failed || (cx & 0x3f) == 0)
		return -1;
	*spt = cx & 0x3f;
	*cyls = (uint16_t)(((cx >> 8) | ((cx & 0xc0) << 2)) + 1);
	*heads = (uint16_t)((dx >> 8) + 1);
	return 0;
}

int disk_select(uint8_t drive)
{
	struct edd_params p;
	struct disk_info *d = &g_disk_info;
	uint8_t ver = 0;
	uint16_t api = 0;

	d->drive = drive;
	d->edd = 0;
	d->api = 0;
	d->flags = 0;
	d->bytes_per_sector = DISK_SECTOR_SIZE;
	d->heads = d->spt = d->cyls = 0;
	g_drive_number = drive;
	d->mode = DISK_MODE_LBA;

	if (int13_ext_check(drive, &ver, &api) == 0) {
		d->edd = ver;
		d->api = api;
	}
#ifdef DISK_FI_NOEXT
	/* Test builds only: a BIOS without INT 13h extensions. */
	d->edd = 0;
#endif
	if (d->edd && int13_get_params(drive, &p) == 0) {
		d->flags = p.flags;
		if (p.bytes_per_sector)
			d->bytes_per_sector = p.bytes_per_sector;
	}
#ifdef DISK_FI_SECTOR_SIZE
	d->bytes_per_sector = DISK_FI_SECTOR_SIZE; /* test builds only */
#endif
	/* No extensions reported: AH=42h may still work (the loader used it
	 * unconditionally before this probe existed); only if a real
	 * one-sector read of LBA 0 fails three times (with a reset between)
	 * is this a CHS-only BIOS. The probe reads into fat.c's staging
	 * buffer, unused this early. */
	if (!d->edd) {
		int t, ok = 0;

		for (t = 0; t < 3 && !ok; t++) {
			if (t)
				disk_int13_reset();
			ok = disk_int13_read_lba(0, 1, g_fat_io_buf_ptr) == 0;
		}
		if (!ok) {
			if (int13_get_geometry(drive, &d->heads, &d->spt, &d->cyls) != 0)
				return -1;
			d->mode = DISK_MODE_CHS;
		}
	}
	g_disk_bios_limit = d->mode == DISK_MODE_CHS ? d->spt : DISK_CHUNK_LIMIT;
	disk_set_chunk(disk_get_chunk());
	/* Reads stay in 512-byte units either way (see disk.h). */
	return d->bytes_per_sector == DISK_SECTOR_SIZE ? 0 : 1;
}

uint8_t disk_current(void)
{
	return g_drive_number;
}

void disk_init(uint8_t drive_number)
{
	(void)disk_select(drive_number);
}

/* INT 13h AH=02h, CHS: `count` sectors from `lba`, track by track. */
static int int13_read_chs(uint64_t lba, uint16_t count, uint8_t *buf)
{
	const struct disk_info *d = &g_disk_info;

	while (count > 0) {
		uint16_t c, left, n;
		uint8_t h, sec, failed;
		uint16_t ax, cx, dx, bx;

		if (disk_lba_to_chs(lba, d->heads, d->spt, d->cyls, &c, &h, &sec, &left) != 0)
			return 0x04; /* "sector not found": beyond what CHS can address */
		n = count < left ? count : left;
		ax = (uint16_t)(0x0200 | n);
		cx = (uint16_t)(((c & 0xff) << 8) | ((c >> 2) & 0xc0) | sec);
		dx = (uint16_t)((h << 8) | g_drive_number);
		bx = (uint16_t)(unsigned long)buf;
		/* ES:BX is the buffer: ES == DS everywhere in this stage's C code
		 * (stage2_entry.S sets both; every asm that changes ES restores
		 * it). No push/pop: `failed` may be addressed relative to %esp. */
		__asm__ __volatile__(
			"int $0x13\n\t"
			"setc %0\n\t"
			: "=m"(failed), "+a"(ax), "+b"(bx), "+c"(cx), "+d"(dx)
			:
			: "esi", "edi", "ebp", "cc", "memory"
		);
		if (failed)
			return (ax >> 8) ? (int)(ax >> 8) : 0xff;
		lba += n;
		buf += (uint32_t)n * DISK_SECTOR_SIZE;
		count -= n;
	}
	return 0;
}

/*
 * The "disk address packet" INT 0x13, AH=0x42 reads its arguments
 * from - exact on-disk/in-memory layout the BIOS service itself
 * defines (Ralf Brown's Interrupt List, INT 13h/AH=42h), not this
 * project's own choice.
 */
struct disk_address_packet {
	uint8_t size;         /* sizeof(this struct) = 0x10 */
	uint8_t reserved;     /* must be 0 */
	uint16_t num_blocks;  /* sectors to transfer */
	uint16_t buffer_offset;
	uint16_t buffer_segment;
	uint64_t start_lba;
} __attribute__((packed));

/*
 * Same guard as gpt.c's own GPT header struct - a wrong-sized
 * uint64_t here silently corrupts the on-the-wire DAP BIOS actually
 * reads (see cdrom_disk_int13.c's own header comment for the real
 * bug this class of mistake caused once, for the exact same struct
 * shape, on the CD-ROM boot path).
 */
_Static_assert(sizeof(struct disk_address_packet) == 16,
               "disk_address_packet must be exactly 16 bytes (BIOS AH=0x42 DAP format)");

#ifdef DISK_FI_MODE
/*
 * Test builds only: a BIOS that returns CF=0 with wrong data. Applied to
 * a read that succeeded for real, when it matches the trigger:
 *   DISK_FI_ABOVE=N      every transfer of more than N sectors
 *   DISK_FI_LBA=X        every transfer that covers LBA X, the first
 *   DISK_FI_TIMES=T      T times (255 = always)
 *   DISK_FI_FROM=X,      every transfer that reaches into [X, Y]
 *   DISK_FI_TO=Y         (always; a whole file's worth of bad blocks)
 *   DISK_FI_DRIVE=D      only on BIOS drive D (default: any)
 * DISK_FI_MODE: 1 one bit flipped, 2 a whole sector of zeros, 3 stale
 * data (the buffer is left as it was: nothing transferred), 4 the data of
 * the wrong LBA (lba+1), 5 a short transfer with the DAP count unchanged
 * (the last sector stale). bios/tests/blkverify_host_test.c covers the
 * same kinds on the host.
 */
#ifndef DISK_FI_TIMES
#define DISK_FI_TIMES 255
#endif
static uint8_t g_fi_hits;

/* The sector (index within the transfer) to damage, or -1. */
static int fi_target(uint64_t lba, uint16_t count)
{
#ifdef DISK_FI_DRIVE
	if (g_drive_number != DISK_FI_DRIVE)
		return -1;
#endif
#ifdef DISK_FI_ABOVE
	if (count > DISK_FI_ABOVE)
		return count / 2;
#endif
#ifdef DISK_FI_FROM
	if (lba <= (uint64_t)DISK_FI_TO && lba + count > (uint64_t)DISK_FI_FROM)
		return lba >= (uint64_t)DISK_FI_FROM ? 0 : (int)((uint64_t)DISK_FI_FROM - lba);
#endif
#ifdef DISK_FI_LBA
	if ((uint64_t)DISK_FI_LBA >= lba && (uint64_t)DISK_FI_LBA < lba + count &&
	    (DISK_FI_TIMES == 255 || g_fi_hits < DISK_FI_TIMES)) {
		g_fi_hits++;
		return (int)((uint64_t)DISK_FI_LBA - lba);
	}
#endif
	(void)lba;
	(void)count;
	return -1;
}
#endif

int disk_int13_read(uint64_t lba, uint16_t count, void *buf)
{
#ifdef DISK_FI_MODE
	int t = fi_target(lba, count);

	if (t >= 0) {
		uint8_t *p = (uint8_t *)buf + (uint32_t)t * DISK_SECTOR_SIZE;
		int st;
		unsigned i;

		if (DISK_FI_MODE == 3)
			return 0;                                         /* nothing transferred */
		if (DISK_FI_MODE == 4)
			return disk_int13_read_lba(lba + 1, count, buf);  /* the wrong LBA */
		if (DISK_FI_MODE == 5)
			return count > 1 ? disk_int13_read_lba(lba, (uint16_t)(count - 1), buf) : 0;
		st = disk_int13_read_lba(lba, count, buf);
		if (st)
			return st;
		if (DISK_FI_MODE == 1)
			p[123] ^= 0x10;
		else
			for (i = 0; i < DISK_SECTOR_SIZE; i++)
				p[i] = 0;
		return 0;
	}
#endif
	return disk_int13_read_lba(lba, count, buf);
}

static int disk_int13_read_lba(uint64_t lba, uint16_t count, void *buf)
{
	struct disk_address_packet dap;
	uint16_t buf_off, dap_off, ds_seg;
	uint8_t failed;
	uint16_t ax;

	if (g_disk_info.mode == DISK_MODE_CHS)
		return int13_read_chs(lba, count, (uint8_t *)buf);

	/*
	 * buf is a plain near pointer into this stage's own data area -
	 * under -m16, GCC's pointers are still 32-bit values, but only
	 * the low 16 bits matter here (this stage's whole code+data
	 * fits well under 64KB - see link.ld's own layout comment), which
	 * is exactly the real-mode segment:offset split this BIOS service
	 * itself expects: the segment half is simply the CURRENT %ds
	 * (stage2_entry.S sets %ds once, before calling into any C code
	 * here, to cover this stage's whole data area - see its own
	 * comment), read directly here rather than assumed to be some
	 * fixed constant.
	 */
	buf_off = (uint16_t)(unsigned long)buf;
	dap_off = (uint16_t)(unsigned long)&dap;
	__asm__ __volatile__("mov %%ds, %0" : "=r"(ds_seg));

#ifdef DISK_FAULT_INJECT_MAX_SECTORS
	/*
	 * Test builds only (-DDISK_FAULT_INJECT_MAX_SECTORS=N, never set by the
	 * release build): behave like a BIOS that rejects any transfer above N
	 * sectors with status 0x09, which is the failure mode this loader's
	 * chunking exists for. tests/bios-hdd-int13-limit-test.sh boots such a
	 * build in QEMU, because QEMU's own SeaBIOS accepts every size.
	 */
	if (count > DISK_FAULT_INJECT_MAX_SECTORS)
		return 0x09;
#endif
#ifdef DISK_FI_NOEXT
	return 0x01; /* test builds only: AH=42h unsupported, see disk_select() */
#endif

	dap.size = sizeof(dap);
	dap.reserved = 0;
	dap.num_blocks = count;
	dap.buffer_offset = buf_off;
	dap.buffer_segment = ds_seg;
	dap.start_lba = lba;

	/*
	 * DS:SI -> the DAP, AH=0x42, DL = drive number, INT 0x13. Carry
	 * flag set on error (BIOS convention for every INT 13h service) -
	 * read via SETC into a plain byte rather than relying on the
	 * flag surviving back out to C, since nothing about inline asm's
	 * "cc" clobber promises flags are still readable after the asm
	 * block ends.
	 *
	 * "memory" clobber - NOT optional. This asm only takes `dap_off`
	 * (a plain integer address) as an operand, with no way for GCC to
	 * see that INT 0x13 itself reads the DAP bytes at that address -
	 * confirmed the hard way, by actually building this at the real
	 * -O2 the Makefile uses and disassembling the result: GCC's own
	 * alias analysis concluded every `dap.*` field store above was
	 * dead code (nothing "observably" read them) and deleted all six
	 * of them entirely, leaving SI pointing at whatever garbage
	 * happened to already be on the stack. A "memory" clobber tells
	 * GCC this asm may read/write memory it can't enumerate, which is
	 * exactly true here and is what makes it keep the stores - the
	 * `-O0` build (which happened to keep them anyway, since -O0
	 * barely optimizes at all) never actually exercised this bug,
	 * which is why it went unnoticed until built the same way the
	 * real Makefile does.
	 *
	 * "ebp" clobber (F18, unidoc-alip's PR #5 review): the same real
	 * hardware finding console.c's own console_putc() and e820.c's own
	 * INT 0x15 call already carry a clobber for - a BIOS's own
	 * interrupt handler is free to use, and not restore, EBP
	 * internally, a plain GPR here under -fomit-frame-pointer. "esi"
	 * is deliberately NOT also added - unlike e820.c's case, this asm
	 * already explicitly preserves it itself (push/pop around the
	 * call), a stronger guarantee than a clobber would add (the real
	 * original value survives, not just "GCC no longer trusts it").
	 * Confirmed to compile cleanly at this file's own real -O2 build
	 * flags and boot for real under QEMU (tests/bios-hdd-entry-test.sh)
	 * - not just reasoned about.
	 */
	/*
	 * dap_off is bound to %si explicitly ("+S": the BIOS may change SI, and
	 * declaring it in/out says so, which is why this no longer pushes and pops
	 * it). An earlier form took it in as a free "r" operand next to an "=a"
	 * output, and at -O2 GCC was free to put the DAP pointer in %eax - one
	 * build then fed the low half of the LBA to INT 13h as the pointer, which
	 * the fault-injection test caught. Nothing here may be left to the
	 * register allocator.
	 */
	{
		uint16_t si = dap_off;

		__asm__ __volatile__(
			"movb $0x42, %%ah\n\t"
			"int $0x13\n\t"
			"setc %0\n\t"
			: "=&q"(failed), "=a"(ax), "+S"(si)
			: "d"(g_drive_number)
			: "ebp", "cc", "memory"
		);
	}

	/*
	 * What the call's outcome means - carry set (the BIOS's own status
	 * in AH), or carry clear with a DAP that now reports fewer sectors
	 * than asked for - is decided in disk_int13_status() (disk_policy.c,
	 * host-tested). Not every real BIOS updates the DAP's own sector-count
	 * field to reflect a SHORT transfer (INT 13h/AH=42h's own documented
	 * behavior leaves this implementation-defined - the carry flag is the
	 * one universally-reliable signal), but on the ones that DO, this is
	 * a real, free extra check: safe either way, since dap.num_blocks was
	 * set to `count` as an INPUT before the call, so a BIOS that never
	 * touches it leaves the comparison trivially true. Reading it back
	 * here (rather than trusting the local `count` parameter) is what
	 * actually proves the read - a BIOS that reports success (CF clear)
	 * but silently transferred fewer sectors than requested used to be
	 * indistinguishable from a genuine full read.
	 */
	return disk_int13_status(failed, ax, dap.num_blocks, count);
}

void disk_reset(void)
{
	disk_int13_reset();
}

void disk_int13_reset(void)
{
	uint8_t drive = g_drive_number;

	/* AH=00h, DL=drive. The result is ignored: this is only ever the
	 * step before a retry, and the retry reports whether it worked. */
	__asm__ __volatile__(
		"xorb %%ah, %%ah\n\t"
		"int $0x13\n\t"
		: "+d"(drive)
		:
		: "eax", "ebp", "cc", "memory"
	);
}
