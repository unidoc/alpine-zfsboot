#include "stdint_local.h"

#include "bootparams.h"
#include "build_id.h"
#include "console.h"
#include "disk.h"
#include "e820.h"
#include "fat.h"
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

static uint8_t g_boot_params[BOOT_PARAMS_SIZE];
static uint8_t g_cmdline[CMDLINE_BUF_SIZE];
static uint8_t g_header_buf[SETUP_HEADER_FILE_OFFSET + sizeof(struct setup_header)];
static struct boot_e820_entry g_e820[BOOT_PARAMS_E820_MAX_ENTRIES];

static const uint8_t esp_type_guid[16] = ZFSBOOT_ESP_TYPE_GUID_BYTES;

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
		struct progress_state st = { 0 };
		if (fat_read_range(&vol, &kernel_file, real_mode_bytes, protected_mode_size, KERNEL_LOAD_ADDR,
		                    progress_dot, &st) != 0)
			die("FAT read failed loading kernel");
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
		struct progress_state st = { 0 };
		if (fat_read_range(&vol, &initrd_file, 0, initrd_file.size, INITRD_LOAD_ADDR,
		                    progress_dot, &st) != 0)
			die("FAT read failed loading initrd");
	}
	console_puts(" done\n");

	if (fat_read_range(&vol, &cmdline_file, 0, cmdline_file.size, phys_of(g_cmdline), 0, 0) != 0)
		die("FAT read failed loading cmdline");
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

	e820_count = e820_get_map(g_e820, BOOT_PARAMS_E820_MAX_ENTRIES);
	if (e820_count == 0)
		die("E820 memory map query failed - BIOS too old?");

	for (i = 0; i < (int)BOOT_PARAMS_SIZE; i++)
		g_boot_params[i] = 0;
	bootparams_build(g_boot_params, &kernel_hdr, INITRD_LOAD_ADDR, initrd_file.size,
	                  phys_of(g_cmdline), g_e820, e820_count);

	console_puts("alpine-zfsboot-bios: starting kernel\n");
	jump_to_kernel(kernel_hdr.code32_start, phys_of(g_boot_params));
}
