#include "ata_atapi.h"
#include "console.h"

/*
 * See ata_atapi.h's own header comment for what this is and why it
 * exists. This file implements it: PIO-only (no DMA/bus-mastering,
 * no IRQ handling - the device's own interrupt line is masked off up
 * front and every step below is a plain polling loop, the only option
 * available this early with no interrupt infrastructure of our own
 * running yet), against the two fixed legacy IDE compatibility-mode
 * port ranges every PC chipset still decodes for backward
 * compatibility, real hardware in CSM/legacy mode included.
 */

#define ATA_IO_PRIMARY 0x1F0
#define ATA_CTRL_PRIMARY 0x3F6
#define ATA_IO_SECONDARY 0x170
#define ATA_CTRL_SECONDARY 0x376

/* Task-file register offsets from the channel's I/O base. */
#define ATA_REG_DATA 0
#define ATA_REG_FEATURES 1
#define ATA_REG_LBA_MID 4
#define ATA_REG_LBA_HIGH 5
#define ATA_REG_DEVHEAD 6
#define ATA_REG_STATUS 7
#define ATA_REG_COMMAND 7

#define ATA_STATUS_ERR 0x01
#define ATA_STATUS_DRQ 0x08
#define ATA_STATUS_BSY 0x80

/* Device Control register (the channel's separate control-block
 * port, not offset from the I/O base): bit1 disables the device's
 * IRQ line (nIEN - required here, since nothing services IRQs yet);
 * bit3 is reserved-but-must-be-one, a holdover from the original ATA-1
 * spec that every real controller still expects. */
#define ATA_DEVCTL_NIEN 0x02
#define ATA_DEVCTL_ONE 0x08

#define ATA_CMD_PACKET 0xA0
#define ATA_CMD_IDENTIFY_PACKET_DEVICE 0xA1

#define ATAPI_CMD_TEST_UNIT_READY 0x00
#define ATAPI_CMD_REQUEST_SENSE 0x03
#define ATAPI_CMD_READ10 0x28

#define NATIVE_SECTOR_SIZE 2048

/*
 * A plain iteration count, not a real time unit - deliberately NOT
 * the BIOS tick counter at 0040:006C this file used before: that
 * counter only advances while the timer IRQ is actually serviced,
 * which requires interrupts enabled (IF=1).
 *
 * UPDATE (F22, unidoc-alip's PR #5 review, catching a stale claim):
 * this comment used to say interrupts are left enabled for this
 * stage's entire execution, citing an `sti` in stage2_entry.S - that
 * `sti` is gone. stage2_entry.S's own current header comment states
 * plainly: interrupts stay OFF for this stage's entire execution now
 * (switch32.S's own brief cli/sti pair around its unreal-mode
 * transition is the one deliberate, narrowly-scoped exception). So
 * the tick counter is unusable here for an even more direct reason
 * than before: with IF=0, it simply never advances at all, not just
 * "advancing it would be hazardous."
 *
 * The real incident below is kept, not deleted - it's what a plain
 * spin count was ORIGINALLY chosen to close, and it's the reason
 * stage2_entry.S's own later "interrupts stay off" decision exists in
 * the first place; both fixes now independently prevent the same
 * class of hazard, and a spin count needs no interrupts serviced at
 * all to advance regardless of what a future change to this stage's
 * own IF policy might do. A real boot showed a struct field (GPT
 * header_size, written by a byte-copy loop immediately before being
 * compared) reading back as mismatched at the exact comparison
 * instruction, then reading back CORRECTLY moments later from the
 * SAME memory - with the actual compiled instruction proven (via
 * disassembly) to be a single direct memory-operand comparison, not a
 * cached register value a compiler could have hoisted. The only
 * remaining explanation, at the time (before interrupts were turned
 * off for the whole stage), was an asynchronous timer IRQ landing on
 * this stage's own stack between the two reads - the BIOS's own
 * default IRQ0 handler runs ON WHATEVER STACK WAS ACTIVE AT THE TIME,
 * which is this stage's own SS:SP, not a separate one. Generous (tens
 * of millions of port reads - each one taking at least one full
 * PCI/ISA bus cycle) for a PIO handshake that normally completes in
 * well under a millisecond.
 */
