#ifndef ZFSBOOT_BIOS_E820_H
#define ZFSBOOT_BIOS_E820_H

#include "stdint_local.h"
#include "bootparams.h"

/*
 * Gathers the BIOS memory map via INT 0x15, EAX=0xE820 - real mode,
 * no GDT tricks needed (see switch32.h for the two things that DO
 * need those). Fills `entries` (caller-supplied, up to `max` long,
 * verbatim - every entry type BIOS reports, not just usable RAM: the
 * kernel itself needs the full map, reserved/ACPI regions included,
 * to avoid treating them as usable) and returns the number of
 * entries actually filled, or 0 if even the first call fails (a BIOS
 * old enough to lack E820 support entirely - not something this code
 * has a fallback for; every target this project cares about, real
 * hardware and Proxmox/SeaBIOS alike, supports it).
 */
uint8_t e820_get_map(struct boot_e820_entry *entries, uint8_t max);

#endif
