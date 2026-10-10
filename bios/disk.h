#ifndef ZFSBOOT_BIOS_DISK_H
#define ZFSBOOT_BIOS_DISK_H

#include "stdint_local.h"

/*
 * Plain BIOS INT 0x13 disk reads - deliberately the ONLY thing in
 * this file, kept apart from switch32.S's own GDT/CR0 trickery (see
 * that file's header comment): a real-mode BIOS interrupt call needs
 * none of that, so it stays ordinary, easy-to-review C + inline asm.
 */

#define DISK_SECTOR_SIZE 512

/*
 * Records the BIOS drive number (e.g. 0x80 for the first hard disk) -
 * stage1 receives this from the BIOS itself in %dl at boot (before
 * anything has a chance to clobber it) and stage2_entry.S passes it
 * through as stage2_main()'s own argument; this must be called before
 * disk_read_lba() below.
 */
void disk_init(uint8_t drive_number); /* disk_select(); see g_disk_info */

/*
 * Reads `count` sectors starting at LBA `lba` into `buf`, via the
 * BIOS INT 0x13, AH=0x42 "extended read" service (the LBA-capable
 * one), or - only on a BIOS where AH=0x42 does not work at all (see
 * disk_select()) - the old CHS-based AH=0x02, which cannot address a
 * disk past ~8GB. `buf` must be real-mode-addressable - a
 * plain near pointer into this stage's own data area (below 1MB,
 * within the current %ds) - never a >1MB address; see switch32.S's
 * unreal_copy() for getting data from a low buffer like this one up
 * to its final >1MB destination.
 *
 * Returns 0 on success, -1 on failure. A request is never trusted to
 * be one BIOS call: it is split into chunks of at most the current
 * chunk size (the cap: DISK_MAX_CHUNK_SECTORS, or alpine-zfsboot.int13chunk,
 * never above what the BIOS interface allows). When a call
 * fails, the controller is reset and the same sectors are retried with
 * HALF the chunk size, down to one sector, and the smaller size is
 * kept for the rest of the boot (disk_notify_shrink() tells the
 * operator). Only a failure at one sector, twice, is final. What failed
 * is left in g_disk_last_error. Implemented in disk_policy.c (plain C,
 * host-testable) on top of disk_int13_read()/disk_int13_reset() (disk.c,
 * the only code that touches the BIOS) - the ISO build has its own
 * disk_read_lba() in cdrom_disk.c.
 */
int disk_read_lba(uint64_t lba, uint16_t count, void *buf);

/*
 * The INT 13h transfer cap: the largest chunk disk_read_lba() starts with
 * and never exceeds. Build-time default (override with
 * -DDISK_MAX_CHUNK_SECTORS=N); a boot can set it with
 * alpine-zfsboot.int13chunk=N in EFI/ALPINE/CMDLINE (disk_set_chunk(),
 * from stage2_main.c - which reads the cmdline only after the GPT/MBR and
 * FAT metadata reads, so those few small reads always use this default).
 * Never above 32 sectors (16 KiB) by default: one hypothesis for a
 * physical-host failure is a BIOS that mishandles large transfers, and a
 * BIOS that returns wrong data with CF=0 is not caught by the
 * halve-on-failure logic. 16 was chosen by measurement (README, "INT 13h
 * transfer size").
 */
#ifndef DISK_MAX_CHUNK_SECTORS
#define DISK_MAX_CHUNK_SECTORS 16
#endif
/* The largest cap a boot may ask for (alpine-zfsboot.int13chunk=1..127):
 * 127 sectors is the largest transfer the EDD 1.1 spec lets every BIOS
 * accept in one AH=42h call. */
#define DISK_CHUNK_LIMIT 127
#if DISK_MAX_CHUNK_SECTORS < 1 || DISK_MAX_CHUNK_SECTORS > DISK_CHUNK_LIMIT
#error "DISK_MAX_CHUNK_SECTORS must be 1..127"
#endif

/* Sets the cap (1..DISK_CHUNK_LIMIT; anything else is ignored; never above
 * what the drive's BIOS interface allows, g_disk_bios_limit) for the rest
 * of the boot; the halve-on-failure policy then starts from it. */
void disk_set_chunk(uint16_t sectors);

/* The chunk in use now (the cap, or less after a shrink). */
uint16_t disk_get_chunk(void);

/* The largest transfer the current drive's BIOS interface allows (set by
 * disk_select(): 127 for EDD AH=42h - the EDD spec's limit; INT 13h
 * AH=41h/48h report no maximum transfer of their own - or the sectors
 * per track for CHS AH=02h). */
extern uint16_t g_disk_bios_limit;

/*
 * What disk_select() learned about a drive: INT 13h AH=41h (extensions
 * present? which EDD version?), AH=48h (bytes per sector, information
 * flags) and, only without extensions, AH=08h (CHS geometry).
 */