#define ATA_TIMEOUT_SPINS 20000000UL

static uint16_t g_io_base;
static uint16_t g_ctrl_base;
static uint8_t g_dev_select;
static int g_found;

static inline void outb(uint16_t port, uint8_t val)
{
	__asm__ __volatile__("outb %0, %%dx" : : "a"(val), "d"(port));
}

static inline uint8_t inb(uint16_t port)
{
	uint8_t val;
	__asm__ __volatile__("inb %%dx, %0" : "=a"(val) : "d"(port));
	return val;
}

static inline void outw(uint16_t port, uint16_t val)
{
	__asm__ __volatile__("outw %0, %%dx" : : "a"(val), "d"(port));
}

static inline uint16_t inw(uint16_t port)
{
	uint16_t val;
	__asm__ __volatile__("inw %%dx, %0" : "=a"(val) : "d"(port));
	return val;
}

static int wait_status_clear(uint16_t io_base, uint8_t mask)
{
	unsigned long spins;

	for (spins = 0; spins < ATA_TIMEOUT_SPINS; spins++) {
		if (!(inb(io_base + ATA_REG_STATUS) & mask))
			return 0;
		/*
		 * `pause` - the standard x86 spin-wait hint (encodes as `rep
		 * nop`, so it's a valid no-op on any CPU old enough not to
		 * recognize it specially, not something that needs an ISA
		 * feature check). Under a hypervisor specifically, this is
		 * more than a hint: confirmed via this project's own phase-
		 * level timing (wait_for_data, in atapi_send_packet(),
		 * measuring exactly this kind of wait) that a real, sometimes
		 * very large, share of ATAPI command time is spent right here
		 * - waiting for the virtual device to actually finish
		 * servicing the request, not executing any of this project's
		 * own code at all. A tight loop with no `pause` spins this
		 * vCPU at 100% the entire time, which on a busy/oversubscribed
		 * host can itself compete with the host's own thread that's
		 * trying to complete the very I/O this loop is waiting on -
		 * `pause` is the documented, standard way to tell the CPU
		 * (and, under virtualization, the hypervisor) "this is a
		 * spin-wait, feel free to let something else run instead of
		 * treating this core as fully busy."
		 */
		__asm__ __volatile__("pause");
	}
	return -1;
}

/*
 * The conventional ~400ns settle after writing a command/select
 * register, before the first status sample is trusted at all - the
 * same throwaway-alt-status-read trick probe_drive() already used for
 * device selection, needed here too: a device takes real (if short)
 * time to actually raise BSY after the command register is written,
 * and sampling status in that gap reads STALE bits left over from
 * whatever the task-file registers said before this command (in
 * particular: BSY=0 with DRQ still set from the *previous* phase) -
 * wait_status_clear() would then return instantly on a status that
 * was never really "ready", not caught by any error check at all.
 */
static void io_settle(uint16_t ctrl_base)
{
	int i;

	for (i = 0; i < 4; i++)
		(void)inb(ctrl_base);
}

/*
 * Advances *data_buf and *got_words by exactly one DRQ phase's worth
 * (take words = take*2 bytes) - the ONLY place either counter moves in
 * atapi_send_packet()'s data-phase loop. Deliberately pure C (no asm,
 * no port I/O, no BIOS/hardware dependency of any kind) so it's
 * host-buildable and unit-testable on its own, driven through a
 * scripted multi-phase `take` sequence - see
 * bios/tests/ata_atapi_host_test.c. This is what makes "did the
 * pointer/counter arithmetic across N phases end up exactly right"
 * checkable without real ATAPI hardware or a hypervisor that happens
 * to split a transfer across phases (QEMU/SeaBIOS never do, which is
 * exactly how a previous double-advance bug here went undetected for
 * a whole session - see atapi_send_packet's own comment at its call
 * site).
 */
