#include "stdint_local.h"

#include "bootparams.h"
#include "cmdline_opt.h"
#include "build_id.h"
#include "console.h"
#include "disk.h"
#include "e820.h"
#include "fat.h"
#include "gpt.h"
#include "mbr.h"
#include "blkverify.h"
#include "payload_sum.h"
#include "switch32.h"

/* Must match switch32.S's own SEG and link.ld's ORIGIN assumption -
 * every near pointer in this whole stage is an offset relative to
 * this exact segment; see switch32.S's own header comment.
 *
 * Overridable via -DSTAGE2_SEGMENT: the ISO/El-Torito build (see
 * Makefile's stage2-iso.bin target) loads at 0x7c0, not 0x1000 - the
 * real-mode segment El Torito's "no emulation" boot loads its image
 * at by convention (Load Segment = 0 means "default to 0x7c0",
 * physical address 0x7c00 - the same address a normal MBR boot sector
 * loads at), passed via --defsym to switch32.S/stage2_entry.S the
 * same way. */
#ifndef STAGE2_SEGMENT
#define STAGE2_SEGMENT 0x1000
#endif

/* 1MB - where the Linux/x86 boot protocol expects the protected-mode
 * kernel image, per Documentation/arch/x86/boot.rst. */
#define KERNEL_LOAD_ADDR 0x00100000u

/*
 * 64MB - generously clear of any realistically-sized kernel loaded at
 * 1MB (a real kernel+its own data is comfortably under 32MB), and
 * still well below every initrd_addr_max value this project has ever
 * seen a real kernel report (see the sanity check in stage2_main()
 * below, which catches it for real rather than just assuming this
 * holds).
 */
#define INITRD_LOAD_ADDR 0x04000000u

/*
 * Sanity ceilings on the kernel/initramfs files' own recorded sizes,
 * checked before ANY size-derived arithmetic below runs - not just
 * generous headroom over a realistic kernel+initrd, but a real
 * security boundary against a corrupt or hostile FAT directory entry.
 * MAX_KERNEL_SIZE is deliberately small enough that KERNEL_LOAD_ADDR +
 * MAX_KERNEL_SIZE stays well clear of INITRD_LOAD_ADDR - the two
 * destinations can never overlap as long as this holds.
 */
#define MAX_KERNEL_SIZE (48u * 1024 * 1024)
#define MAX_INITRD_SIZE (512u * 1024 * 1024)

/*
 * The kernel command line, read as a plain text file
 * (/EFI/ALPINE/CMDLINE - see fat_open()'s own namespace comment) -
 * generous for any realistic cmdline, with room left over for this
 * file's own defensive NUL terminator (and a stripped trailing
 * newline, a real hazard for a file that started life as a plain text
 * file rather than this project's own former packed-and-NUL-included
 * boot-blob format).
 */
#define CMDLINE_BUF_SIZE 512

/*
 * Confirmed the hard way, on real hardware: a disk read can come back
 * from disk_read_lba() reporting SUCCESS (no BIOS error at all - see
 * cdrom_disk.c's own retry loop, which only catches a BIOS-flagged
 * failure) while the actual CONTENT is still wrong. There's no lower
 * layer that can ever catch that - only a real content check
 * (signature/CRC/magic, whatever the specific read is validating) can
 * tell. Every read+validate step below that matters for booting at
 * all retries the WHOLE step (not just the raw disk_read_lba() call)
 * up to this many times before giving up for real.
 *
 * Deliberately NO delay between attempts, confirmed the OPPOSITE way
 * around from the usual "give it time to settle" instinct: a real
 * boot's own diagnostic output showed gpt_read_header() failing on
 * attempt N, then an immediate (zero-delay) extra read - issued right
 * after, for debugging - coming back with a byte-for-byte VALID
 * header (stored_crc == computed_crc), followed by attempt N+1 (after
 * this loop's own ~20M-iteration delay) failing again, identically.
 * Reads issued back-to-back succeeded; reads issued after a real
 * pause did not - a busy-wait delay here was actively counter-
 * productive, not neutral, on this specific virtual CD-ROM/BIOS
 * combination. Retrying as fast as possible is the fix.
 */
#define GPT_RETRY_ATTEMPTS 10

/*
 * boot_params is built only after the last disk read of this boot (the
 * kernel, initrd and cmdline are loaded and verified by then), so it lives
 * in fat.c's disk staging buffer instead of 4 KiB of its own: this stage's
 * code, data, .bss and stack share one 64 KiB segment, and the extra 4 KiB
 * is what keeps check-bss-bounds passing with margin on every compiler
 * (see Makefile). fat.c asserts the buffer is large enough. Nothing may
 * call fat_read_range() (or otherwise use that buffer) after the zeroing
 * loop near the end of stage2_main().
 */
#define g_boot_params g_fat_io_buf_ptr
static uint8_t g_cmdline[CMDLINE_BUF_SIZE];
static uint8_t g_header_buf[SETUP_HEADER_FILE_OFFSET + sizeof(struct setup_header)];
static struct boot_e820_entry g_e820[BOOT_PARAMS_E820_MAX_ENTRIES];

static const uint8_t esp_type_guid[16] = ZFSBOOT_ESP_TYPE_GUID_BYTES;

static uint32_t phys_of(const void *p)
{
	return (uint32_t)(STAGE2_SEGMENT << 4) + (uint32_t)(unsigned long)p;
}

/*
 * Diagnostic output (E820 map, every loader-owned physical range, the
 * kernel's relocation-related header fields, every failed BIOS read). Off
 * by default so a successful boot looks exactly as before; on when built
 * with -DBIOS_DIAG, or for one boot when the kernel command line in
 * EFI/ALPINE/CMDLINE contains the word alpine-zfsboot.diag=1. The command
 * line is only read after the FAT partition is mounted, so BIOS read
 * failures before that point are printed only by a -DBIOS_DIAG build.
 * Every line starts with "diag:"; ranges are printed start-end with the end
 * exclusive. Disk build only: the ISO stage has no room left for them (see
 * Makefile, check-bss-bounds - even a -DBIOS_DIAG ISO stage does not fit),
 * and the ISO path is not where the physical-disk failures were seen.
 */
#ifndef ZFSBOOT_ISO_BUILD
#define STAGE2_DIAG 1
#else
#define STAGE2_DIAG 0
#endif
#if STAGE2_DIAG && defined(BIOS_DIAG)
static uint8_t g_diag = 1;
#elif STAGE2_DIAG
static uint8_t g_diag;
#endif

static void die(const char *msg)
{
	console_puts("alpine-zfsboot-bios: FATAL: ");
	console_puts(msg);
	console_puts("\n");
	for (;;)
		__asm__ __volatile__("hlt");
}

/*
 * Failure reporting. The first real-hardware failure of this loader said
 * only "FAT read failed loading kernel", which covers a BIOS read error, a
 * FAT chain that ends before the file does, a loop and more - so now the
 * reason, the cluster, the BIOS status and the sector are printed first.
 */
struct disk_error g_disk_last_error;

static void puthex64(uint64_t v)
{
	static const char digits[] = "0123456789abcdef";
	int i;

	console_puts("0x");
	for (i = 60; i >= 0; i -= 4)
		console_putc(digits[(v >> i) & 0xf]);
}

void disk_notify_shrink(const struct disk_error *e, uint16_t new_chunk)
{
	console_puts("alpine-zfsboot-bios: BIOS refused a read (INT13 status ");
	console_puts_hex32(e->status);
	console_puts(", ");
	console_puts_hex32(e->count);
	console_puts(" sectors at LBA ");
	puthex64(e->lba);
	console_puts("); reading ");
	console_puts_hex32(new_chunk);
	console_puts(" sectors at a time from now on\n");
}

