#include "ata_atapi.h"
#include "console.h"
#include "disk.h"

/*
 * disk_read_lba()/disk_init() for booting from an El Torito "no
 * emulation" CD-ROM boot entry - an ALTERNATIVE to disk.c (real hard
 * disk reads), swapped in by the Makefile for the ISO build only.
 * Everything else in this whole stage (gpt.c, stage2_main.c,
 * bootparams.c, e820.c, console.c) is compiled completely unchanged
 * for both targets and knows nothing about which one of these two
 * files it's actually linked against.
 *
 * TWO backends, tried in this order, at disk_init() time:
 *
 *   1. INT 13h/AH=0x42 ("extended read") straight against the BIOS-
 *      reported no-emulation drive number - the obvious, portable
 *      approach, and now this project's PREFERRED one (see below for
 *      why it wasn't, for a long time).
 *   2. Direct ATA/ATAPI PIO (ata_atapi.c) - this project's own
 *      from-scratch driver, talking to the controller's task-file
 *      registers directly, bypassing INT13h/BIOS entirely.
 *
 * History, and why this is now the opposite of how this file used to
 * read: an early version of this project tried INT13h ONLY, tested it
 * exhaustively on real hardware, and it failed identically no matter
 * which parameter (LBA, sector count, destination segment, drive
 * number) was varied - AH=0x0C every time. That looked, at the time,
 * like "the read path is closed at the INT13h abstraction itself for
 * this firmware", and the direct-ATA/ATAPI driver (ata_atapi.c) was
 * written specifically to route around it. It later turned out that
 * conclusion was itself an artifact of a real, separate bug this
 * project hit and fixed much later: Alpine/musl's own <stdint.h>,
 * under -m16, silently defines uint64_t as 4 bytes instead of 8 (see
 * stdint_local.h's own header comment for the full DWARF-confirmed
 * story) - and the INT13h "disk address packet" this project's own
 * disk.c (the real-HDD path) has always used has a uint64_t start_lba
 * field at exactly that fault line. Under the broken typedef, that
 * whole struct silently packed to 12 bytes instead of the BIOS-
 * mandated 16, so EVERY INT13h call this project ever issued against
 * the CD-ROM drive was handing BIOS a malformed packet - genuinely
 * plausible cause for "every single read failed identically no matter
 * what any C-level variable said": the bytes on the wire were wrong
 * regardless. Re-tested for real after that fix (see git history/this
 * project's own build for the actual A/B benchmark), on the same VM,
 * same ISO, same firmware: INT13h now works, and is dramatically
 * faster than this file's own direct-ATAPI driver on this host - very
 * likely because each ATAPI PIO port I/O this driver issues is its
 * own guest/QEMU-device-emulation round trip (a KVM vmexit per
 * in/outw), where BIOS's own internal CD-ROM handling evidently
 * doesn't pay that same cost per byte.
 *
 * The direct-ATAPI driver is kept, not deleted, as the fallback for
 * exactly the case that motivated writing it in the first place: some
 * other firmware/drive combination where INT13h genuinely doesn't
 * work (or stops working partway through a boot) - talking to the
 * ATA/ATAPI protocol directly can see and clear the ACTUAL condition
 * (a SCSI Unit Attention/Not Ready state, reported via REQUEST SENSE -
 * see ata_atapi.c's own atapi_init()) that INT13h's own single opaque
 * error code collapses away, so it remains a genuine, independent
 * safety net rather than a doomed retry of the same failing thing.
 * disk_init() below decides ONCE, per boot, which backend to use for
 * the whole rest of that boot (a real functional test read, not just
 * "the BIOS claims extensions are present" - see
 * int13_extensions_present()'s own comment for why presence alone
 * isn't enough evidence) - and native_read_batch() falls further back
 * from INT13h to ATAPI, once, permanently for the rest of this boot,
 * if INT13h ever exhausts its own retries on a real read later too.
 * Scope for the ATAPI fallback specifically (see ata_atapi.h's own
 * header comment): an ATAPI CD-ROM behind a legacy-compatible IDE
 * controller (real hardware in CSM/legacy mode, and QEMU/Proxmox's
 * default IDE CD-ROM bus) - not AHCI-native or virtio-scsi-attached
 * CD-ROMs.
 *
 * The one thing that hasn't changed regardless of backend: the native
 * (2048-byte) vs. GPT (512-byte) unit reconciliation below. The
 * on-disk GPT this stage's gpt.c reads (written by xorriso's
 * -appended_part_as_gpt) is in the standard 512-byte GPT unit
 * regardless of the media's native block size - confirmed directly in
 * libisofs's own source (libisofs/system_area.c: appended_part_start[i]
 * * 4 before being stored as a GPT start_block, and boot_sectors.txt's
 * own doc stating plainly "Block size is always 512" for the GPT it
 * writes) - the same 512-byte unit gpt.c/stage2_main.c already use
 * throughout for the real-HDD path. So: two different units, both
 * real, that this file's whole job is to reconcile - by presenting the
 * SAME 512-byte-unit disk_read_lba() contract disk.h documents
 * (DISK_SECTOR_SIZE, unchanged), while internally doing native
 * 2048-byte reads (via whichever backend is active) and slicing out
 * whichever 512-byte sub-range within each native sector the caller
 * actually asked for. A caller never has to know or care which
 * backend it's actually talking to.
 */