void atapi_advance_after_phase(uint8_t **data_buf, uint16_t *got_words, uint16_t take)
{
	*data_buf += (uint32_t)take * 2;
	*got_words += take;
}

/*
 * atapi_transfer_complete GOT_WORDS WANT_WORDS - true (nonzero) only if
 * the data phase actually delivered everything the CDB requested.
 * Same reasoning and same host-testability goal as
 * atapi_advance_after_phase above, extracted for the identical reason:
 * a full source audit found atapi_send_packet()'s own data-phase loop
 * trusted "the device says DRQ is clear now" (BSY=0/DRQ=0/ERR=0) as
 * the ONLY completion signal, with nothing checking that got_words had
 * actually reached want_words first. A device ending the data phase
 * early, before delivering everything the CDB itself requested, is a
 * genuine malfunction per the ATA/ATAPI PACKET command's own contract
 * - but without this check, atapi_send_packet() returned SUCCESS
 * regardless, leaving the destination buffer's own tail as whatever
 * stale/uninitialized bytes were already there, silently treated as
 * real kernel/initrd content by every caller. This one-line comparison
 * has no asm/port I/O in it either, so - like the sibling function
 * above - it's host-buildable and unit-testable without real ATAPI
 * hardware, which the surrounding function's own real inb/outb/insw
 * instructions are not (privileged x86 I/O instructions - they fault
 * outright on a plain host process with no ATAPI device behind them).
 */
int atapi_transfer_complete(uint16_t got_words, uint16_t want_words)
{
	return got_words == want_words;
}

/*
 * The core ATAPI PACKET-command transaction: select-and-issue, wait
 * for the command/data request phase, push the 12-byte CDB, then (for
 * a data-in command) read back whatever the device reports it
 * actually has, up to what the caller asked for. Every failure branch
 * (timeout, ERR status, missing DRQ where expected) returns -1 with
 * no further detail - the caller's job (atapi_read_native() below,
 * and atapi_init()'s own readiness check) is deciding what to do
 * next, not this function's.
 */
#ifdef ZFSBOOT_TEST_SERIAL
/*
 * Test-only call-site trace for atapi_send_packet()'s five internal
 * wait_status_clear() calls, gated exactly like console.c's own serial
 * mirror (never defined by a real build). Exists because a real
 * register dump at a CI timeout cannot tell these five calls apart:
 * `io + ATA_REG_STATUS` is the same loop-invariant address at every one
 * of them, so EIP/registers alone only say "stuck somewhere in this
 * function", not which wait. Prints a single letter right before each
 * call (no matching "site done" after a hang, by construction) plus the
 * CDB's opcode byte at entry, so the NEXT CI failure's serial.log names
 * the exact stuck call instead of leaving it ambiguous.
 */
static void trace_site(char c)
{
	console_putc(c);
}
#else
#define trace_site(c) ((void)0)
#endif

