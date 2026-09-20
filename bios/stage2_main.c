#include "stdint_local.h"

#include "bootblob.h"
#include "bootparams.h"
#include "build_id.h"
#include "console.h"
#include "disk.h"
#include "e820.h"
#include "gpt.h"
#include "mbr.h"
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
 * How much of the boot-blob's kernel/initrd sections this stage reads
 * per disk_read_lba()+unreal_copy() cycle - a multiple of the sector
 * size so every non-final chunk maps to a whole number of sectors
 * with nothing left over; see load_to_high()'s own comment for why
 * the final, possibly-shorter chunk needs no special handling either.
 *
 * Raised from an original 8192 (16 sectors = 4 native ATAPI blocks):
 * confirmed the hard way that THIS constant, not cdrom_disk.c's own
 * NATIVE_BATCH, was the real ceiling on ATAPI command batching for
 * the whole kernel/initrd load - disk_read_lba() never sees a `count`
 * bigger than sectors_for(CHUNK_SIZE) from this file, regardless of
 * how many native blocks its own internal buffer could otherwise
 * batch per command, so NATIVE_BATCH raised on its own (an earlier
 * attempt at this same performance problem) changed nothing real: it
 * had headroom to batch far more than it was ever actually asked for.
 * disk_read_lba()'s own loop already issues as many NATIVE_BATCH-sized
 * ATAPI commands as a request needs, so CHUNK_SIZE and NATIVE_BATCH
 * are independent knobs, not required to match - this one just needed
 * to actually be raised.
 */
#define CHUNK_SIZE 18432

/*
 * Sanity ceilings on blob_hdr.kernel_size/initrd_size, checked before
 * ANY of the sector-count/LBA arithmetic below runs - not just
 * generous headroom over a realistic kernel+initrd, but a real
 * security boundary: kernel_size/initrd_size come straight off disk
 * with no other independent check that they're sane before being fed
 * into sectors_for()'s own `(bytes + 511) / 512` (a 32-bit add that
 * genuinely overflows for a value near UINT32_MAX - confirmed by
 * reasoning through the arithmetic, not just asserted) and then used
 * as a physical-memory copy length in load_to_high(). MAX_KERNEL_SIZE
 * is deliberately small enough that KERNEL_LOAD_ADDR + MAX_KERNEL_SIZE
 * stays well clear of INITRD_LOAD_ADDR - the two destinations can
 * never overlap as long as this holds, independent of whatever the
 * boot-blob partition's own size (blob_sectors) claims.
 */
#define MAX_KERNEL_SIZE (48u * 1024 * 1024)
#define MAX_INITRD_SIZE (512u * 1024 * 1024)

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

static uint8_t g_chunk[CHUNK_SIZE];
static uint8_t g_boot_params[BOOT_PARAMS_SIZE];
static uint8_t g_cmdline[ZFSBOOT_BOOTBLOB_SECTOR_SIZE];
static struct boot_e820_entry g_e820[BOOT_PARAMS_E820_MAX_ENTRIES];

static const uint8_t bootblob_type_guid[16] = ZFSBOOT_BOOTBLOB_TYPE_GUID_BYTES;

static uint32_t phys_of(const void *p)
{
	return (uint32_t)(STAGE2_SEGMENT << 4) + (uint32_t)(unsigned long)p;
}

static void die(const char *msg)
{
	console_puts("alpine-zfsboot-bios: FATAL: ");
	console_puts(msg);
	console_puts("\n");
	for (;;)
		__asm__ __volatile__("hlt");
}

static uint32_t sectors_for(uint32_t bytes)
{
	return (bytes + (ZFSBOOT_BOOTBLOB_SECTOR_SIZE - 1)) / ZFSBOOT_BOOTBLOB_SECTOR_SIZE;
}

/*
 * Reads total_bytes starting at start_lba (whole sectors only, as
 * every disk_read_lba() call is) through the low, real-mode-
 * addressable g_chunk staging buffer, moving each chunk up to its
 * final destination dst_phys via unreal_copy() (see switch32.h) -
 * the standard "stage through low memory, then move up" pattern any
 * real-mode loader needs once data has to end up above 1MB. The
 * final chunk may read a little more than total_bytes' own remainder
 * (rounded up to the next whole sector) but this always copies
 * exactly the requested number of real bytes out of that chunk, never
 * the trailing sector-padding past it - only relevant on the very
 * last chunk, which has no next iteration for a stray few extra bytes
 * to ever matter to.
 */