/*
 * The diagnostic/integrity helpers below run at most a few times per boot,
 * and this stage's code+data+stack share one 64 KiB segment (see Makefile,
 * check-bss-bounds): they are compiled for size, the rest of the file is
 * unchanged at -O2.
 */
#define SMALL __attribute__((noinline, optimize("Os")))

/* "alpine-zfsboot-bios: " + s, without repeating the prefix in every string. */
SMALL static void say(const char *s)
{
	console_puts("alpine-zfsboot-bios: ");
	console_puts(s);
}

/* v as exactly `digits` lowercase hex digits, no prefix. */
SMALL static void puthexn(uint64_t v, int digits)
{
	static const char hex[] = "0123456789abcdef";
	int i;

	for (i = (digits - 1) * 4; i >= 0; i -= 4)
		console_putc(hex[(v >> i) & 0xf]);
}

/* An address in 8 hex digits, or 16 if it does not fit in 32 bits. */
SMALL static void putaddr(uint64_t v)
{
	puthexn(v, (v >> 32) ? 16 : 8);
}

/* " name start-end" (end exclusive). */
SMALL static void putrange(const char *name, uint64_t start, uint64_t len)
{
	console_putc(' ');
	console_puts(name);
	console_putc(' ');
	putaddr(start);
	console_putc('-');
	putaddr(start + len);
}

#ifndef ZFSBOOT_ISO_BUILD
SMALL void disk_notify_failure(const struct disk_error *e, uint8_t attempt)
{
	if (!g_diag)
		return;
	console_puts("\ndiag: int13 fail lba ");
	puthexn(e->lba, 16);
	console_puts(" n ");
	puthexn(e->count, 4);
	console_puts(" try ");
	puthexn(attempt, 2);
	console_puts(" status ");
	puthexn(e->status, 4);
	console_puts("\n");
}
#endif

#if STAGE2_DIAG
SMALL static void diag_dump_e820(uint8_t n)
{
	uint8_t i;

	console_puts("diag: e820 ");
	puthexn(n, 2);
	console_puts(" entries (start+size type)\n");
	for (i = 0; i < n; i++) {
		console_putc(' ');
		puthexn(g_e820[i].addr, 16);
		console_putc('+');
		puthexn(g_e820[i].size, 16);
		console_putc(' ');
		puthexn(g_e820[i].type, 2);
		if ((i & 1) || i + 1 == n)
			console_puts("\n");
		else
			console_putc(' ');
	}
}



/* The diagnostic dump of every range the loader owns or writes, and the
 * kernel header fields that decide where the kernel goes. */
SMALL static void diag_dump_layout(uint8_t e820_count, const struct setup_header *h, uint32_t protected_mode_size,
                                   int have_run, uint64_t run_start, uint64_t run_len, uint32_t initrd_size,
                                   uint32_t cmdline_len)
{
	diag_dump_e820(e820_count);
	console_puts("diag:");
	putrange("stage2", (uint32_t)STAGE2_SEGMENT << 4, 0x10000);
	putrange("iobuf", phys_of(g_fat_io_buf_ptr), g_fat_io_buf_size);
	console_puts("\ndiag:");
	putrange("kernel", KERNEL_LOAD_ADDR, protected_mode_size);
	if (have_run)
		putrange("run", run_start, run_len);
	else
		console_puts(" run ?");
	console_puts(" proto ");
	puthexn(h->version, 4);
	console_puts("\ndiag:");
	putrange("initrd", INITRD_LOAD_ADDR, initrd_size);
	console_puts("\ndiag:");
	putrange("bootparams", phys_of(g_boot_params), BOOT_PARAMS_SIZE);
	putrange("cmdline", phys_of(g_cmdline), cmdline_len + 1);
	console_puts("\ndiag: code32 ");
	puthexn(h->code32_start, 8);
	console_puts(" align ");
	puthexn(h->kernel_alignment, 8);
	console_puts(" reloc ");
	puthexn(h->relocatable_kernel, 2);
	console_puts(" pref ");
	putaddr(h->pref_address);
	console_puts(" init_size ");
	puthexn(h->init_size, 8);
	console_puts("\n");
}
#endif

/*
 * Checks a destination range against the firmware's E820 map (see
 * e820_range.c) before anything is copied there. Deliberately
 * conservative - nothing here may stop a machine that booted with the
 * previous loader, which did no such check: only a range that runs above
 * the end of ALL reported RAM stops the boot (there is certainly no
 * memory to load into, so that boot could not have worked before
 * either). A range in a hole between RAM entries, or overlapping a
 * reserved/ACPI entry, prints a warning with the address and the map,
 * and the boot goes on exactly as before.
 */
SMALL static void check_ram_range(const char *what, uint64_t start, uint64_t len, uint8_t e820_count)
{
	uint64_t bad;
	int r = e820_range_check(g_e820, e820_count, start, len, &bad);

	if (r == E820_RANGE_OK)
		return;
	say("e820:");
	putrange(what, start, len);
	console_puts(r == E820_RANGE_RESERVED ? " overlaps a reserved range at " : " is not all RAM, first bad address ");
	putaddr(bad);
	console_puts("\n");
#if STAGE2_DIAG
	if (!g_diag)
		diag_dump_e820(e820_count);
#endif
	if (r == E820_RANGE_BEYOND_RAM)
		die("load destination is above the end of RAM (E820)");
	say("WARNING: loading there anyway\n");
}

static void report_fat_failure(void)
{
	console_puts("alpine-zfsboot-bios: fat: ");
	console_puts(fat_err_name(g_fat_error.reason));
	console_puts(" cluster=");
	console_puts_hex32(g_fat_error.cluster);
	if (g_fat_error.reason == FAT_ERR_CHAIN_END) {
		console_puts(" entry=");
		console_puts_hex32(g_fat_error.value);
	}
	console_puts(" bytes-left=");
	console_puts_hex32(g_fat_error.remaining);
	if (g_fat_error.lba != 0) {
		console_puts(" lba=");
		puthex64(g_fat_error.lba);
	}
	console_puts("\n");
	if (g_disk_last_error.status != 0) {
		console_puts("alpine-zfsboot-bios: int13: last failure status=");
		console_puts_hex32(g_disk_last_error.status);
		console_puts(" lba=");
		puthex64(g_disk_last_error.lba);
		console_puts(" sectors=");
		console_puts_hex32(g_disk_last_error.count);
		console_puts(" chunk=");
		console_puts_hex32(g_disk_last_error.chunk);
		console_puts("\n");
	}
}

static void die_fat(const char *msg)
{
	report_fat_failure();
	die(msg);
}

/*
 * a20_verify() - the classic, textbook A20 test (the same technique
 * GRUB/syslinux both use): with A20 gated OFF, physical address bit 20
 * is masked to 0, so a write to (some low address + 1MB) silently
 * ALIASES back onto that same low address instead of landing 1MB away
 * - this writes a known pattern to a low probe variable, then uses the
 * ALREADY-PROVEN unreal_copy() to write a DIFFERENT pattern to the
 * address exactly 1MB above it, then checks whether the low probe's
 * own value changed. If A20 is truly enabled, the high write lands at
 * a genuinely different physical address and the low probe is
 * untouched; if A20 is still gated off, the high write silently
 * corrupts the low probe instead.
 *
 * A full source audit found enable_a20() (switch32.S) tried both the
 * documented BIOS service (INT 15h/AX=2401h) and the port-0x92 fast-
 * A20 fallback, but never actually verified either one WORKED before
 * returning - on a real chipset where neither method is honored (rare,
 * but the entire reason this file's own comment calls the two methods
 * "belt-and-suspenders" rather than assuming either one alone is
 * enough), every subsequent unreal_copy() to a >=1MB destination
 * (KERNEL_LOAD_ADDR is exactly 1MB) would silently wrap back down to
 * physical address 0, corrupting the IVT/BDA and leaving the kernel
 * never actually loaded where the CPU is told to jump to - a silent,
 * catastrophic failure with no error message at all, the single worst
 * possible outcome this loader can produce. This closes that gap: if
 * A20 genuinely isn't enabled, die() here with a real, actionable
 * message instead of proceeding into silent memory corruption.
 *
 * phys_of(), not a bare pointer cast - low_probe's REAL physical
 * address must account for this stage's own nonzero segment base
 * (STAGE2_SEGMENT<<4), exactly the same reasoning phys_of()'s own
 * existing callers already depend on; a bare cast would only give the
 * in-segment offset and silently test the wrong pair of addresses.
 */
