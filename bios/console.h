#ifndef ZFSBOOT_BIOS_CONSOLE_H
#define ZFSBOOT_BIOS_CONSOLE_H

/*
 * The ONLY visible feedback this whole stage has - INT 0x10, AH=0x0E
 * "teletype output", the simplest possible BIOS text service (no
 * cursor-position tracking needed, no video-mode setup: print this
 * character wherever the cursor already is and advance it). Every
 * real milestone this whole project has hit depended on visible
 * boot-time output to debug (see efi/main.c's own heavy use of
 * Print() for the same reason) - a from-scratch bootloader with zero
 * visible output would be undebuggable on real hardware, hence this
 * exists from the first version of this code, not bolted on later
 * once something mysteriously failed with no way to tell why.
 */
void console_putc(char c);
void console_puts(const char *s);
void console_puts_hex32(unsigned long v);

#endif