static int atapi_send_packet(const uint8_t cdb[12], uint8_t *data_buf, uint16_t data_len)
{
	uint16_t io = g_io_base;
	int i;
	uint8_t st;

#ifdef ZFSBOOT_TEST_SERIAL
	console_puts("PKT op=");
	console_puts_hex32(cdb[0]);
	console_putc(' ');
#endif

	trace_site('a');
	if (wait_status_clear(io, ATA_STATUS_BSY) != 0)
		return -1;

	/* Select the drive first (both task-file registers below are
	 * shared per-channel state, latched against whichever drive is
	 * selected when the command register is finally written - but
	 * selecting first, then settling, then setting them is the
	 * conventional order every reference sequence uses, so this
	 * follows it rather than relying on that latching detail). */
	outb(io + ATA_REG_DEVHEAD, g_dev_select);
	io_settle(g_ctrl_base);
	trace_site('b');
	if (wait_status_clear(io, ATA_STATUS_BSY) != 0)
		return -1;

	outb(io + ATA_REG_FEATURES, 0); /* PIO, no DMA/overlap */
	/* Byte-count limit for the data phase - set to exactly what this
	 * transaction expects so the device signals it all in one DRQ
	 * burst instead of splitting across several this code would then
	 * have to loop over. */
	outb(io + ATA_REG_LBA_MID, (uint8_t)(data_len & 0xFF));
	outb(io + ATA_REG_LBA_HIGH, (uint8_t)(data_len >> 8));

	outb(io + ATA_REG_COMMAND, ATA_CMD_PACKET);
	/*
	 * Settle before trusting status at all - confirmed the hard way,
	 * on real hardware: without this, the very first status sample
	 * below can still read BSY=0/DRQ=1 left over from BEFORE this
	 * command was issued (stale, not this command's real answer),
	 * which made every read "succeed" while silently returning wrong
	 * (uninitialized/stale-buffer) content instead of a real error.
	 */
	io_settle(g_ctrl_base);

	/* Command phase: BSY clears, then the device raises DRQ to ask
	 * for the CDB (or sets ERR if it's rejecting the command
	 * outright). */
	trace_site('c');
	if (wait_status_clear(io, ATA_STATUS_BSY) != 0)
		return -1;
	st = inb(io + ATA_REG_STATUS);
	if ((st & ATA_STATUS_ERR) || !(st & ATA_STATUS_DRQ))
		return -1;

	for (i = 0; i < 6; i++) {
		uint16_t w = (uint16_t)cdb[i * 2] | ((uint16_t)cdb[i * 2 + 1] << 8);
		outw(io + ATA_REG_DATA, w);
	}
	/* Same settle, after the CDB push - the device needs a moment to
	 * act on it (fetch the target sector, or reject it) before BSY/DRQ
	 * reflect that instead of the command phase's own stale bits. */
	io_settle(g_ctrl_base);

	if (data_len == 0) {
		/* No data phase (TEST UNIT READY) - just the completion
		 * status. */
		trace_site('d');
		if (wait_status_clear(io, ATA_STATUS_BSY) != 0)
			return -1;
#ifdef ZFSBOOT_TEST_SERIAL
		console_puts("ok\n");
#endif
		return (inb(io + ATA_REG_STATUS) & ATA_STATUS_ERR) ? -1 : 0;
	}

	/*
	 * Data phase(s): the byte-count-limit programmed above is a
	 * ceiling on ONE phase, not a guarantee the device delivers
	 * everything in exactly one - the ATA/ATAPI spec explicitly allows
	 * a device to split a large transfer across several DRQ phases,
	 * each with its own (possibly smaller) actual_len, and this
	 * project's own batching (multiple native sectors per command,
	 * see cdrom_disk.c's own NATIVE_BATCH) makes that a real
	 * possibility rather than a theoretical one to hedge against.
	 * Loop until the device itself signals completion (BSY clears
	 * with DRQ also clear) rather than stopping after the first phase
	 * and assuming that was everything.
	 */
	{
		uint16_t want_words = data_len / 2;
		uint16_t got_words = 0;

		for (;;) {
			uint16_t actual_len, actual_words, remaining_want, take;

			trace_site('e');
			if (wait_status_clear(io, ATA_STATUS_BSY) != 0)
				return -1;
			st = inb(io + ATA_REG_STATUS);
			if (st & ATA_STATUS_ERR)
				return -1;
			if (!(st & ATA_STATUS_DRQ))
				break; /* device says the command is complete */

			/* The device reports how many bytes it's actually
			 * sending back THIS PHASE in the same two registers the
			 * byte-count LIMIT was written to above - read it back
			 * rather than assuming it matches what was asked for. */
			actual_len = (uint16_t)inb(io + ATA_REG_LBA_MID) |
			            ((uint16_t)inb(io + ATA_REG_LBA_HIGH) << 8);
			actual_words = actual_len / 2;
			remaining_want = (got_words < want_words) ? (want_words - got_words) : 0;
			take = actual_words < remaining_want ? actual_words : remaining_want;

			/*
			 * `rep insw` instead of a manual per-word inw() loop: a
			 * plain loop issues one IN instruction per 16-bit word,
			 * and under KVM/QEMU each one is its own VM exit (a real,
			 * measured cost, not a theoretical one - confirmed the
			 * hard way: batching multiple native sectors per ATAPI
			 * command alone, without this, still left kernel/initrd
			 * loading unacceptably slow, since that only amortizes
			 * the fixed per-COMMAND overhead, not the per-WORD
			 * transfer cost that dominates for any real transfer
			 * size). A single `rep insw` lets the hypervisor service
			 * the whole burst as one string-instruction trap instead
			 * of `take` separate ones - the same optimization every
			 * real BIOS's own legacy PIO IDE path relies on.
			 *
			 * `addr32`: required here for the same reason
			 * stage2_entry.S's own bss-zeroing `addr32 rep stosb`
			 * needs it - this whole translation unit is assembled in
			 * GAS's .code16gcc mode (-m16, see Makefile), where a
			 * bare string instruction's implicit address/count
			 * registers default to DI/CX (16-bit), not EDI/ECX,
			 * unless this prefix says otherwise; `data_buf`/`take`
			 * are ordinary ints (32-bit, confirmed via DWARF earlier
			 * in this project's own investigation) and the C operand
			 * constraints below put them in EDI/ECX, so the
			 * instruction itself must be told to use the same width.
			 * `cld` first: guarantees the forward (incrementing)
			 * direction this depends on, regardless of any assumption
			 * about DF's state elsewhere in this stage.
			 */
			if (take > 0) {
				/* asm_dst is a SCRATCH copy, deliberately separate from
				 * data_buf: "+D"(asm_dst) below is a read-write operand
				 * bound to EDI, and `rep insw` itself increments EDI by
				 * 2 bytes per word transferred - GCC writes EDI back
				 * into whatever C variable is bound to "+D" as part of
				 * honoring that constraint. Binding that straight to
				 * data_buf (this function's own previous shape) meant
				 * data_buf was advanced ONCE by the asm block's own
				 * side effect, invisibly - and a second, explicit
				 * `data_buf += take * 2` after it (also this function's
				 * own previous shape) double-advanced the pointer on
				 * every phase after the first. Harmless for a single-
				 * DRQ-phase transfer (the only shape ever exercised
				 * under QEMU/SeaBIOS, which is why this went unnoticed
				 * for a whole session), but on a real multi-phase
				 * READ(10) it scatters phase N's data at 2x the correct
				 * stride, eventually running the destination pointer
				 * into whatever memory follows g_native_buf (this
				 * driver's own port-base globals) and off the end of
				 * the ES segment.
				 *
				 * The fix is architectural, not just deleting the extra
				 * line: data_buf/got_words are now advanced in EXACTLY
				 * ONE place, atapi_advance_after_phase() below - a
				 * small, pure, host-testable function with no asm and
				 * no port I/O in it at all - so the asm block's own
				 * destructive EDI write-back can never again collide
				 * with a second advance. See
				 * bios/tests/ata_atapi_host_test.c, which drives that
				 * function through a real multi-phase sequence (the
				 * shape QEMU/SeaBIOS never exercises) and would have
				 * caught this. */
				uint8_t *asm_dst = data_buf;
				uint16_t words = take;

				__asm__ __volatile__(
					"cld\n\t"
					"addr32 rep insw"
					: "+D"(asm_dst), "+c"(words)
					: "d"(io + ATA_REG_DATA)
					: "memory"
				);
				atapi_advance_after_phase(&data_buf, &got_words, take);
			}
			/* Drain anything THIS PHASE reported beyond what's still
			 * wanted - the phase isn't complete until every byte it
			 * announced has been read, even the part this call has
			 * no room left for. */
			for (i = (int)take; i < (int)actual_words; i++)
				(void)inw(io + ATA_REG_DATA);
		}

		/* See atapi_transfer_complete's own comment for the real gap
		 * this closes - the loop's own exit condition alone never
		 * proved the full requested transfer actually happened. */
		if (!atapi_transfer_complete(got_words, want_words))
			return -1;
	}

#ifdef ZFSBOOT_TEST_SERIAL
	console_puts("ok\n");
#endif
	return 0;
}