struct disk_info {
	uint8_t drive;
	uint8_t mode;              /* DISK_MODE_LBA (AH=42h) or DISK_MODE_CHS (AH=02h) */
	uint8_t edd;               /* AH=41h AH: EDD version (0x01 1.x, 0x20 2.0, 0x21 EDD-1.1, 0x30 3.0); 0 = none */
	uint16_t api;              /* AH=41h CX: interface subsets (bit 0 = AH=42h..48h) */
	uint16_t flags;            /* AH=48h information flags (bit 0: DMA boundary errors handled by the BIOS) */
	uint16_t bytes_per_sector; /* AH=48h; 512 when AH=48h is missing or says 0 */
	uint16_t heads, spt;       /* AH=08h, CHS mode only */
	uint16_t cyls;
};
#define DISK_MODE_LBA 1
#define DISK_MODE_CHS 2
extern struct disk_info g_disk_info;

/*
 * Probes `drive` (AH=41h, AH=48h, and AH=08h when there are no
 * extensions) and makes it the drive every later read uses. AH=42h is
 * used whenever it works - also if AH=41h denies the extensions but a
 * one-sector AH=42h read of LBA 0 succeeds, exactly as before this probe
 * existed; AH=02h (CHS) only when AH=42h does not work at all (three tries
 * with a reset). Returns 0; 1 when AH=48h reports sectors of other than
 * 512 bytes - the drive is still set up and read in 512-byte units, as
 * stage1 read it to load this stage (the on-disk layout - stage1's LBA 34,
 * the GPT and FAT - is in 512-byte units; see README), and the caller
 * decides (the boot drive: a warning; another drive: not used); -1 when
 * the drive has no working read interface at all.
 */
int disk_select(uint8_t drive);
uint8_t disk_current(void); /* the drive disk_select() made current */

/* INT 13h AH=00h for the current drive (also the ISO stage's). */
void disk_reset(void);

/* ISO stage only: 1 when the ATAPI driver (not INT 13h) serves the reads. */
int disk_cd_atapi(void);

/* LBA -> CHS for AH=02h (pure, host-tested): 0 and cylinder, head and sector plus how
 * many sectors are left in that track, or -1 when the LBA lies beyond
 * what CHS can address (cylinder > 1023 or > the drive's own). */
int disk_lba_to_chs(uint64_t lba, uint16_t heads, uint16_t spt, uint16_t cyls,
                    uint16_t *c, uint8_t *h, uint8_t *s, uint16_t *left);

/* Counters for the diagnostic dump. */
struct disk_stats {
	uint32_t calls;   /* INT 13h read calls, retries included */
	uint16_t cap;     /* the configured cap */
	uint16_t chunk;   /* the chunk in use now (below cap after a shrink) */
	uint16_t largest; /* the largest transfer that succeeded */
	uint16_t failures; /* BIOS calls that failed (and were retried) */
};
extern struct disk_stats g_disk_stats;

/* disk_int13_read()'s return value for "the BIOS said success but moved
 * fewer sectors than asked" - every other nonzero value is the BIOS's own
 * status byte (AH), e.g. 0x09 "data boundary error", 0x01 "invalid
 * command", 0x20 "controller failure", 0x40 "seek failed". */
#define DISK_ERR_SHORT 0x100

/* One INT 13h AH=42h call for `count` sectors: 0, or the BIOS status (AH) /
 * DISK_ERR_SHORT. No retry, no splitting. */
int disk_int13_read(uint64_t lba, uint16_t count, void *buf);

/* INT 13h AH=00h (reset disk system) for the boot drive; best effort. */
void disk_int13_reset(void);

/* The most recent failed read, for the fatal messages in stage2_main.c.
 * status 0 means "nothing has failed". */
struct disk_error {
	uint16_t status; /* BIOS status (AH) or DISK_ERR_SHORT */
	uint64_t lba;    /* where the failing call started */
	uint16_t count;  /* how many sectors that call asked for */
	uint16_t chunk;  /* the chunk size disk_read_lba() is using now */
};
extern struct disk_error g_disk_last_error;

/* Called by disk_read_lba() when a failure makes it shrink its chunk size
 * (defined in stage2_main.c: prints one line, so the operator can see that
 * the BIOS did not accept the larger transfer). */
void disk_notify_shrink(const struct disk_error *e, uint16_t new_chunk);

/* Called by disk_read_lba() for EVERY failed BIOS call, before it retries
 * (defined in stage2_main.c: prints it when diagnostics are on - see
 * BIOS_DIAG there - and does nothing otherwise). `attempt` counts the
 * failures within the current disk_read_lba() request, from 1. */
void disk_notify_failure(const struct disk_error *e, uint8_t attempt);

/* What one INT 13h AH=42h call's outcome means (disk_policy.c, pure, so the
 * host test covers it): `failed` = the carry flag, `ax` = AX after the call,
 * `dap_count` = the DAP's num_blocks after the call, `count` = what was
 * asked for. 0, the BIOS status (0xFF if carry was set with AH=0), or
 * DISK_ERR_SHORT for a carry-clear call whose DAP reports fewer sectors. */
int disk_int13_status(uint8_t failed, uint16_t ax, uint16_t dap_count, uint16_t count);

#endif