#define NATIVE_SECTOR_SIZE 2048
#define UNITS_PER_NATIVE (NATIVE_SECTOR_SIZE / DISK_SECTOR_SIZE) /* = 4 */

/*
 * How many native (2048-byte) sectors this file fetches in a single
 * read command (either backend). Not the full ATAPI_MAX_READ_BLOCKS
 * (31, see ata_atapi.h's own comment for why not 32) - this whole
 * stage's code+rodata+every one of its own buffers+stack all share a
 * single 64KB real-mode segment (see link.ld/stage2_entry.S), unlike
 * the UEFI path's flat 32-bit address space, which has no such
 * ceiling at all - a structural limit of real-mode boot code, not a
 * driver inefficiency. NATIVE_BATCH stays at the same value
 * regardless of which backend is active, both so neither backend's
 * own command-count/overhead is muddied by also comparing different
 * batch sizes, and because this budget is shared: this file's own
 * g_native_buf and fat.c's g_fat_io_buf (FAT_IO_BATCH_SECTORS below)
 * are the two large buffers competing for the same headroom.
 *
 * An earlier version of this comment claimed 16 "leaves a comfortable
 * ~9KB of stack headroom" while the actual constant next to it was
 * 11 - a real discrepancy a later audit caught and re-verified for
 * real (built at every value 11-31, `nm`-inspected `_bss_end` each
 * time, this exact codebase - the 9KB/16 claim was already stale by
 * the time it was written, most likely left over from an earlier,
 * smaller build of this stage rather than ever having been true of
 * this code).
 *
 * That table itself went stale in turn: fat.c grew a second, separate
 * FAT-sector cache (g_fat_table_cache2 - see that file's own comment
 * on why: fat_chain_advance()'s Floyd hare/tortoise cycle detection
 * was sharing ONE cache between both pointers, so once a chain got
 * long enough hare and tortoise were usually in different FAT
 * sectors and kept evicting each other's cache line - measured on a
 * realistic ~72MB kernel+initrd payload at 20,827 total INT13h/ATAPI
 * calls before the fix, 2,824 after; giving tortoise its own 512-byte
 * cache slot fixed it, a 7.4x reduction). That extra 512 bytes of
 * .bss, plus this project's own later hardening-pass growth, ate most
 * of NATIVE_BATCH=11's documented ~4.4KB margin - confirmed for real
 * on a genuinely different toolchain (Alpine's own gcc 15.2.0,
 * musl-based build host, not this sandbox's Debian gcc cross build):
 * the exact same source built there landed _bss_end 26 bytes PAST
 * check-bss-bounds's own limit and failed the real build, while this
 * sandbox's own gcc build still passed with only 1286 bytes to spare
 * - a toolchain-to-toolchain swing of ~1.3KB on identical source, all
 * by itself most of the remaining margin. Re-measured the same way
 * the original table was built (every value, `nm`-inspected
 * `_bss_end`, this sandbox's toolchain, FAT_IO_BATCH_SECTORS still at
 * its current 36):
 *
 *   NATIVE_BATCH  _bss_end  margin to check-bss-bounds's own limit
 *        11        0xf2ea    1286 bytes  <- was "current", now too
 *                                            tight (see above - a
 *                                            different real toolchain
 *                                            already failed here)
 *        10        0xeaea    3334 bytes
 *         9        0xe2ea    5382 bytes  <- current, this file
 *         8        0xdaea    7430 bytes
 *         7        0xd2ea    9478 bytes
 *
 * Each step is exactly 2048 bytes (NATIVE_SECTOR_SIZE), as expected -
 * g_native_buf is by far this build's largest single .bss consumer.
 * 9 is the new deliberate choice: 5382 bytes of margin in THIS
 * (sandbox) toolchain is enough headroom that even the ~1.3KB
 * toolchain swing just observed firsthand (not a hypothetical) leaves
 * comfortably more than the original ~4.4KB design intent behind 11
 * used to provide, without guessing at how much bigger some future
 * toolchain might run.
 *
 * UPDATE (F22, unidoc-alip's PR #5 follow-up review, correcting a
 * transcription error in the FIRST review's own re-measurement): re-
 * measured against the REAL Alpine gcc 15.2.0 CI toolchain this
 * project actually ships with (not this sandbox's own Debian cross-
 * compiler, the toolchain every number in the table above was built
 * with) - _bss_end there is 0xe84a, 4006 bytes of margin at
 * NATIVE_BATCH=9. Still real, still comfortable headroom (over the
 * ~4.4KB original design intent behind NATIVE_BATCH=11, and nowhere
 * near check-bss-bounds's own limit) - just a third real data point
 * (sandbox gcc: 5382B, Alpine gcc 15.2.0: 4006B) worth keeping
 * alongside the table above rather than letting it silently stand in
 * as if it were the number that matters for the toolchain real builds
 * actually use.
 *
 * Costs a real, modest thing in return: the ATAPI-fallback
 * native-sector backend (cdrom_disk.c's own atapi_read_native() path
 * - only ever used when a BIOS lacks working INT13h extensions on
 * optical media, see this file's own int13_extensions_present()) now
 * issues roughly 11/9 (~22%) more read commands for the same payload
 * than before. Accepted deliberately: this is the fallback path, not
 * the common one (see int13_read_native() above, tried first), and a
 * stack/.bss collision on real hardware is a silent-corruption bug,
 * not a recoverable slow boot - the same trade-off this whole
 * check-bss-bounds gate exists to enforce.
 *
 * Separately, real-mode-segment-offset safety (bss_end < 0x10000,
 * checked at build time by check-bss-bounds - see ../Makefile) is NOT
 * the same claim as "never crosses a 64KB-ALIGNED PHYSICAL boundary"
 * for this build specifically - see ../Makefile's own check-bss-bounds
 * comment for why the two only coincide when the segment's own
 * physical base is itself 64KB-aligned, which STAGE2_SEGMENT=0x1000
 * (the real-disk build) is and ISO_SEG=0x7c0 (this build, physical
 * base 0x7c00) is NOT. Verified directly for this file's own transfer
 * buffer: the one 64KB-aligned physical address reachable at all from
 * this segment (offset range [0,0x10000) only, since nothing here
 * exceeds that) is physical 0x10000, which is segment offset
 * 0x10000-0x7c00 = 0x8400. g_native_buf sits at a fixed offset - 0x9ae0
 * today (re-verified via `nm` alongside the table above; grew from an
 * earlier-documented 0x9660 once fat.c's own g_fat_table_cache2 was
 * added ahead of it in link order - see that comment above), with
 * FAT_IO_BATCH_SECTORS=36, independent of NATIVE_BATCH itself -
 * everything ahead of it in link order is unaffected by this
 * constant, only BY this constant's own size is what comes AFTER it
 * (nothing, it's last) - already past that point for every value in
 * the table above, so its own transfer buffer never straddles that
 * boundary - confirmed by inspection, not assumed from the
 * segment-offset check alone. This would need re-checking by hand
 * (not just re-running check-bss-bounds) if FAT_IO_BATCH_SECTORS's
 * own size, or anything else placed before g_native_buf in this
 * file's object, ever changed enough to move g_native_buf's start
 * below offset 0x8400.
 */