static int a20_verify(void)
{
	static volatile uint32_t low_probe = 0x58601216u; /* an arbitrary, non-zero, non-repeating pattern */
	uint32_t overwrite = 0xa1cf9e73u;                  /* a different arbitrary pattern */
	uint32_t high_phys = phys_of((const void *)&low_probe) + 0x00100000u;

	unreal_copy(high_phys, &overwrite, sizeof(overwrite));

	return low_probe == 0x58601216u;
}

/* One dot per MiB actually transferred - cheap, real feedback on a
 * large FAT read (a 50+MB initrd over real BIOS disk I/O takes long
 * enough that silent output reads as a hang, not progress). fat.c
 * itself has no notion of "a MiB" or "a dot" - this is deliberately
 * the one place that policy lives (see fat.h's own fat_progress_fn
 * comment); fat_read_range() just reports raw byte counts as they
 * complete. */
#define PROGRESS_DOT_BYTES (1024u * 1024u)

struct progress_state {
	uint32_t since_dot;
};

static void progress_dot(void *ctx, uint32_t bytes_done)
{
	struct progress_state *st = (struct progress_state *)ctx;

	st->since_dot += bytes_done;
	while (st->since_dot >= PROGRESS_DOT_BYTES) {
		console_putc('.');
		st->since_dot -= PROGRESS_DOT_BYTES;
	}
}

/*
 * Payload integrity (see payload_sum.h): the expected SHA-256 of the bytes
 * that end up in RAM, recorded at install/update time (and by iso.sh for
 * the ISO), plus the per-block table in BLKSUM (blkverify.h) that lets a
 * block the BIOS delivered wrong be re-read as it is loaded.
 */
static struct payload_sum g_sum;
static uint8_t g_sum_loaded;
/* The manifest text is parsed into g_sum before the kernel header is read
 * into g_header_buf, so it borrows that buffer instead of its own. */
_Static_assert(sizeof(g_header_buf) >= PAYLOAD_SUM_MAX_BYTES, "g_header_buf too small for CHECKSUM");

SMALL static void payload_sum_load(const struct fat_volume *vol)
{
	struct fat_file f;
	enum payload_sum_result r = PAYLOAD_SUM_MALFORMED;
	int i;

	/* One lookup, no retry loop: on an install from before this file
	 * existed, "not found" is the normal answer on every boot. (An
	 * extension-less name on purpose: fat.c resolves 8-character short
	 * names without extensions only.) */
	if (fat_open(vol, PAYLOAD_SUM_PATH, &f) != 0) {
		say("no EFI/ALPINE/CHECKSUM - kernel/initrd not verified\n");
		return;
	}
	if (f.size == 0 || f.size > PAYLOAD_SUM_MAX_BYTES) {
		say("EFI/ALPINE/CHECKSUM has an unexpected size - not verified\n");
		return;
	}
	/* Read + parse is retried as one step, like every other read+validate
	 * step in this file (see GPT_RETRY_ATTEMPTS). */
	for (i = 0; i < GPT_RETRY_ATTEMPTS; i++) {
		if (fat_read_range(vol, &f, 0, f.size, phys_of(g_header_buf), 0, 0) != 0) {
			r = PAYLOAD_SUM_MALFORMED;
			continue;
		}
		r = payload_sum_parse((const char *)g_header_buf, f.size, &g_sum);
		if (r == PAYLOAD_SUM_OK || r == PAYLOAD_SUM_UNSUPPORTED)
			break;
	}
	if (r == PAYLOAD_SUM_UNSUPPORTED) {
		say("EFI/ALPINE/CHECKSUM is a newer format - not verified\n");
		return;
	}
	if (r != PAYLOAD_SUM_OK) {
		say("EFI/ALPINE/CHECKSUM unreadable or malformed - not verified\n");
		return;
	}
	g_sum_loaded = 1;
}

/* Before loading: is the file on the FAT still the one the manifest was
 * written for? A mismatch means a stale manifest or a changed file: in
 * mode enforce the boot stops here (it would fail the digest check after
 * a long load anyway); otherwise a warning, and the boot goes on without
 * hashing. */
SMALL static void payload_sum_check_file(const char *name, const struct payload_sum_entry *e, uint32_t size, uint32_t offset)
{
	enum payload_sum_result r;

	if (!g_sum_loaded)
		return;
	r = payload_sum_check_layout(e, size, offset);
	if (r == PAYLOAD_SUM_OK)
		return;
	/* `alpine-zfsboot verify` prints the numbers; no room for them here. */
	say("integrity: ");
	console_puts(name);
	console_puts(r == PAYLOAD_SUM_SIZE ? " size" : " offset");
	console_puts(" differs from CHECKSUM\n");
	if (g_sum.mode == PAYLOAD_SUM_MODE_ENFORCE)
		die("KERNEL/INITRD do not match CHECKSUM, not booting");
	/* Not enforced (see payload_sum.h): say so and boot, unverified. */
	say("WARNING: CHECKSUM is stale or the file changed - booting unverified\n");
	g_sum_loaded = 0;
}

SMALL static void putdigest(const uint8_t d[32])
{
	int i;

	for (i = 0; i < 32; i++)
		puthexn(d[i], 2);
}

/*
 * After loading: SHA-256 of the bytes really in RAM (read back through
 * unreal_read(), not the staging buffer they passed through), compared
 * with the manifest. Returns nonzero on a mismatch, after printing both
 * digests and where the range lies.
 */
SMALL static int payload_sum_verify(const char *name, const struct payload_sum_entry *e, uint32_t phys, uint32_t len,
                              uint32_t *since_dot)
{
	struct sha256_ctx c;
	uint8_t got[32];
	uint32_t p = phys, left = len;

	sha256_init(&c);
	while (left > 0) {
		uint32_t n = left < g_fat_io_buf_size ? left : g_fat_io_buf_size;

		unreal_read(g_fat_io_buf_ptr, p, n);
		sha256_update(&c, g_fat_io_buf_ptr, n);
		p += n;
		left -= n;
		*since_dot += n;
		while (*since_dot >= 4u * PROGRESS_DOT_BYTES) {
			console_putc('.');
			*since_dot -= 4u * PROGRESS_DOT_BYTES;
		}
	}
	sha256_final(&c, got);
	if (payload_sum_digest_cmp(got, e->sha256) == 0)
		return 0;

	console_puts("\n");
	say("integrity: loaded ");
	console_puts(name);
	console_puts(" does not match CHECKSUM\n");
	console_puts("  want ");
	putdigest(e->sha256);
	console_puts("\n  got  ");
	putdigest(got);
	console_puts("\n ");
	putrange("RAM", phys, len);
	console_puts(" = file bytes ");
	puthexn(e->offset, 8);
	console_putc('-');
	puthexn(e->size, 8);
	console_puts("\n");
	return 1;
}

