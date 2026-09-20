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
 * (31, see ata_atapi.h's own comment for why not 32) - checked
 * directly by building at several values and reading back the
 * linker's own _bss_end: this whole stage's code+rodata+every one of
 * its own buffers+stack all share a single 64KB real-mode segment
 * (see link.ld/stage2_entry.S), unlike the UEFI path's flat 32-bit
 * address space, which has no such ceiling at all - a structural limit
 * of real-mode boot code, not a driver inefficiency. 16 (32768 bytes
 * per command) leaves a comfortable ~9KB of stack headroom, confirmed
 * via the same build+_bss_end check; NATIVE_BATCH stays at the same
 * value regardless of which backend is active, both for that same
 * stack-headroom reason and so a future timing comparison between the
 * two backends is never muddied by also comparing different batch
 * sizes.
 */
#define NATIVE_BATCH 11
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

	/* See disk.c's own identical comment for the "memory" clobber. */
	__asm__ __volatile__(
		"push %%si\n\t"
		"mov %1, %%si\n\t"
		"movb $0x42, %%ah\n\t"
		"int $0x13\n\t"
		"setc %0\n\t"
		"pop %%si\n\t"
		: "=q"(failed)
		: "r"(dap_off), "d"(g_drive_number)
		: "ah", "cc", "memory"
	);

	return failed ? -1 : 0;
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
 */
static int int13_extensions_present(void)
{
	uint16_t result_bx, result_cx;
	uint8_t carry;

	__asm__ __volatile__(
		"movw $0x55aa, %%bx\n\t"
		"movb $0x41, %%ah\n\t"
		"int $0x13\n\t"
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
	if (int13_extensions_present() && int13_read_native(0, 1, g_native_buf) == 0) {
		g_use_int13 = 1;
		console_puts("alpine-zfsboot-bios: CD-ROM backend=INT13h drive_number=");
		console_puts_hex32(drive_number);
		console_putc('\n');
		return;
	}

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