#define NATIVE_BATCH 9
#define NATIVE_BATCH_BYTES (NATIVE_BATCH * NATIVE_SECTOR_SIZE)

static uint8_t g_native_buf[NATIVE_BATCH_BYTES];
static uint8_t g_drive_number;
static int g_use_int13;
static int g_atapi_probed;
static int g_atapi_ready;

/*
 * Same "disk address packet" layout as disk.c's own (see that file's
 * header comment for the exact BIOS-defined field meanings) -
 * duplicated here rather than shared via disk.h, since disk.h's own
 * disk_read_lba()/disk_init() declarations are the public interface
 * every backend (this file, disk.c) implements, not something either
 * can also expose a private struct through.
 */
struct disk_address_packet {
	uint8_t size;
	uint8_t reserved;
	uint16_t num_blocks;
	uint16_t buffer_offset;
	uint16_t buffer_segment;
	uint64_t start_lba;
} __attribute__((packed));

/*
 * The exact bug this whole file's history section above describes was
 * a wrong-sized uint64_t silently shrinking this struct - guarded
 * permanently, the same way gpt.c guards its own GPT header struct
 * against the identical class of bug, and disk.c guards its own
 * (separate, real-HDD) copy of this same struct.
 */
_Static_assert(sizeof(struct disk_address_packet) == 16,
               "disk_address_packet must be exactly 16 bytes (BIOS AH=0x42 DAP format)");