/*
 * The memory-map checks before loading (see stage2_main()): reads the E820
 * map, dumps the layout in diagnostic mode, and checks every fixed
 * destination against the map. Returns the number of E820 entries.
 */
SMALL static uint8_t check_layout(const struct setup_header *kernel_hdr, uint32_t protected_mode_size,
                                  uint32_t initrd_size, uint32_t cmdline_len)
{
	uint8_t e820_count;
	uint64_t run_start = 0, run_len = 0;
	int have_run;

	(void)cmdline_len; /* diagnostics only; the ISO stage has none */
	e820_count = e820_get_map(g_e820, BOOT_PARAMS_E820_MAX_ENTRIES);
	if (e820_count == 0)
		die("E820 memory map query failed - BIOS too old?");
	have_run = kernel_runtime_range(kernel_hdr, KERNEL_LOAD_ADDR, &run_start, &run_len) == 0;

#if STAGE2_DIAG
	if (g_diag)
		diag_dump_layout(e820_count, kernel_hdr, protected_mode_size, have_run, run_start, run_len,
		                 initrd_size, cmdline_len);
#endif

	check_ram_range("kernel", KERNEL_LOAD_ADDR, protected_mode_size, e820_count);
	if (have_run)
		check_ram_range("kernel-runtime", run_start, run_len, e820_count);
	check_ram_range("initrd", INITRD_LOAD_ADDR, initrd_size, e820_count);
	if (have_run && e820_ranges_overlap(run_start, run_len, INITRD_LOAD_ADDR, initrd_size)) {
		console_puts("alpine-zfsboot-bios:");
		putrange("kernel-runtime", run_start, run_len);
		putrange("overlaps initrd", INITRD_LOAD_ADDR, initrd_size);
		console_puts("\n");
		/* Not certain to fail (the decompressor writes only part of that
		 * area early), and the previous loader booted it: warn only. */
		say("WARNING: booting anyway\n");
	}
	return e820_count;
}

/* The integrity check right before the jump (see stage2_main()). */
SMALL static void verify_loaded(uint32_t protected_mode_size, uint32_t initrd_size)
{
	if (g_sum_loaded) {
		uint32_t since_dot = 0;
		int bad;

		say("verifying kernel+initrd ");
		bad = payload_sum_verify("KERNEL", &g_sum.kernel, KERNEL_LOAD_ADDR, protected_mode_size, &since_dot);
		bad |= payload_sum_verify("INITRD", &g_sum.initrd, INITRD_LOAD_ADDR, initrd_size, &since_dot);
		if (bad && g_sum.mode == PAYLOAD_SUM_MODE_ENFORCE) {
			console_puts("\n");
			die("kernel/initrd differs from CHECKSUM, not booting it");
		}
		if (bad)
			say("WARNING: differs from CHECKSUM - booting anyway\n");
		else
			console_puts(" ok\n");
	}
}

/*
 * BLKSUM: the per-block table (blkverify.h). Used only together with a
 * CHECKSUM it agrees with (same sizes, offsets and SHA-256s - a BLKSUM left
 * over from another payload is ignored), so CHECKSUM's mode governs both.
 */
static struct blksum_hdr g_blk;
static uint8_t g_blk_loaded;
static struct fat_file g_blk_file;
static const struct fat_volume *g_blk_vol;
static uint8_t g_blk_sec[BLKSUM_SECTOR];
static uint32_t g_blk_sec_idx = 0xffffffffu;
static uint8_t g_blk_table_bad;

/* Table sector `idx` into g_blk_sec, checked against its own hash; a bad
 * read is retried like a payload block (reset, smaller transfer is moot
 * for one sector). 0 or -1. */
SMALL static int blksum_sector(uint32_t idx)
{
	int t;

	if (g_blk_sec_idx == idx)
		return 0;
	for (t = 0; t <= BLKV_TRIES; t++) {
		if (t) {
			disk_reset();
			fat_cache_invalidate();
		}
		if (fat_read_range(g_blk_vol, &g_blk_file, idx * BLKSUM_SECTOR, BLKSUM_SECTOR, phys_of(g_blk_sec), 0, 0) == 0 &&
		    (idx == 0 || blksum_check_sector(g_blk_sec) == 0)) {
			g_blk_sec_idx = idx;
			return 0;
		}
	}
	g_blk_sec_idx = 0xffffffffu;
	return -1;
}

SMALL static void blksum_load(const struct fat_volume *vol)
{
	int r = BLKSUM_BAD, t, i;

	if (!g_sum_loaded)
		return; /* no CHECKSUM, or mode off: BLKSUM is not used either */
	if (fat_open(vol, BLKSUM_PATH, &g_blk_file) != 0) {
		say("no EFI/ALPINE/BLKSUM - blocks not checked as they load\n");
		return;
	}
	g_blk_vol = vol;
	for (t = 0; t < GPT_RETRY_ATTEMPTS && r == BLKSUM_BAD; t++) {
		g_blk_sec_idx = 0xffffffffu;
		if (blksum_sector(0) == 0)
			r = blksum_parse_header(g_blk_sec, &g_blk);
	}
	g_blk_sec_idx = 0xffffffffu;
	if (r == BLKSUM_UNSUPPORTED) {
		say("EFI/ALPINE/BLKSUM is a newer format - blocks not checked\n");
		return;
	}
	if (r != BLKSUM_OK) {
		say("EFI/ALPINE/BLKSUM unreadable or malformed - blocks not checked\n");
		return;
	}
	for (i = 0; i < 2; i++) {
		const struct payload_sum_entry *e = i ? &g_sum.initrd : &g_sum.kernel;
		const struct blksum_file *f = &g_blk.f[i];

		if (f->size != e->size || f->offset != e->offset || payload_sum_digest_cmp(f->sha256, e->sha256) != 0) {
			say("EFI/ALPINE/BLKSUM does not match CHECKSUM - blocks not checked\n");
			return;
		}
	}
	g_blk_loaded = 1;
}

/* One load (KERNEL or INITRD) in progress: the progress dots plus, with a
 * BLKSUM, the per-block checks (blkverify.h) as the data lands. */
struct load_ctx {
	struct progress_state st;
	struct blkv bv;
	const struct fat_volume *vol;
	const struct fat_file *file;
	uint32_t file_off; /* where the RAM range starts in the file */
	uint8_t which;     /* 0 KERNEL, 1 INITRD */
	uint8_t checking;
};

static struct load_ctx g_load[2];

static void load_progress(void *ctx, uint32_t bytes_done)
{
	struct load_ctx *lc = (struct load_ctx *)ctx;

	progress_dot(&lc->st, bytes_done);
	if (lc->checking)
		blkv_landed(&lc->bv, bytes_done);
}

/* blkverify hooks - see blkverify.h. */
SMALL static int hook_reread(struct blkv *b, uint32_t off, uint32_t len)
{
	struct load_ctx *lc = (struct load_ctx *)b->ctx;

	fat_cache_invalidate(); /* a cached FAT sector may itself be what the BIOS got wrong */
	return fat_read_range(lc->vol, lc->file, lc->file_off + off, len, b->base + off, 0, 0);
}

SMALL static int hook_entry(struct blkv *b, uint32_t i, uint8_t out[32])
{
	struct load_ctx *lc = (struct load_ctx *)b->ctx;
	uint32_t slot = i % BLKSUM_PER_SECTOR, k;

	if (blksum_sector(g_blk.f[lc->which].first + i / BLKSUM_PER_SECTOR) != 0) {
		g_blk_table_bad = 1;
		return -1;
	}
	for (k = 0; k < 32; k++)
		out[k] = g_blk_sec[slot * 32 + k];
	return 0;
}

