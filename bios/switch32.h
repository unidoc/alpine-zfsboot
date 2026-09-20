#ifndef ZFSBOOT_BIOS_SWITCH32_H
#define ZFSBOOT_BIOS_SWITCH32_H

#include "stdint_local.h"

/*
 * The two places this stage ever touches a GDT/CR0 - real mode's own
 * addressing tops out at 1MB, but the kernel and initrd this stage
 * loads both need to end up ABOVE that line (0x100000 is where the
 * Linux/x86 boot protocol requires the kernel image itself), and the
 * final handoff into the kernel needs genuine 32-bit protected mode,
 * not real mode at all. Both of those are implemented in hand-written
 * assembly (switch32.S), not C: GCC's -m16 mode has no portable way
 * to express "load this exact GDT" or "toggle CR0.PE and immediately
 * reload a segment register with it" - this is the one piece of this
 * whole BIOS path that's unavoidably raw asm, kept as small and
 * self-contained as the protocol allows.
 */

/*
 * "Unreal mode" 32-bit-addressed copy: moves len bytes from src (a
 * plain real-mode-addressable pointer, always < 1MB) to dst (an
 * arbitrary 32-bit physical address, may be anywhere in the low 4GB,
 * including above 1MB) - via a brief, self-contained toggle into
 * protected mode just long enough to load a flat-4GB descriptor into
 * %es, then immediately back to real mode; the CPU never actually
 * runs any protected-mode CODE during this - only %es's cached
 * base/limit changes, a well-established, decades-old real-CPU
 * behavior every "unreal mode" bootloader relies on (not exotic or
 * hardware-specific). Returns control to the caller in real mode,
 * exactly as it found it. len does not need to be a multiple of 4.
 */
void unreal_copy(uint32_t dst, const void *src, uint32_t len);

/*
 * Enables the A20 gate - must be called at least once before ANY copy
 * to a physical address >= 1MB (unreal_copy()'s whole reason for
 * existing), or such a copy silently wraps back down to address 0
 * instead. Tries the documented, portable BIOS service first (INT 15h,
 * AX=2401h - "enable A20 gate", implemented by SeaBIOS's own
 * src/system.c), then unconditionally also pokes the chipset's fast-
 * A20 bit on port 0x92 - present on every real chipset and on QEMU's
 * own emulated one, safe to set even if the BIOS call already
 * succeeded (idempotent). Belt-and-suspenders on purpose: the same
 * dual approach real bootloaders (GRUB, syslinux) use, rather than
 * trusting a single method to be honored by every BIOS.
 */
void enable_a20(void);

/*
 * The final, one-way handoff into the kernel's own 32-bit entry
 * point - loads a real (this time permanent) GDT with flat code+data
 * descriptors, switches to protected mode for good, sets %esi to
 * boot_params (exactly what Documentation/arch/x86/boot.rst's
 * "32-bit boot protocol" specifies the kernel entry expects), and
 * jumps to entry. Never returns - the kernel takes over completely
 * from here, same as efi_main() calling StartImage() on the UEFI
 * path (see efi/main.c) never returning either.
 */
void jump_to_kernel(uint32_t entry, uint32_t boot_params) __attribute__((noreturn));

#endif