static int int13_read_native(uint32_t native_lba, uint16_t native_count, void *buf)
{
	struct disk_address_packet dap;
	uint16_t buf_off, dap_off, ds_seg;
	uint8_t failed;

	/* See disk.c's own identical comment for buf_off/dap_off/ds_seg. */
	buf_off = (uint16_t)(unsigned long)buf;
	dap_off = (uint16_t)(unsigned long)&dap;
	__asm__ __volatile__("mov %%ds, %0" : "=r"(ds_seg));

	dap.size = sizeof(dap);
	dap.reserved = 0;
	dap.num_blocks = native_count;
	dap.buffer_offset = buf_off;
	dap.buffer_segment = ds_seg;
	dap.start_lba = native_lba;

	/* See disk.c's own identical comment for the "memory" clobber, and
	 * e820.c's own comment for why "ebp" (F18, unidoc-alip's PR #5
	 * review - the same real hardware finding console.c's own
	 * console_putc() and e820.c's own INT 0x15 call already carry a
	 * clobber for: a BIOS's own interrupt handler is free to use, and
	 * not restore, EBP internally, a plain GPR here under
	 * -fomit-frame-pointer). "esi" is deliberately NOT also added -
	 * unlike e820.c's case, this asm already explicitly preserves it
	 * itself (push/pop around the call), a stronger guarantee than a
	 * clobber would add (the real original value survives, not just
	 * "GCC no longer trusts it"). Confirmed to compile cleanly at this
	 * file's own real -O2 build flags and boot for real under QEMU
	 * (tests/bios-iso-entry-test.sh, both the plain INT13h and forced-
	 * ATAPI paths, and tests/bios-hdd-entry-test.sh for disk.c's own
	 * identical fix) - not just reasoned about. */
	__asm__ __volatile__(
		"push %%si\n\t"
		"mov %1, %%si\n\t"
		"movb $0x42, %%ah\n\t"
		"int $0x13\n\t"
		"setc %0\n\t"
		"pop %%si\n\t"
		: "=q"(failed)
		: "r"(dap_off), "d"(g_drive_number)
		: "ah", "ebp", "cc", "memory"
	);

	if (failed)
		return -1;

	/* See disk.c's own identical comment for why this is a real,
	 * free-either-way check - not every real BIOS updates this field
	 * on a short transfer, but reading it back here catches the ones
	 * that do, and is a no-op for the ones that don't. */
	if (dap.num_blocks != native_count)
		return -1;

	return 0;
}