/* The transfer size for a re-read: the chunk in use changes, the
 * configured cap (what the summary reports as "cap") does not. */
SMALL static void hook_set_cap(uint16_t n)
{
#ifndef ZFSBOOT_ISO_BUILD
	uint16_t cap = g_disk_stats.cap;

	disk_set_chunk(n);
	g_disk_stats.cap = cap;
#else
	disk_set_chunk(n);
#endif
}

SMALL static void putdigest(const uint8_t d[32]);

/* The first block of the boot that stayed wrong, for the summary. */
static struct {
	uint8_t set, which;
	uint32_t off, lba;
	uint8_t want[6], got[6];
} g_first_bad;

SMALL static void hook_report(struct blkv *b, uint32_t block, const uint8_t want[32], const uint8_t got[32], uint8_t tries)
{
	struct load_ctx *lc = (struct load_ctx *)b->ctx;
	uint32_t off = lc->file_off + block * BLKSUM_BLOCK;
	uint64_t lba = 0;
	int k;

	fat_cache_invalidate();
	fat_offset_lba(lc->vol, lc->file, off, &lba);
	if (!g_first_bad.set) {
		g_first_bad.set = 1;
		g_first_bad.which = lc->which;
		g_first_bad.off = off;
		g_first_bad.lba = (uint32_t)lba;
		/* volatile: there is no libc here, and GCC would turn this loop
		 * into a memmove() call otherwise. */
		for (k = 0; k < 6; k++) {
			((volatile uint8_t *)g_first_bad.want)[k] = want[k];
			((volatile uint8_t *)g_first_bad.got)[k] = got[k];
		}
	}
	console_puts("\n");
	say("integrity: ");
	console_puts(lc->which ? "INITRD" : "KERNEL");
	console_puts(" block ");
	puthexn(block, 4);
	console_puts(" stayed wrong after ");
	puthexn(tries, 2);
	console_puts(" re-reads\n  file offset ");
	puthexn(off, 8);
	console_puts(" lba ");
	puthexn(lba, 10);
	console_puts("\n  want ");
	putdigest(want);
	console_puts("\n  got  ");
	putdigest(got);
	console_puts("\n");
}

#ifndef ZFSBOOT_ISO_BUILD
/*
 * The second source: another BIOS hard disk (0x80.. as many as the BIOS
 * data area counts) that carries an alpine-zfsboot ESP whose CHECKSUM
 * describes exactly the same KERNEL and INITRD (sizes, offsets, SHA-256)
 * - a mirror member. Looked for only when a block could not be healed;
 * whatever it serves is checked against the table like any other read.
 */
static struct {
	uint8_t probed, drive;
	struct fat_volume vol;
	struct fat_file f[2];
} g_alt;
static uint8_t g_boot_drive;

/* Back to the boot drive after using another one. */
SMALL static void alt_leave(uint16_t chunk)
{
	disk_select(g_boot_drive);
	disk_set_chunk(chunk);
	fat_cache_invalidate();
}

SMALL static int alt_try(uint8_t d)
{
	struct gpt_header hdr;
	struct fat_file sum;
	struct payload_sum ps;
	uint64_t lba, sectors;

	if (disk_select(d) != 0)
		return -1; /* also a drive that reports non-512-byte sectors: not trusted as a second source */
	fat_cache_invalidate();
	if (gpt_read_header(&hdr) == 0) {
		if (gpt_find_partition(&hdr, esp_type_guid, &lba, &sectors) != 0)
			return -1;
	} else if (mbr_find_partition(ZFSBOOT_FAT_MBR_TYPE, &lba, &sectors) != 0) {
		return -1;
	}
	if (fat_mount(&g_alt.vol, lba, sectors) != 0 || fat_open(&g_alt.vol, "/EFI/ALPINE/KERNEL", &g_alt.f[0]) != 0 ||
	    fat_open(&g_alt.vol, "/EFI/ALPINE/INITRD", &g_alt.f[1]) != 0 || fat_open(&g_alt.vol, PAYLOAD_SUM_PATH, &sum) != 0 ||
	    sum.size == 0 || sum.size > PAYLOAD_SUM_MAX_BYTES ||
	    fat_read_range(&g_alt.vol, &sum, 0, sum.size, phys_of(g_header_buf), 0, 0) != 0 ||
	    payload_sum_parse((const char *)g_header_buf, sum.size, &ps) != PAYLOAD_SUM_OK)
		return -1;
	if (ps.kernel.size != g_sum.kernel.size || ps.kernel.offset != g_sum.kernel.offset ||
	    payload_sum_digest_cmp(ps.kernel.sha256, g_sum.kernel.sha256) != 0 || ps.initrd.size != g_sum.initrd.size ||
	    payload_sum_digest_cmp(ps.initrd.sha256, g_sum.initrd.sha256) != 0 || g_alt.f[0].size != g_sum.kernel.size ||
	    g_alt.f[1].size != g_sum.initrd.size)
		return -1;
	return 0;
}

SMALL static int hook_second(struct blkv *b, uint32_t off, uint32_t len)
{
	struct load_ctx *lc = (struct load_ctx *)b->ctx;
	uint16_t chunk = disk_get_chunk();
	int r = -1;

	if (!g_alt.probed) {
		uint8_t n = 0, d;

		g_alt.probed = 1;
		unreal_read(&n, 0x475, 1); /* BIOS data area: number of hard disks */
		for (d = 0x80; d < 0x80 + n && d < 0x90; d++) {
			if (d == g_boot_drive)
				continue;
			if (alt_try(d) == 0) {
				g_alt.drive = d;
				break;
			}
		}
		alt_leave(chunk);
	}
	if (!g_alt.drive)
		return -1;
	if (disk_select(g_alt.drive) == 0) {
		fat_cache_invalidate();
		disk_set_chunk(chunk);
		r = fat_read_range(&g_alt.vol, &g_alt.f[lc->which], lc->file_off + off, len, b->base + off, 0, 0);
	}
	alt_leave(chunk);
	if (g_diag) {
		console_puts("\ndiag: second source drive ");
		puthexn(g_alt.drive, 2);
		console_puts(lc->which ? " INITRD" : " KERNEL");
		console_puts(" block ");
		puthexn(off / BLKSUM_BLOCK, 4);
		console_puts("\n");
	}
	return r;
}
#endif

static const struct blkv_hooks g_blkv_hooks = {
	hook_reread,
#ifndef ZFSBOOT_ISO_BUILD
	hook_second,
#else
	0,
#endif
	hook_entry,
	unreal_read,
	disk_get_chunk,
	hook_set_cap,
	disk_reset,
	hook_report,
};

/* Loads one file's RAM range, checking every block as it lands when there
 * is a BLKSUM; returns what fat_read_range() returns. */
SMALL static int load_file(uint8_t which, const struct fat_volume *vol, const struct fat_file *file, uint32_t file_off,
                           uint32_t len, uint32_t dst)
{
	struct load_ctx *lc = &g_load[which];

	lc->vol = vol;
	lc->file = file;
	lc->file_off = file_off;
	lc->which = which;
	/* g_sum_loaded too: a CHECKSUM found stale after the kernel header was
	 * read (warn mode) turns all checking off, BLKSUM's included. */
	lc->checking = g_blk_loaded && g_sum_loaded;
	if (lc->checking)
		blkv_start(&lc->bv, &g_blkv_hooks, lc, dst, len, g_blk.f[which].nblocks, g_fat_io_buf_ptr, g_fat_io_buf_size);
	if (fat_read_range(vol, file, file_off, len, dst, load_progress, lc) != 0)
		return -1;
	if (lc->checking && lc->bv.unreported) {
		console_puts("\n");
		say("integrity: ");
		puthexn(lc->bv.unreported, 4);
		console_puts(which ? " more INITRD" : " more KERNEL");
		console_puts(" blocks wrong, not re-read\n");
	}
	if (lc->checking && lc->bv.bad && g_sum.mode == PAYLOAD_SUM_MODE_ENFORCE) {
		console_puts("\n");
		die("integrity enforce: a block stayed wrong, not booting");
	}
	return 0;
}

