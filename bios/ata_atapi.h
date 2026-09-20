#ifndef ZFSBOOT_BIOS_ATA_ATAPI_H
#define ZFSBOOT_BIOS_ATA_ATAPI_H

#include "stdint_local.h"

/*
 * A from-scratch ATA/ATAPI PIO driver, written directly against the
 * ATA/ATAPI command-set specification (the standard PIO task-file
 * register layout and the ATAPI PACKET-command protocol used to issue
 * SCSI command blocks like READ(10) over an ATA interface) - not
 * copied or ported from any other BIOS/OS driver's source, so this
 * project owns it outright under its own license. Scoped to exactly
 * what cdrom_disk.c needs: find the one ATAPI CD-ROM drive behind a
 * legacy (or PCI-native-compatible-mode) IDE controller, and read
 * single 2048-byte native sectors from it.
 *
 * Exists because this project's own exhaustive, real-hardware testing
 * established that INT 13h/AH=0x42 has no working read path at all
 * for a "no emulation" El Torito boot drive on this firmware (every
 * variable INT13h exposes - LBA, sector count, destination segment,
 * drive number - was tried and all fail identically with AH=0x0C; see
 * cdrom_disk.c's own header comment for the full account). Talking to
 * the ATA/ATAPI protocol directly lets this code see and clear the
 * actual SCSI condition (Unit Attention, Not Ready, etc.) that
 * INT13h's own generic error code hides.
 *
 * Scope, stated plainly: this covers an ATAPI CD-ROM attached to an
 * IDE controller in legacy-compatible port addressing (0x1F0/0x3F6
 * primary, 0x170/0x376 secondary) - the standard case for a BIOS/CSM
 * boot (real hardware in legacy mode, and QEMU/Proxmox's default IDE
 * CD-ROM bus). A CD-ROM attached via AHCI-native or virtio-scsi is
 * NOT found by this driver - that would need a second backend behind
 * the same interface below, out of scope unless actually needed.
 */

/*
 * Locates the ATAPI CD-ROM drive by probing both legacy IDE channels
 * (primary/secondary) and both drive selects (master/slave) via
 * IDENTIFY PACKET DEVICE, then clears any pending Unit Attention
 * condition (REQUEST SENSE after the first TEST UNIT READY, if
 * needed - a real, expected SCSI condition on first command, not a
 * failure). Must be called once before atapi_read_native(). Returns 0
 * if an ATAPI drive was found, -1 otherwise.
 */
int atapi_init(void);

/*
 * Upper bound on `count` for atapi_read_native() below: the ATAPI
 * PACKET protocol's own byte-count-limit (the value this driver loads
 * into the LBA_MID/LBA_HIGH task-file registers before issuing the
 * command, telling the device how much to send back in this data
 * phase) is a 16-bit field, max 0xFFFF. 32 native blocks would be
 * exactly 0x10000 (65536) - one past what a 16-bit register can hold,
 * wrapping to a byte-count-limit of 0, which the spec allows a device
 * to interpret as "no limit" but doesn't require - implementation-
 * defined behavior this driver has no way to verify across every
 * ATAPI device/hypervisor it might run on. 31 blocks (63488 bytes,
 * 0xF800) is the largest value that unambiguously fits, so that's the
 * real ceiling, not a round number chosen for its own sake.
 */
#define ATAPI_MAX_READ_BLOCKS 31

/*
 * Reads `count` (1..ATAPI_MAX_READ_BLOCKS) contiguous native
 * 2048-byte sectors starting at LBA `lba` into `buf` (which must be
 * at least count*2048 bytes) via a single ATAPI READ(10) command.
 * Returns 0 on success, -1 on any failure (timeout, ATA status ERR,
 * SCSI check condition after an internal retry, or count out of
 * range) - cdrom_disk.c's own native_read_batch() retries this at a
 * higher level, same as it retried the old INT13h call.
 *
 * Batching more than one native block per command matters for real
 * throughput, not just style: confirmed the hard way on real
 * hardware - reading one 2048-byte block per command for a ~70MB
 * kernel+initrd meant on the order of 35,000 separate PACKET
 * transactions, each paying its own fixed select/settle/wait
 * overhead regardless of how little data it actually moved, and was
 * unacceptably slow. Reading up to 31 blocks per command amortizes
 * that same fixed overhead across up to 31x as much data per
 * transaction instead.
 */
int atapi_read_native(uint32_t lba, uint16_t count, void *buf);

#endif