/*
 * "IBM/MS INT 13 Extensions - INSTALLATION CHECK" (AH=0x41): BIOS
 * reports (via BX/CX on return, carry clear) whether AH=0x42/extended
 * reads are supported for this drive number AT ALL. NOT, on its own,
 * proof that a real read will actually succeed against THIS specific
 * no-emulation CD-ROM drive - this project's own history (see this
 * file's header comment) is a direct example of a real, different bug
 * making every subsequent real read fail regardless of what this
 * check reported. disk_init() below treats this as a cheap first
 * filter only, always followed by one real, functional test read
 * before trusting this backend for the whole rest of the boot.
 *
 * F18 (unidoc-alip's PR #5 review, closed by the follow-up review):
 * this asm passes g_drive_number in via a plain "d" (DX) input
 * constraint, and AH=0x41's own real BIOS return convention can leave
 * DH holding data this code never reads - a bare input operand doesn't
 * tell GCC DX's contents are redefined afterward, so in principle some
 * OTHER live value the compiler happened to also be keeping in DX
 * around this call could be silently corrupted, the same class of
 * finding that got "ebp" added to this file's own int13_read_native()
 * above and to e820.c's INT 0x15 call. Unlike those two, a GCC-level
 * fix (a real "+d" capture operand telling GCC DX is destroyed) does
 * NOT fit here - confirmed empirically, not just reasoned, that adding
 * either "dx" to the clobber list (invalid - a register can't be both
 * a constraint operand and a clobber) or a "+d" scratch capture
 * operand both fail this file's own real -O2 build with "asm operand
 * has impossible constraints or there are not enough registers", the
 * identical register-exhaustion failure e820.c's own comment already
 * documents for its unrelated "edi" case - this function's tightly-
 * packed 4-operand set (carry/bx/cx/dx) genuinely has no register
 * budget left for a fifth.
 *
 * The actual fix needs no extra operand at all: push %dx onto the
 * stack as the asm block's own first instruction (BEFORE anything
 * could touch it), pop it back as its second-to-last (after int $0x13
 * returns, before setc reads the carry flag - `pop` does not itself
 * touch EFLAGS, so this ordering is safe). GCC is never told DX
 * changed because, from its own point of view, it genuinely didn't:
 * whatever value it handed this asm block in DX is exactly what comes
 * back out, regardless of what the BIOS did to DH/DL internally in
 * between - the same class of protection "ebp" gets elsewhere in this
 * file, achieved here via the stack instead of a constraint, at the
 * cost of 2 bytes of stack space for the asm block's own duration and
 * nothing else.
 */
static int int13_extensions_present(void)
{
	uint16_t result_bx, result_cx;
	uint8_t carry;

	__asm__ __volatile__(
		"pushw %%dx\n\t"
		"movw $0x55aa, %%bx\n\t"
		"movb $0x41, %%ah\n\t"
		"int $0x13\n\t"
		"popw %%dx\n\t"
		"setc %0\n\t"
		: "=q"(carry), "=b"(result_bx), "=c"(result_cx)
		: "d"(g_drive_number)
		: "ah", "cc"
	);

	/* BX must come back byte-swapped (0xAA55) per the BIOS spec;
	 * CX bit 0 is "extended disk access functions (AH=42h-44h,
	 * 47h,48h) supported". */
	return !carry && result_bx == 0xaa55 && (result_cx & 1);
}

void disk_init(uint8_t drive_number)
{
	g_drive_number = drive_number;

	/*
	 * Native LBA 0, straight into g_native_buf - a real, functional
	 * probe read, not just trusting int13_extensions_present()'s own
	 * presence check (see that function's own comment for why: this
	 * project has direct, painful experience of "BIOS says the
	 * capability exists" and "a real read against this exact drive
	 * works" being two different questions). g_native_buf is still
	 * empty/unused at this point in boot either way, so overwriting
	 * it here for a test read costs nothing.
	 */
#ifndef ZFSBOOT_FORCE_ATAPI
	if (int13_extensions_present() && int13_read_native(0, 1, g_native_buf) == 0) {
		g_use_int13 = 1;
		console_puts("alpine-zfsboot-bios: CD-ROM backend=INT13h drive_number=");
		console_puts_hex32(drive_number);
		console_putc('\n');
		return;
	}
#endif
	/*
	 * ZFSBOOT_FORCE_ATAPI (a build-time -D, not a runtime flag) skips
	 * the INT13h attempt above entirely, unconditionally exercising the
	 * ATAPI backend even under QEMU/SeaBIOS, where INT13h normally
	 * succeeds and this whole driver would otherwise never run during
	 * an automated boot test. Real hardware naturally always ends up
	 * here anyway (this project's own real-hardware testing: INT13h has
	 * no working read path at all for a "no emulation" El Torito boot
	 * drive on the firmware this was tested against - see ata_atapi.h's
	 * own header comment) - this flag exists purely so that same code
	 * path can be verified with a real QEMU boot on demand (`make
	 * stage-iso.bin ZFSBOOT_FORCE_ATAPI=1`, or see the Justfile's
	 * test-iso-atapi recipe), not to change real boot behavior.
	 */
	console_puts("alpine-zfsboot-bios: CD-ROM backend=INT13h unavailable, using ATAPI\n");
	g_use_int13 = 0;
	g_atapi_ready = (atapi_init() == 0);
	g_atapi_probed = 1;
}