/*
 * The boot summary, just before "starting kernel": printed whenever
 * anything had to be retried, healed, served by another drive, or stayed
 * wrong - even without diagnostics, so one screenshot or serial capture
 * shows it - and on every boot in diagnostic mode. Two lines (three when
 * a block stayed wrong: the first such block), each under 80 columns, all
 * numbers hex; diagnostic mode adds one line of BIOS details.
 */
SMALL static void boot_summary(void)
{
	uint16_t mism = 0, healed = 0, second = 0, bad = 0, retries = 0;
	int i, event;

	for (i = 0; i < 2; i++) {
		mism += g_load[i].bv.mismatches;
		healed += g_load[i].bv.healed;
		second += g_load[i].bv.second;
		bad += g_load[i].bv.bad;
		retries += g_load[i].bv.retries;
	}
#ifndef ZFSBOOT_ISO_BUILD
	event = mism || g_disk_stats.failures || g_blk_table_bad || g_diag ||
	        g_disk_info.bytes_per_sector != DISK_SECTOR_SIZE;
	if (!event)
		return;
	console_puts("summary: drive ");
	puthexn(g_disk_info.drive, 2);
	console_puts(g_disk_info.mode == DISK_MODE_CHS ? " chs" : " lba");
	console_puts(" bps ");
	puthexn(g_disk_info.bytes_per_sector, 4);
	console_puts(" cap ");
	puthexn(g_disk_stats.cap, 4);
	console_puts(" now ");
	puthexn(disk_get_chunk(), 4);
	console_puts(" int13 ");
	puthexn(g_disk_stats.calls, 8);
	console_puts(" fail ");
	puthexn(g_disk_stats.failures, 4);
	console_puts("\n");
	if (g_diag) {
		console_puts("summary: edd ");
		puthexn(g_disk_info.edd, 2);
		console_puts(" api ");
		puthexn(g_disk_info.api, 4);
		console_puts(" flags ");
		puthexn(g_disk_info.flags, 4);
		console_puts(" limit ");
		puthexn(g_disk_bios_limit, 4);
		console_puts(" largest ");
		puthexn(g_disk_stats.largest, 4);
		console_puts("\n");
	}
#else
	event = mism || g_blk_table_bad;
	if (!event)
		return;
	console_puts(disk_cd_atapi() ? "summary: cd atapi cap " : "summary: cd int13 cap ");
	puthexn(disk_get_chunk(), 4);
	console_puts("\n");
#endif
	console_puts(g_blk_loaded ? "summary: blocks mism " : "summary: blocks NOT checked mism ");
	puthexn(mism, 4);
	console_puts(" reread ");
	puthexn(retries, 4);
	console_puts(" healed ");
	puthexn(healed, 4);
	console_puts(" 2nd ");
	puthexn(second, 4);
	console_puts(" bad ");
	puthexn(bad, 4);
	if (g_blk_table_bad)
		console_puts(" table-bad");
	console_puts("\n");
	if (g_first_bad.set) {
		/* "bad KERNEL @<file offset> lba <lba> exp <want> got <got>" -
		 * the first 6 bytes of each SHA-256 state (blkverify.h). */
		console_puts(g_first_bad.which ? "summary: bad INITRD @" : "summary: bad KERNEL @");
		puthexn(g_first_bad.off, 8);
		console_puts(" lba ");
		puthexn(g_first_bad.lba, 8);
		console_puts(" exp ");
		for (i = 0; i < 6; i++)
			puthexn(g_first_bad.want[i], 2);
		console_puts(" got ");
		for (i = 0; i < 6; i++)
			puthexn(g_first_bad.got[i], 2);
		console_puts("\n");
	}
}

/* Options from the command line (diagnostics, INT 13h transfer cap,
 * integrity mode) and the CHECKSUM manifest, before anything large is
 * loaded (see stage2_main()). */
SMALL static void read_options(const struct fat_volume *vol, uint32_t kernel_size, uint32_t initrd_size)
{
	(void)vol;
	(void)kernel_size;
	(void)initrd_size;
	{
		/* alpine-zfsboot.int13chunk=N: the INT 13h transfer cap for every
		 * read from here on (disk.h). Said on screen when set, so an A/B
		 * test on a failing machine shows which value a boot used. */
		uint32_t n;
		const char *v = cmdline_value((const char *)g_cmdline, "alpine-zfsboot.int13chunk=", &n);

		if (v) {
			int c = cmdline_parse_chunk(v, n, DISK_CHUNK_LIMIT);

			if (c < 0) {
				say("alpine-zfsboot.int13chunk= ignored (not 1..127)\n");
			} else {
				disk_set_chunk((uint16_t)c);
				say("int13chunk: transfers of at most ");
				console_puts_hex32((unsigned long)c);
				console_puts(" sectors\n");
			}
		}
	}
#if STAGE2_DIAG
	{
		uint32_t n;
		const char *v = cmdline_value((const char *)g_cmdline, "alpine-zfsboot.diag=", &n);

		if (v && n == 1 && *v == '1')
			g_diag = 1;
	}
	if (g_diag)
		console_puts("diag: diagnostics on\n");
#endif

	payload_sum_load(vol);
	if (g_sum_loaded) {
		/* alpine-zfsboot.integrity=off|warn|enforce on the command line
		 * wins over CHECKSUM's MODE for this boot (cmdline > MODE > warn). */
		uint32_t n;
		const char *v = cmdline_value((const char *)g_cmdline, "alpine-zfsboot.integrity=", &n);
		int m = v ? payload_sum_mode(v, n) : -1;

		if (m >= 0)
			g_sum.mode = (uint8_t)m;
		if (g_sum.mode == PAYLOAD_SUM_MODE_OFF) {
			say("integrity: off, not verified\n");
			g_sum_loaded = 0;
		} else if (g_sum.mode == PAYLOAD_SUM_MODE_ENFORCE) {
			say("integrity: enforce\n");
		}
	}
	/* Sizes now; the kernel's offset once its header has been read. */
	payload_sum_check_file("KERNEL", &g_sum.kernel, kernel_size, g_sum.kernel.offset);
	payload_sum_check_file("INITRD", &g_sum.initrd, initrd_size, 0);
	blksum_load(vol);
}

/*
 * fat_mount()/fat_open() already validate everything they read (BPB
 * geometry/signature, directory-entry type match) - a bad/stale read
 * fails their own return code, so retrying the WHOLE call (not just
 * the underlying disk_read_lba()) is exactly the same "retry the
 * validated step" discipline GPT_RETRY_ATTEMPTS's own comment
 * describes, applied to the FAT path instead of the GPT path.
 */
static int fat_mount_retry(struct fat_volume *vol, uint64_t partition_lba, uint64_t partition_sectors)
{
	int i;

	for (i = 0; i < GPT_RETRY_ATTEMPTS; i++) {
		if (fat_mount(vol, partition_lba, partition_sectors) == 0)
			return 0;
		console_puts("alpine-zfsboot-bios: FAT mount failed, retrying\n");
	}
	return -1;
}