static int atapi_test_unit_ready(void)
{
	uint8_t cdb[12] = { 0 };

	return atapi_send_packet(cdb, 0, 0);
}

static int atapi_request_sense(uint8_t *sense_key, uint8_t *asc, uint8_t *ascq)
{
	uint8_t cdb[12] = { 0 };
	uint8_t buf[18];
	int i;

	for (i = 0; i < (int)sizeof(buf); i++)
		buf[i] = 0;
	cdb[0] = ATAPI_CMD_REQUEST_SENSE;
	cdb[4] = sizeof(buf);
	if (atapi_send_packet(cdb, buf, sizeof(buf)) != 0)
		return -1;

	*sense_key = buf[2] & 0x0F;
	*asc = buf[12];
	*ascq = buf[13];
	return 0;
}

int atapi_read_native(uint32_t lba, uint16_t count, void *buf)
{
	uint8_t cdb[12] = { 0 };

	if (!g_found)
		return -1;
	if (count == 0 || count > ATAPI_MAX_READ_BLOCKS)
		return -1;

	cdb[0] = ATAPI_CMD_READ10;
	cdb[2] = (uint8_t)(lba >> 24);
	cdb[3] = (uint8_t)(lba >> 16);
	cdb[4] = (uint8_t)(lba >> 8);
	cdb[5] = (uint8_t)lba;
	cdb[7] = (uint8_t)(count >> 8); /* transfer length, big-endian u16 */
	cdb[8] = (uint8_t)count;

	/*
	 * count*NATIVE_SECTOR_SIZE never exceeds 31*2048 = 63488 (checked
	 * above via ATAPI_MAX_READ_BLOCKS), well within the 16-bit
	 * data_len atapi_send_packet()/the byte-count-limit registers it
	 * programs can hold - see ATAPI_MAX_READ_BLOCKS's own comment for
	 * why 31 is the real ceiling, not 32.
	 */
	return atapi_send_packet(cdb, (uint8_t *)buf, (uint16_t)(count * NATIVE_SECTOR_SIZE));
}