/*
 * AH=0x0C (DISK_RET_EBADTRACK)-equivalent retrying: even talking to
 * the hardware directly, a single transient failure (the device busy
 * settling right after the BIOS handoff, say) is worth one immediate
 * retry before giving up - same READ_MAX_ATTEMPTS budget and same
 * "no delay between attempts" reasoning this project confirmed the
 * hard way on real hardware (see stage2_main.c's own
 * GPT_RETRY_ATTEMPTS comment): a growing busy-wait delay here never
 * once cleared a failure across repeated attempts, while fast,
 * back-to-back retries did.
 */
#define READ_MAX_ATTEMPTS 10

/*
 * Reads `native_count` (1..NATIVE_BATCH) contiguous native sectors
 * starting at native_lba into g_native_buf in ONE command, via
 * whichever backend disk_init() selected - falling all the way back
 * to ATAPI (lazily initializing it, exactly once, right here) if
 * INT13h ever exhausts its own retries on a real read, and staying on
 * ATAPI for the rest of this boot from that point on rather than
 * flapping back and forth. See this file's own header comment for the
 * full backend-selection story and NATIVE_BATCH's own comment for why
 * batching like this matters at all.
 */
static int native_read_batch(uint32_t native_lba, uint32_t native_count)
{
	int attempt;

	if (g_use_int13) {
		for (attempt = 0; attempt < READ_MAX_ATTEMPTS; attempt++) {
			if (int13_read_native(native_lba, (uint16_t)native_count, g_native_buf) == 0) {
				if (attempt > 0)
					console_puts("alpine-zfsboot-bios: CD-ROM read retry succeeded (INT13h)\n");
				return 0;
			}
		}
		console_puts("alpine-zfsboot-bios: CD-ROM read failed after retries (INT13h), falling back to ATAPI\n");
		g_use_int13 = 0;
		if (!g_atapi_probed) {
			g_atapi_ready = (atapi_init() == 0);
			g_atapi_probed = 1;
		}
	}

	if (!g_atapi_ready)
		return -1;

	for (attempt = 0; attempt < READ_MAX_ATTEMPTS; attempt++) {
		if (atapi_read_native(native_lba, (uint16_t)native_count, g_native_buf) == 0) {
			if (attempt > 0)
				console_puts("alpine-zfsboot-bios: CD-ROM read retry succeeded (ATAPI)\n");
			return 0;
		}
		console_puts("alpine-zfsboot-bios: CD-ROM read failed (ATAPI), retrying\n");
	}

	console_puts("alpine-zfsboot-bios: CD-ROM read failed after retries (ATAPI)\n");
	return -1;
}

int disk_read_lba(uint64_t lba, uint16_t count, void *buf)
{
	uint8_t *dst = (uint8_t *)buf;
	uint64_t cur = lba;
	uint16_t remaining = count;

	while (remaining > 0) {
		uint64_t native_lba = cur / UNITS_PER_NATIVE;
		uint32_t sub_index = (uint32_t)(cur % UNITS_PER_NATIVE);
		/*
		 * How many native sectors, starting at native_lba, cover
		 * every 512-byte unit still wanted (sub_index accounts for
		 * `cur` not necessarily starting exactly on a native-sector
		 * boundary) - capped at NATIVE_BATCH, this file's own fixed
		 * per-command batch ceiling, so a large request (a whole
		 * kernel/initrd chunk) is served in as few NATIVE_BATCH-sized
		 * commands as it takes, not one native sector at a time.
		 */
		uint32_t native_count = (sub_index + remaining + UNITS_PER_NATIVE - 1) / UNITS_PER_NATIVE;
		uint32_t avail, take;
		uint32_t i;
		const uint8_t *src;

		if (native_count > NATIVE_BATCH)
			native_count = NATIVE_BATCH;

		if (native_read_batch((uint32_t)native_lba, native_count) != 0)
			return -1;

		avail = native_count * UNITS_PER_NATIVE - sub_index;
		take = avail < remaining ? avail : remaining;

		src = g_native_buf + (sub_index * DISK_SECTOR_SIZE);
		for (i = 0; i < take * DISK_SECTOR_SIZE; i++)
			dst[i] = src[i];

		dst += take * DISK_SECTOR_SIZE;
		cur += take;
		remaining -= (uint16_t)take;
	}

	return 0;
}