static int fat_open_retry(const struct fat_volume *vol, const char *path, struct fat_file *file)
{
	int i;

	for (i = 0; i < GPT_RETRY_ATTEMPTS; i++) {
		if (fat_open(vol, path, file) == 0)
			return 0;
		console_puts("alpine-zfsboot-bios: FAT lookup failed for ");
		console_puts(path);
		console_puts(", retrying\n");
	}
	return -1;
}

/*
 * stage2_main - called once by stage2_entry.S with the BIOS drive
 * number the firmware handed stage1 in %dl at boot (see that file's
 * own comment). Never returns on success (ends by jumping into the
 * kernel via jump_to_kernel()); on any failure, die() halts with a
 * message rather than returning to a caller that has nothing
 * meaningful left to do either way.
 */
void stage2_main(uint8_t drive_number)
{
	struct gpt_header hdr;
	struct fat_volume vol;
	struct fat_file kernel_file, initrd_file, cmdline_file;
	uint64_t esp_lba, esp_sectors;
	struct setup_header kernel_hdr;
	uint32_t real_mode_sectors, real_mode_bytes, protected_mode_size, header_len;
	uint32_t cmdline_len;
	uint8_t e820_count;
	struct boot_video video;
	int i;

	/*
	 * Printed unconditionally, before anything else can possibly fail
	 * or hang - see build_id.h's own Makefile rule. Confirms, straight
	 * from the running binary itself, exactly which build produced it,
	 * rather than trusting that whatever source change was made
	 * actually made it into the binary under test on real hardware.
	 */
	console_puts("alpine-zfsboot-bios: alpine-zfsboot by UniDoc, version " ZFSBOOT_VERSION ", build=" ZFSBOOT_BUILD_ID "\n");

#ifndef ZFSBOOT_ISO_BUILD
	/* INT 13h AH=41h/48h (and AH=08h without extensions) - see disk.h. */
	g_boot_drive = drive_number;
	i = disk_select(drive_number);
	if (i < 0)
		die("boot drive: neither INT 13h AH=42h nor AH=02h reads work");
	/* AH=48h says the sectors are not 512 bytes, but stage1 has just
	 * read this stage from LBA 34 in 512-byte units on this very BIOS:
	 * the BIOS addresses 512-byte units, so carry on as earlier versions
	 * did, and say so (the summary repeats the reported size). */
	if (i > 0) {
		say("WARNING: AH=48h sector size ");
		puthexn(g_disk_info.bytes_per_sector, 4);
		console_puts("h, reading 512 anyway\n");
	}
#else
	disk_init(drive_number);
#endif
	console_puts("alpine-zfsboot-bios: stage2 started, drive_number=");
	console_puts_hex32((uint32_t)drive_number);
	console_puts("\n");

	/*
	 * GPT first (this project's own sgdisk-based install instructions
	 * always produce one), falling back to a classic MBR partition
	 * table only if no valid GPT is found at all - not per-attempt:
	 * gpt_read_header() failing here means "this disk has no valid
	 * GPT", not "the read was transient", so the whole GPT_RETRY_
	 * ATTEMPTS loop gets a fair chance at a real GPT before this stage
	 * ever falls back to treating the disk as MBR-labeled instead. See
	 * mbr.h's own header comment for why stage1.S itself needs no
	 * changes to support this - only this lookup does. Finds the
	 * canonical alpine-zfsboot FAT/ESP partition - the SAME partition
	 * UEFI firmware itself boots from on a UEFI install, and the one
	 * /init reads config/authorized_keys/ssh_host_ed25519_key from
	 * after boot (see fat.h's own header comment) - not a
	 * BIOS-specific partition of any kind.
	 */
	{
		int have_gpt = 0;

		for (i = 0; i < GPT_RETRY_ATTEMPTS; i++) {
			int gpt_result = gpt_read_header(&hdr);
			if (gpt_result == 0) {
				have_gpt = 1;
				break;
			}
			/*
			 * GPT_ERR_NO_SIGNATURE means the sector read fine but
			 * simply isn't GPT at all - the normal, PERMANENT state
			 * of a msdos/MBR-labeled disk (DISK_LAYOUT=msdos).
			 * Retrying can't turn that into a signature, and every
			 * msdos-mode boot would otherwise print this loop's
			 * "retrying" line up to GPT_RETRY_ATTEMPTS times, every
			 * single boot, for something that isn't a problem at
			 * all - stop immediately instead and fall through to
			 * the calm MBR message below. A genuine -1 (read error,
			 * or a signature that IS present but doesn't validate)
			 * is still worth the full retry budget.
			 */
			if (gpt_result == GPT_ERR_NO_SIGNATURE)
				break;
			console_puts("alpine-zfsboot-bios: GPT header invalid, retrying\n");
		}

		if (have_gpt) {
			for (i = 0; i < GPT_RETRY_ATTEMPTS; i++) {
				if (gpt_find_partition(&hdr, esp_type_guid, &esp_lba, &esp_sectors) == 0)
					break;
				console_puts("alpine-zfsboot-bios: FAT/ESP partition lookup failed, retrying\n");
			}
			if (i == GPT_RETRY_ATTEMPTS)
				die("FAT/ESP partition not found");
		} else {
			/*
			 * Stated as plain fact, not failure language - on a
			 * DISK_LAYOUT=msdos install this is the expected,
			 * always-taken path on every single boot, not something
			 * gone wrong (see GPT_ERR_NO_SIGNATURE's own comment
			 * above).
			 */
			console_puts("alpine-zfsboot-bios: no GPT found, using MBR\n");
			for (i = 0; i < GPT_RETRY_ATTEMPTS; i++) {
				if (mbr_find_partition(ZFSBOOT_FAT_MBR_TYPE, &esp_lba, &esp_sectors) == 0)
					break;
				console_puts("alpine-zfsboot-bios: MBR FAT partition lookup failed, retrying\n");
			}
			if (i == GPT_RETRY_ATTEMPTS)
				die("FAT/ESP partition not found (no valid GPT or MBR)");
		}
	}
	/*
	 * esp_sectors is GPT/MBR's own real, external answer to "how big
	 * is this partition" - the actual trust boundary fat_mount() now
	 * checks its own BPB-claimed total_sectors against (a full source
	 * audit found this used to be discarded here entirely, trusting
	 * the volume's own self-reported size unconditionally - see
	 * fat_mount()'s own comment on why a BPB that is merely internally
	 * self-consistent is not enough on its own).
	 */
	console_puts("alpine-zfsboot-bios: mounting FAT partition\n");
	if (fat_mount_retry(&vol, esp_lba, esp_sectors) != 0)
		die("FAT mount failed - not a valid FAT32 volume?");

	if (fat_open_retry(&vol, "/EFI/ALPINE/KERNEL", &kernel_file) != 0)
		die("EFI/ALPINE/KERNEL not found on FAT partition");
	if (fat_open_retry(&vol, "/EFI/ALPINE/INITRD", &initrd_file) != 0)
		die("EFI/ALPINE/INITRD not found on FAT partition");
	if (fat_open_retry(&vol, "/EFI/ALPINE/CMDLINE", &cmdline_file) != 0)
		die("EFI/ALPINE/CMDLINE not found on FAT partition");

	if (kernel_file.size == 0 || kernel_file.size > MAX_KERNEL_SIZE)
		die("kernel file size out of range");
	if (initrd_file.size == 0 || initrd_file.size > MAX_INITRD_SIZE)
		die("initrd file size out of range");
	if (cmdline_file.size == 0 || cmdline_file.size > CMDLINE_BUF_SIZE - 1)
		die("cmdline file size out of range");

	/*
	 * The cmdline is read first (it used to be read after the initrd): it
	 * is tiny, and alpine-zfsboot.diag=1 in it has to be known before the
	 * kernel and initrd are loaded to be of any use.
	 */
	if (fat_read_range(&vol, &cmdline_file, 0, cmdline_file.size, phys_of(g_cmdline), 0, 0) != 0)
		die_fat("FAT read failed loading cmdline");
	/*
	 * The cmdline file is a plain text file (unlike this project's own
	 * former packed boot-blob format, which included its own NUL in
	 * cmdline_size) - strip one trailing newline if present (a common
	 * side effect of how such a file tends to get written/edited), then
	 * NUL-terminate for real. cmdline_file.size was already checked
	 * against CMDLINE_BUF_SIZE-1 above, so this NUL always fits.
	 */
	cmdline_len = cmdline_file.size;
	if (cmdline_len > 0 && g_cmdline[cmdline_len - 1] == '\n')
		cmdline_len--;
	g_cmdline[cmdline_len] = '\0';
	read_options(&vol, kernel_file.size, initrd_file.size);

	console_puts("alpine-zfsboot-bios: kernel_size=");
	console_puts_hex32(kernel_file.size);
	console_puts(" initrd_size=");
	console_puts_hex32(initrd_file.size);
	console_puts(" initrd_end=");
	console_puts_hex32(INITRD_LOAD_ADDR + initrd_file.size);
	console_puts("\n");

	console_puts("alpine-zfsboot-bios: reading kernel header\n");
	/*
	 * setup_header itself is 123 bytes (see bootparams.h), starting at
	 * SETUP_HEADER_FILE_OFFSET (0x1f1 = byte 497 of the kernel file) -
	 * its own end (byte 620) is past the first 512-byte sector, so
	 * reading fewer bytes than header_len here would leave its LATER
	 * fields (header/"HdrS" at 0x202, and everything after) reading as
	 * whatever g_header_buf's own stale/zeroed content was rather than
	 * the kernel's real bytes.
	 */
	header_len = SETUP_HEADER_FILE_OFFSET + sizeof(kernel_hdr);
	if (header_len > kernel_file.size)
		die("kernel file smaller than its own setup header");

	for (i = 0; i < GPT_RETRY_ATTEMPTS; i++) {
		int j;

		if (fat_read_range(&vol, &kernel_file, 0, header_len, phys_of(g_header_buf), 0, 0) != 0) {
			report_fat_failure();
			console_puts("alpine-zfsboot-bios: kernel header read failed, retrying\n");
			continue;
		}
		{
			const uint8_t *src = g_header_buf + SETUP_HEADER_FILE_OFFSET;
			uint8_t *dst = (uint8_t *)&kernel_hdr;
			for (j = 0; j < (int)sizeof(kernel_hdr); j++)
				dst[j] = src[j];
		}
		if (kernel_hdr.boot_flag == SETUP_HEADER_BOOT_FLAG && kernel_hdr.header == SETUP_HEADER_MAGIC)
			break;
		console_puts("alpine-zfsboot-bios: kernel header invalid, retrying\n");
	}
	if (i == GPT_RETRY_ATTEMPTS)
		die("kernel header magic (HdrS) mismatch - not a valid bzImage?");

	/*
	 * setup_sects counts sectors AFTER the boot sector itself (a 0
	 * here means 4, a historical convention this field has carried
	 * since the very first boot protocol version) - the real-mode
	 * portion of the file is (setup_sects + 1) sectors in total,
	 * counting the boot sector back in; everything past that is the
	 * protected-mode kernel image this loader actually places at
	 * KERNEL_LOAD_ADDR. The modern (>=2.02) protocol this loader
	 * follows never executes any of that real-mode portion itself -
	 * only setup_header, read out of it above, is ever used.
	 */
	real_mode_sectors = (kernel_hdr.setup_sects == 0 ? 4 : kernel_hdr.setup_sects) + 1;
	real_mode_bytes = real_mode_sectors * 512u;
	if (real_mode_bytes >= kernel_file.size)
		die("kernel file smaller than its own reported real-mode portion");
	protected_mode_size = kernel_file.size - real_mode_bytes;

	/* The kernel's RAM image starts after its real-mode part; the
	 * manifest records where it thinks that is (see payload_sum.h). */
	payload_sum_check_file("KERNEL", &g_sum.kernel, kernel_file.size, real_mode_bytes);

	/*
	 * The memory map is read BEFORE anything is loaded (it used to be read
	 * just before the jump), so every fixed destination can be checked
	 * against it first: the kernel image at KERNEL_LOAD_ADDR, the range
	 * the kernel will occupy once it runs (init_size from its runtime
	 * start, see e820_range.c) and the initrd. A destination that is not
	 * RAM, or that overlaps a reserved range, stops here with the address
	 * instead of being written to.
	 */
	e820_count = check_layout(&kernel_hdr, protected_mode_size, initrd_file.size, cmdline_len);

	/*
	 * Must happen before the first copy to any address >= 1MB
	 * (KERNEL_LOAD_ADDR is exactly 0x100000 - bit 20 is the only set
	 * bit): with A20 gated off, that write silently aliases back to
	 * physical 0, corrupting the IVT/BDA instead of actually landing
	 * at 1MB. Called once, here, rather than before every fat_read_
	 * range() call above (all of which targeted g_header_buf, a
	 * low/near address well under 1MB - unreal_copy() to a low
	 * destination never wraps regardless of A20 state, see switch32.h's
	 * own comment) - A20 has no "off" path anywhere after this in the
	 * whole boot, so enabling it any earlier would just be wasted work.
	 */
	enable_a20();
	if (!a20_verify())
		die("A20 line did not enable - cannot safely load above 1MB");

	console_puts("alpine-zfsboot-bios: loading kernel ");
	{
		if (load_file(0, &vol, &kernel_file, real_mode_bytes, protected_mode_size, KERNEL_LOAD_ADDR) != 0)
			die_fat("FAT read failed loading kernel");
	}
	console_puts(" done\n");

	console_puts("alpine-zfsboot-bios: loading initrd ");
	/*
	 * initrd_addr_max: the highest physical address the kernel says
	 * the initrd may safely occupy (older kernels report a value
	 * well under 4GB) - checked for real here rather than just
	 * assumed to always clear INITRD_LOAD_ADDR + its own size.
	 */
	if ((uint64_t)INITRD_LOAD_ADDR + initrd_file.size > kernel_hdr.initrd_addr_max)
		die("initrd would exceed kernel's own initrd_addr_max");
	{
		if (load_file(1, &vol, &initrd_file, 0, initrd_file.size, INITRD_LOAD_ADDR) != 0)
			die_fat("FAT read failed loading initrd");
	}
	console_puts(" done\n");

	/*
	 * Last thing before the jump: are the bytes in RAM the ones the
	 * installer recorded? The per-block checks above judged each block
	 * as it landed; this is the end-to-end SHA-256 over what is in RAM
	 * now - it also catches anything that overwrote the kernel or initrd
	 * after it was loaded.
	 */
	verify_loaded(protected_mode_size, initrd_file.size);
	boot_summary();

	for (i = 0; i < (int)BOOT_PARAMS_SIZE; i++)
		g_boot_params[i] = 0;
	/* Print first, query after: screen_info's cursor must be where the
	 * kernel's own console output will start, i.e. below this line. */
	console_puts("alpine-zfsboot-bios: starting kernel\n");
	console_query_video(&video);
	bootparams_build(g_boot_params, &kernel_hdr, INITRD_LOAD_ADDR, initrd_file.size,
	                  phys_of(g_cmdline), g_e820, e820_count, &video);
	jump_to_kernel(kernel_hdr.code32_start, phys_of(g_boot_params));
}
