package biosboot

import (
	"strings"
	"testing"
)

func TestCheckSectorSize(t *testing.T) {
	disk := fakeDisk(t, 4096, 0)
	if err := CheckSectorSize(disk); err != nil {
		t.Fatalf("a regular file counts as 512: %v", err)
	}
	orig := logicalSectorSize
	t.Cleanup(func() { logicalSectorSize = orig })
	logicalSectorSize = func(string) (int, error) { return 4096, nil }
	if err := CheckSectorSize(disk); err == nil || !strings.Contains(err.Error(), "4096-byte logical sectors") {
		t.Fatalf("a 4Kn disk: want a refusal, got %v", err)
	}
	logicalSectorSize = func(string) (int, error) { return 512, nil }
	if err := CheckSectorSize(disk); err != nil {
		t.Fatalf("512: %v", err)
	}
}
