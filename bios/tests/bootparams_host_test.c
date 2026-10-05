/* Host test for bootparams_build()'s screen_info (VGA text console) fields.
 * This loader enters the kernel at its 32-bit entry, so the kernel's real-mode
 * setup never fills screen_info; without these bytes console=tty0 shows nothing
 * on the VGA screen after "starting kernel" (BIOS boots only). */
#include <stdio.h>
#include <string.h>
#include "bootparams.h"

static int failures;
#define CHECK(c) do { if (!(c)) { printf("FAIL - %s (line %d)\n", #c, __LINE__); failures++; } } while (0)

static uint16_t u16(const uint8_t *p, int off) { return (uint16_t)(p[off] | (p[off + 1] << 8)); }

int main(void)
{
	static uint8_t bp[BOOT_PARAMS_SIZE];
	struct setup_header hdr;
	struct boot_e820_entry e = { .addr = 0, .size = 0x9f000, .type = E820_TYPE_RAM };
	struct boot_video v = { .mode = 3, .cols = 80, .page = 0, .cursor_row = 17, .cursor_col = 5 };

	memset(&hdr, 0, sizeof(hdr));

	memset(bp, 0, sizeof(bp));
	CHECK(bootparams_build(bp, &hdr, 0x4000000, 1234, 0x20000, &e, 1, &v) == 0);
	CHECK(bp[BOOT_PARAMS_SCREEN_ORIG_X] == 5);
	CHECK(bp[BOOT_PARAMS_SCREEN_ORIG_Y] == 17);
	CHECK(bp[BOOT_PARAMS_SCREEN_VIDEO_MODE] == 3);
	CHECK(bp[BOOT_PARAMS_SCREEN_VIDEO_COLS] == 80);
	CHECK(bp[BOOT_PARAMS_SCREEN_VIDEO_LINES] == 25);
	CHECK(bp[BOOT_PARAMS_SCREEN_VIDEO_ISVGA] == 1);
	CHECK(u16(bp, BOOT_PARAMS_SCREEN_VIDEO_EGA_BX) == 3);
	CHECK(u16(bp, BOOT_PARAMS_SCREEN_VIDEO_POINTS) == 16);
	/* the existing fields are untouched */
	CHECK(bp[BOOT_PARAMS_E820_ENTRIES_OFFSET] == 1);

	/* Offsets match include/uapi/linux/screen_info.h (struct screen_info). */
	CHECK(BOOT_PARAMS_SCREEN_VIDEO_PAGE == 4 && BOOT_PARAMS_SCREEN_VIDEO_MODE == 6 &&
	      BOOT_PARAMS_SCREEN_VIDEO_COLS == 7 && BOOT_PARAMS_SCREEN_VIDEO_EGA_BX == 0x0a &&
	      BOOT_PARAMS_SCREEN_VIDEO_LINES == 0x0e && BOOT_PARAMS_SCREEN_VIDEO_ISVGA == 0x0f &&
	      BOOT_PARAMS_SCREEN_VIDEO_POINTS == 0x10);

	/* Bit 7 of the BIOS mode byte ("memory not cleared") is masked off. */
	v.mode = 0x83;
	memset(bp, 0, sizeof(bp));
	bootparams_build(bp, &hdr, 0x4000000, 1234, 0x20000, &e, 1, &v);
	CHECK(bp[BOOT_PARAMS_SCREEN_VIDEO_ISVGA] == 1);
	CHECK(bp[BOOT_PARAMS_SCREEN_VIDEO_MODE] == 3);
	CHECK(bp[BOOT_PARAMS_SCREEN_VIDEO_LINES] == 25);
	v.mode = 3;

	/* No video info, or a non-text/graphics mode: left zeroed, as before. */
	memset(bp, 0, sizeof(bp));
	bootparams_build(bp, &hdr, 0x4000000, 1234, 0x20000, &e, 1, NULL);
	CHECK(bp[BOOT_PARAMS_SCREEN_VIDEO_ISVGA] == 0);
	v.mode = 0x12;
	memset(bp, 0, sizeof(bp));
	bootparams_build(bp, &hdr, 0x4000000, 1234, 0x20000, &e, 1, &v);
	CHECK(bp[BOOT_PARAMS_SCREEN_VIDEO_ISVGA] == 0);

	if (!failures)
		printf("ok   - bootparams screen_info\n");
	return failures ? 1 : 0;
}