static void load_to_high(uint64_t start_lba, uint32_t total_bytes, uint32_t dst_phys)
{
	uint32_t remaining = total_bytes;
	uint64_t lba = start_lba;
	/*
	 * A dot every 64 chunks (~1.1MB at this CHUNK_SIZE) instead of a
	 * whole numbered line
	 * every 4MB - plain progress feedback ("something is still
	 * happening"), not a debugging aid: the numbered-line version and
	 * the per-chunk RDTSC timing that used to be here both did their
	 * job (localizing a real crash, then a real ATAPI phase-timing
	 * question) and neither is needed now that both are understood/
	 * fixed - see git history for that investigation, not this
	 * function.
	 */
	uint32_t chunk_num = 0;

	while (remaining > 0) {
		uint32_t this_chunk = remaining < CHUNK_SIZE ? remaining : CHUNK_SIZE;
		uint32_t sectors = sectors_for(this_chunk);

		if (disk_read_lba(lba, (uint16_t)sectors, g_chunk) != 0)
			die("disk_read_lba failed loading kernel/initrd");

		unreal_copy(dst_phys, g_chunk, this_chunk);

		lba += sectors;
		dst_phys += this_chunk;
		remaining -= this_chunk;

		chunk_num++;
		if ((chunk_num & 63) == 0)
			console_putc('.');
	}
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
	uint64_t blob_lba, blob_sectors;
	struct zfsboot_bootblob_header blob_hdr;
	struct setup_header kernel_hdr;
	uint32_t real_mode_sectors, real_mode_bytes, protected_mode_size;
	uint64_t kernel_lba, initrd_lba, cmdline_lba;
	uint8_t e820_count;
	int i;

	/*
	 * Printed unconditionally, before anything else can possibly fail
	 * or hang - see build_id.h's own Makefile rule. Confirms, straight
	 * from the running binary itself, exactly which build produced it,
	 * rather than trusting that whatever source change was made
	 * actually made it into the binary under test on real hardware.
	 */
	console_puts("alpine-zfsboot-bios: alpine-zfsboot by UniDoc, version " ZFSBOOT_VERSION ", build=" ZFSBOOT_BUILD_ID "\n");

	disk_init(drive_number);
	console_puts("alpine-zfsboot-bios: stage2 started, drive_number=");
	console_puts_hex32((uint32_t)drive_number);
	console_puts("\n");

	/*
	 * Retrying here (not just inside disk_read_lba()'s own retry loop)
	 * matters for a real, distinct failure mode confirmed on real
	 * hardware: a read that reports success (no error at all) but
	 * whose CONTENT is still wrong - silently, with nothing for a
	 * lower layer to even notice. gpt_read_header()'s own
	 * signature+CRC32 check (and gpt_find_partition()'s "did we
	 * actually find the partition" check, and the boot-blob/kernel
	 * magic checks below) are the ONLY things that can catch that:
	 * retrying the ENTIRE read+validate step, at THIS level, until it
	 * comes out to real content (or genuinely giving up), not just
	 * retrying a failure-flagged transfer that never got the chance to
	 * look wrong in the first place.
	 */
	/*
	 * GPT first (this project's own sgdisk-based install instructions
	 * always produce one), falling back to a classic MBR partition
	 * table only if no valid GPT is found at all - not per-attempt:
	 * gpt_read_header() failing here means "this disk has no valid
	 * GPT", not "the read was transient", so the whole GPT_RETRY_
	 * ATTEMPTS loop gets a fair chance at a real GPT before this stage
	 * ever falls back to treating the disk as MBR-labeled instead. See
	 * mbr.h's own header comment for why stage1.S itself needs no
	 * changes to support this - only this lookup does.
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
				if (gpt_find_partition(&hdr, bootblob_type_guid, &blob_lba, &blob_sectors) == 0)
					break;
				console_puts("alpine-zfsboot-bios: boot-blob partition lookup failed, retrying\n");
			}
			if (i == GPT_RETRY_ATTEMPTS)
				die("boot-blob partition not found");
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
				if (mbr_find_partition(ZFSBOOT_BOOTBLOB_MBR_TYPE, &blob_lba, &blob_sectors) == 0)
					break;
				console_puts("alpine-zfsboot-bios: MBR boot-blob partition lookup failed, retrying\n");
			}
			if (i == GPT_RETRY_ATTEMPTS)
				die("boot-blob partition not found (no valid GPT or MBR)");
		}
	}

	for (i = 0; i < GPT_RETRY_ATTEMPTS; i++) {
		int j;
		int magic_ok;

		if (disk_read_lba(blob_lba, 1, g_chunk) != 0) {
			console_puts("alpine-zfsboot-bios: boot-blob header read failed, retrying\n");
			continue;
		}
		{
			const uint8_t *src = g_chunk;
			uint8_t *dst = (uint8_t *)&blob_hdr;
			for (j = 0; j < (int)sizeof(blob_hdr); j++)
				dst[j] = src[j];
		}
		magic_ok = 1;
		for (j = 0; j < ZFSBOOT_BOOTBLOB_MAGIC_LEN; j++) {
			if (blob_hdr.magic[j] != ZFSBOOT_BOOTBLOB_MAGIC[j])
				magic_ok = 0;
		}
		if (magic_ok && blob_hdr.header_size == sizeof(blob_hdr))
			break;
		console_puts("alpine-zfsboot-bios: boot-blob header invalid, retrying\n");
	}
	if (i == GPT_RETRY_ATTEMPTS)
		die("boot-blob magic mismatch");
	if (blob_hdr.cmdline_size == 0 || blob_hdr.cmdline_size > sizeof(g_cmdline))
		die("boot-blob cmdline_size out of range");
	/*
	 * See MAX_KERNEL_SIZE/MAX_INITRD_SIZE's own comment above - this
	 * must happen before kernel_lba/initrd_lba/cmdline_lba below are
	 * computed at all, since sectors_for() applied to an unbounded
	 * value is exactly the operation these caps make safe.
	 */
	if (blob_hdr.kernel_size == 0 || blob_hdr.kernel_size > MAX_KERNEL_SIZE)
		die("boot-blob kernel_size out of range");
	if (blob_hdr.initrd_size == 0 || blob_hdr.initrd_size > MAX_INITRD_SIZE)
		die("boot-blob initrd_size out of range");

	kernel_lba = blob_lba + 1;
	initrd_lba = kernel_lba + sectors_for(blob_hdr.kernel_size);
	cmdline_lba = initrd_lba + sectors_for(blob_hdr.initrd_size);

	console_puts("alpine-zfsboot-bios: kernel_size=");
	console_puts_hex32(blob_hdr.kernel_size);
	console_puts(" initrd_size=");
	console_puts_hex32(blob_hdr.initrd_size);
	console_puts(" initrd_end=");
	console_puts_hex32(INITRD_LOAD_ADDR + blob_hdr.initrd_size);
	console_puts("\n");

	/*
	 * The boot-blob partition's OWN declared size (blob_sectors, from
	 * gpt_find_partition()) must actually cover everything the header
	 * claims is inside it - header + kernel + initrd + cmdline,
	 * sector-padded exactly like the packer writes them (see
	 * bootblob.h). Without this, a corrupted header (bit rot, a bad
	 * block - not even necessarily anything adversarial) claiming
	 * sizes that overrun the partition's real extent would have every
	 * subsequent disk_read_lba() below silently read from whatever
	 * happens to follow the partition on disk instead of failing
	 * cleanly right here.
	 */
	if (1 + sectors_for(blob_hdr.kernel_size) + sectors_for(blob_hdr.initrd_size) +
	        sectors_for(blob_hdr.cmdline_size) >
	    blob_sectors)
		die("boot-blob header claims more data than its own partition holds");

	console_puts("alpine-zfsboot-bios: reading kernel header\n");
	/*
	 * setup_header itself is 123 bytes (see bootparams.h), starting at
	 * SETUP_HEADER_FILE_OFFSET (0x1f1 = byte 497 of the kernel file) -
	 * its own end (byte 620) is past the first 512-byte sector, so a
	 * few of its LATER fields (header/"HdrS" at 0x202, and everything
	 * after) land in the SECOND sector. Reading only 1 sector here
	 * left those fields reading as zero (g_chunk's own untouched,
	 * .bss-cleared tail) rather than the kernel's real bytes - not a
	 * disk-read or GPT-lookup bug (confirmed the hard way: blob_lba/
	 * kernel_lba and the first 16 bytes actually read - a real "MZ"
	 * EFI-stub signature - were both exactly right), just genuinely
	 * not enough of the file read to begin with. sectors_for() (this
	 * file's own helper, already used for kernel_size/initrd_size
	 * below) does the same ceiling-division here.
	 */
	for (i = 0; i < GPT_RETRY_ATTEMPTS; i++) {
		int j;

		if (disk_read_lba(kernel_lba, (uint16_t)sectors_for(SETUP_HEADER_FILE_OFFSET + sizeof(kernel_hdr)), g_chunk) != 0) {
			console_puts("alpine-zfsboot-bios: kernel header read failed, retrying\n");
			continue;
		}
		{
			const uint8_t *src = g_chunk + SETUP_HEADER_FILE_OFFSET;
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
	real_mode_bytes = real_mode_sectors * ZFSBOOT_BOOTBLOB_SECTOR_SIZE;
	if (real_mode_bytes >= blob_hdr.kernel_size)
		die("kernel file smaller than its own reported real-mode portion");
	protected_mode_size = blob_hdr.kernel_size - real_mode_bytes;

	/*
	 * Must happen before the first copy to any address >= 1MB
	 * (KERNEL_LOAD_ADDR is exactly 0x100000 - bit 20 is the only set
	 * bit): with A20 gated off, that write silently aliases back to
	 * physical 0, corrupting the IVT/BDA instead of actually landing
	 * at 1MB - a real, separate bug from the CD-ROM read issue this
	 * stage has otherwise been chasing, confirmed via SeaBIOS's own
	 * source: SeaBIOS unconditionally enables A20 for the DURATION of
	 * its own INT13h calls (see src/stacks.c's call32_prep()) and
	 * restores whatever it was before on return - it is NOT left on
	 * afterward just because a disk read happened to run. This
	 * project's own code before this point never asked for A20 itself.
	 * Called once, here, rather than inside unreal_copy() on every
	 * chunk: A20 has no "off" path anywhere after this in the whole
	 * boot (nothing re-disables it), so enabling it repeatedly per
	 * 8KB chunk would just be a wasted INT15h call every time.
	 */
	enable_a20();

	console_puts("alpine-zfsboot-bios: loading kernel");
	load_to_high(kernel_lba + sectors_for(real_mode_bytes), protected_mode_size, KERNEL_LOAD_ADDR);
	console_puts(" done\n");

	console_puts("alpine-zfsboot-bios: loading initrd");
	/*
	 * initrd_addr_max: the highest physical address the kernel says
	 * the initrd may safely occupy (older kernels report a value
	 * well under 4GB) - checked for real here rather than just
	 * assumed to always clear INITRD_LOAD_ADDR + its own size.
	 */
	if ((uint64_t)INITRD_LOAD_ADDR + blob_hdr.initrd_size > kernel_hdr.initrd_addr_max)
		die("initrd would exceed kernel's own initrd_addr_max");
	load_to_high(initrd_lba, blob_hdr.initrd_size, INITRD_LOAD_ADDR);
	console_puts(" done\n");

	if (disk_read_lba(cmdline_lba, (uint16_t)sectors_for(blob_hdr.cmdline_size), g_cmdline) != 0)
		die("could not read cmdline");
	g_cmdline[sizeof(g_cmdline) - 1] = '\0'; /* defensive - the packer already NUL-terminates within cmdline_size, this just bounds worst case */

	e820_count = e820_get_map(g_e820, BOOT_PARAMS_E820_MAX_ENTRIES);
	if (e820_count == 0)
		die("E820 memory map query failed - BIOS too old?");

	for (i = 0; i < (int)BOOT_PARAMS_SIZE; i++)
		g_boot_params[i] = 0;
	bootparams_build(g_boot_params, &kernel_hdr, INITRD_LOAD_ADDR, blob_hdr.initrd_size,
	                  phys_of(g_cmdline), g_e820, e820_count);

	console_puts("alpine-zfsboot-bios: starting kernel\n");
	jump_to_kernel(kernel_hdr.code32_start, phys_of(g_boot_params));
}