/*
 * Probes one drive select on one channel: select it, give it the
 * conventional 400ns settle time (four throwaway status reads - the
 * same trick every real ATA driver uses since there's no sub-tick
 * timer to actually wait 400ns with), then send IDENTIFY PACKET
 * DEVICE. A real ATAPI device answers with DRQ and 256 words of
 * identify data (read here and discarded - this driver only needs to
 * know the device is present and is a PACKET device, not any of its
 * reported geometry/strings); anything else (timeout, ERR, no DRQ)
 * means "nothing useful here", not a fatal condition - the caller
 * just moves on to the next candidate.
 */
static int probe_drive(uint16_t io_base, uint16_t ctrl_base, uint8_t dev_select)
{
	int i;
	uint8_t st;

	outb(ctrl_base, ATA_DEVCTL_ONE | ATA_DEVCTL_NIEN);
	outb(io_base + ATA_REG_DEVHEAD, dev_select);
	for (i = 0; i < 4; i++)
		(void)inb(ctrl_base);

	/*
	 * A floating (no controller/no device) bus reads back as all-1s or
	 * all-0s depending on the board - checked here, before the real
	 * wait_status_clear() timeout below, so an absent channel/select
	 * fails in a handful of port reads instead of burning the full
	 * ATA_TIMEOUT_TICKS budget for every one of the (up to 4) candidates
	 * this project doesn't find a device on.
	 */
	st = inb(io_base + ATA_REG_STATUS);
	if (st == 0xFF || st == 0x00)
		return -1;

	if (wait_status_clear(io_base, ATA_STATUS_BSY) != 0)
		return -1;

	outb(io_base + ATA_REG_COMMAND, ATA_CMD_IDENTIFY_PACKET_DEVICE);
	io_settle(ctrl_base);
	if (wait_status_clear(io_base, ATA_STATUS_BSY) != 0)
		return -1;

	st = inb(io_base + ATA_REG_STATUS);
	if ((st & ATA_STATUS_ERR) || !(st & ATA_STATUS_DRQ))
		return -1;

	for (i = 0; i < 256; i++)
		(void)inw(io_base + ATA_REG_DATA);

	return 0;
}

