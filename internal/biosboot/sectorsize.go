package biosboot

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// logicalSectorSize returns a block device's logical sector size
// (BLKSSZGET); a regular file (tests, disk images) counts as 512.
var logicalSectorSize = func(disk string) (int, error) {
	f, err := os.Open(disk)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if st.Mode()&os.ModeDevice == 0 {
		return layout.SectorSize, nil
	}
	return unix.IoctlGetInt(int(f.Fd()), unix.BLKSSZGET)
}

// CheckSectorSize refuses a disk whose logical sectors are not 512 bytes
// (a "4Kn" disk): the BIOS boot layout is in 512-byte units - stage1 reads
// stage2 from LBA 34 of 512 bytes, the BIOS loader's GPT and FAT code too -
// and on such a disk the BIOS's own LBAs are 4096 bytes, so stage1 would
// read the wrong place and the machine would not boot. Install/update
// call it before writing anything. (512e disks - 4096-byte physical,
// 512-byte logical sectors - are fine.)
func CheckSectorSize(disk string) error {
	n, err := logicalSectorSize(disk)
	if err != nil {
		return fmt.Errorf("reading the logical sector size of %s: %w", disk, err)
	}
	if n != layout.SectorSize {
		return fmt.Errorf("%s has %d-byte logical sectors; the BIOS boot loader supports only 512-byte sectors (stage1/stage2/GPT/FAT layout) - use UEFI on this disk", disk, n)
	}
	return nil
}