int atapi_init(void)
{
	static const struct {
		uint16_t io_base;
		uint16_t ctrl_base;
	} channels[2] = {
		{ ATA_IO_PRIMARY, ATA_CTRL_PRIMARY },
		{ ATA_IO_SECONDARY, ATA_CTRL_SECONDARY },
	};
	int ch, slave;

	g_found = 0;

	/*
	 * Deliberately no `sti` here (an earlier version of this file had
	 * one, to make the BIOS tick counter advance for timeout purposes)
	 * - every wait loop in this file now bounds itself on a plain spin
	 * count instead (see ATA_TIMEOUT_SPINS's own comment for exactly
	 * why: interrupts staying enabled for this driver's own operation
	 * meant they stayed enabled for the REST of this stage's execution
	 * too, since nothing here is scoped to turn them back off - a real,
	 * confirmed-the-hard-way hazard, not a theoretical one). This
	 * driver needs interrupts disabled, not enabled: it never uses a
	 * BIOS service, and the device's own IRQ line is separately masked
	 * off per-channel in probe_drive() below regardless.
	 */

	for (ch = 0; ch < 2; ch++) {
		for (slave = 0; slave < 2; slave++) {
			uint8_t dev_select = slave ? 0xB0 : 0xA0;

			if (probe_drive(channels[ch].io_base, channels[ch].ctrl_base, dev_select) != 0)
				continue;

			g_io_base = channels[ch].io_base;
			g_ctrl_base = channels[ch].ctrl_base;
			g_dev_select = dev_select;
			g_found = 1;

			console_puts("alpine-zfsboot-bios: ATAPI CD-ROM found, io_base=");
			console_puts_hex32((uint32_t)g_io_base);
			console_puts(slave ? " slave\n" : " master\n");

			/*
			 * A fresh device commonly reports a Unit Attention
			 * condition (power-on / possible medium-change
			 * notification) on its first command - a real, expected
			 * SCSI state, not a failure. One REQUEST SENSE reports
			 * and clears it; everything after should proceed
			 * normally.
			 */
			if (atapi_test_unit_ready() != 0) {
				uint8_t sense_key = 0, asc = 0, ascq = 0;

				console_puts("alpine-zfsboot-bios: ATAPI TEST UNIT READY failed, requesting sense\n");
				if (atapi_request_sense(&sense_key, &asc, &ascq) == 0) {
					console_puts("alpine-zfsboot-bios: ATAPI sense key=");
					console_puts_hex32((uint32_t)sense_key);
					console_puts(" asc=");
					console_puts_hex32((uint32_t)asc);
					console_puts(" ascq=");
					console_puts_hex32((uint32_t)ascq);
					console_puts("\n");
				}
				if (atapi_test_unit_ready() != 0)
					console_puts("alpine-zfsboot-bios: ATAPI still not ready after sense - continuing anyway\n");
			}

			return 0;
		}
	}

	console_puts("alpine-zfsboot-bios: no ATAPI CD-ROM found on legacy IDE ports\n");
	return -1;
}
