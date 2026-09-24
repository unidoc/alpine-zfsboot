// Package espconfig writes and verifies alpine-zfsboot's files on the
// canonical FAT/ESP partition: stage2's own boot payload
// (layout.KernelFile/InitrdFile/CmdlineFile) and /init's own
// machine-config files (layout.ConfigFile/AuthorizedKeysFile/
// SSHHostEd25519KeyFile). Every write goes through WriteFile, the one
// transactional primitive this package exists to provide: temp file
// in the SAME directory -> write -> fsync the file -> rename over the
// target -> fsync the directory. This is the fix for a real,
// confirmed gap: init/menu.py's own _write_fat_console_pref() opens
// EFI/ALPINE/config directly in "w" mode (plain truncate-in-place,
// fsyncs only the file, never the directory, no rename) - correct for
// a config file that ALSO carries rescue-SSH/network settings is
// exactly what this package provides instead. menu.py itself stays
// Python (see this project's own plan for why - different runtime,
// same semantics, not shared code) but every Go writer (install,
// update) goes through this.
package espconfig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

// chmodFile is (*os.File).Chmod, swappable in tests to force a failure
// deterministically - a real chmod on a plain regular file (ext4/tmpfs,
// what every test in this package runs against) essentially never
// fails, so nothing in a normal test environment can exercise
// WriteFile's own best-effort handling of a REAL chmod failure without
// this seam (same technique internal/uefiboot's own swappable HardLink
// uses for the identical reason).
var chmodFile = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }

// WriteFile durably replaces mountpoint/relPath with content: a temp
// file is created in the SAME directory as the target (guaranteeing
// the final rename is on one filesystem, hence atomic), written,
// fsynced, renamed over the target, and then the containing
// directory's own fd is fsynced too - the step a plain
// write+fsync-the-file omits, and the one that actually makes the
// new directory entry durable across a crash, not just the file's own
// content.
func WriteFile(mountpoint, relPath string, content []byte, mode os.FileMode) error {
	target := filepath.Join(mountpoint, relPath)
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".alpine-zfsboot-write-*")
	if err != nil {
		return fmt.Errorf("creating a temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails before the rename -
	// once os.Rename succeeds, tmpPath no longer exists under this
	// name, so a stray Remove after that point is a silent no-op.
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	// Best-effort, not fatal to the write - a full source audit flagged
	// this as a real, plausible (though unverified in that sandbox, and
	// still unverified here: no loop-device/root access to actually
	// mount a real vfat filesystem and test) failure mode on the real
	// target filesystem: Linux's fat_setattr() rejects (EPERM) any
	// chmod whose requested permission bits, once masked by the mount's
	// own fmask/dmask, differ from the file's current (also
	// mask-derived) mode - and bootenv.MountESP never passes an
	// explicit fmask=/dmask=/umask= option, so the effective mask (and
	// therefore whether ANY chmod() on this filesystem can ever
	// succeed) depends entirely on whatever default the mount process's
	// own umask happens to produce. If that theory is right, treating
	// this as fatal would make EVERY real espconfig.WriteFile call fail
	// on a real ESP - the single most severe possible form of this bug.
	// Moot either way for what this project actually needs from `mode`:
	// FAT has no on-disk representation of Unix permission bits AT ALL
	// (every file under one mount effectively shares the SAME
	// mask-derived mode, chmod or not) - the real security boundary for
	// e.g. layout.AuthorizedKeysFile's 0600 is the ESP being mounted
	// privately at a temp mountpoint for the duration of this call, not
	// this file's own permission bits, which vfat cannot meaningfully
	// enforce regardless of what this call requests.
	if err := chmodFile(tmp, mode); err != nil {
		fmt.Fprintf(os.Stderr, "alpine-zfsboot: WARNING: setting permissions on %s failed (%v) - proceeding anyway, since FAT cannot represent per-file Unix permissions on disk regardless\n", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, target, err)
	}

	dirFile, err := os.Open(dir)
	if err != nil {
		// The rename itself already succeeded - the file is in place
		// under its real name. A directory-fsync failure here means
		// the durability guarantee is weaker than intended, but it is
		// not a reason to report the write itself as failed.
		return fmt.Errorf("opening %s to fsync the directory entry (the file itself was written and renamed successfully): %w", dir, err)
	}
	defer dirFile.Close()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("fsyncing %s (the file itself was written and renamed successfully): %w", dir, err)
	}
	return nil
}

// VerifyFile re-reads mountpoint/relPath and compares it byte-for-byte
// against want.
func VerifyFile(mountpoint, relPath string, want []byte) error {
	got, err := os.ReadFile(filepath.Join(mountpoint, relPath))
	if err != nil {
		return fmt.Errorf("reading %s: %w", relPath, err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s does not match its expected content", relPath)
	}
	return nil
}

// WritePayload writes stage2's own boot payload - EFI/ALPINE/
// {KERNEL,INITRD,CMDLINE} - via WriteFile, matching
// alpine-install-zfs.sh's own install_alpine_zfsboot_bios() target
// paths exactly (see internal/layout's own KernelFile/InitrdFile/
// CmdlineFile).
func WritePayload(mountpoint string, kernel, initrd, cmdline []byte) error {
	for _, f := range []struct {
		rel     string
		content []byte
	}{
		{layout.KernelFile, kernel},
		{layout.InitrdFile, initrd},
		{layout.CmdlineFile, cmdline},
	} {
		if err := WriteFile(mountpoint, f.rel, f.content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", f.rel, err)
		}
	}
	return nil
}

// VerifyPayload closes a real, confirmed gap in
// alpine-install-zfs.sh's own verify_installation(): closes the
// "config/authorized_keys/host-key are never verified" gap for the
// KERNEL/INITRD/CMDLINE part (see VerifyConfig below for the other
// half).
func VerifyPayload(mountpoint string, kernel, initrd, cmdline []byte) error {
	for _, f := range []struct {
		rel     string
		content []byte
	}{
		{layout.KernelFile, kernel},
		{layout.InitrdFile, initrd},
		{layout.CmdlineFile, cmdline},
	} {
		if err := VerifyFile(mountpoint, f.rel, f.content); err != nil {
			return err
		}
	}
	return nil
}

// Config mirrors alpine-install-zfs.sh's own
// write_alpine_zfsboot_esp_config() env-var surface field-for-field -
// same names (minus the ALPINE_ZFSBOOT_ prefix), same optional/empty
// semantics: a zero-value field emits no line at all, not an
// empty-valued one.
type Config struct {
	Net         string
	IPv4        string
	IPv4Address string
	IPv4Gateway string
	IPv6        string
	IPv6Address string
	IPv6Gateway string
	SSHListen   string
	SSHPort     string
	SSHAllow    string
}

// render produces EFI/ALPINE/config's own exact key=value line
// format - split out from WriteConfig so the format itself has a
// plain-string test independent of any real file I/O.
func (c Config) render() []byte {
	var buf bytes.Buffer
	for _, kv := range []struct{ key, value string }{
		{"alpine-zfsboot.net", c.Net},
		{"alpine-zfsboot.ipv4", c.IPv4},
		{"alpine-zfsboot.ipv4.address", c.IPv4Address},
		{"alpine-zfsboot.ipv4.gateway", c.IPv4Gateway},
		{"alpine-zfsboot.ipv6", c.IPv6},
		{"alpine-zfsboot.ipv6.address", c.IPv6Address},
		{"alpine-zfsboot.ipv6.gateway", c.IPv6Gateway},
		{"alpine-zfsboot.ssh.listen", c.SSHListen},
		{"alpine-zfsboot.ssh.port", c.SSHPort},
		{"alpine-zfsboot.ssh.allow", c.SSHAllow},
	} {
		if kv.value == "" {
			continue
		}
		fmt.Fprintf(&buf, "%s=%s\n", kv.key, kv.value)
	}
	return buf.Bytes()
}

// WriteConfig writes EFI/ALPINE/config via WriteFile - always a fresh
// file (matching write_alpine_zfsboot_esp_config()'s own truncate
// semantics), not a read-modify-write (that pattern belongs only to
// menu.py's own single alpine-zfsboot.console= line, which this
// package does not touch - see this project's own plan for that
// boundary).
func WriteConfig(mountpoint string, cfg Config) error {
	return WriteFile(mountpoint, layout.ConfigFile, cfg.render(), 0o644)
}

// VerifyConfig re-reads EFI/ALPINE/config and confirms it renders
// exactly as cfg would.
func VerifyConfig(mountpoint string, cfg Config) error {
	return VerifyFile(mountpoint, layout.ConfigFile, cfg.render())
}

// WriteAuthorizedKeys writes EFI/ALPINE/authorized_keys - one bare
// SSH public key line, mode 0600 (matches
// write_alpine_zfsboot_esp_config()'s own chmod).
func WriteAuthorizedKeys(mountpoint, sshKey string) error {
	return WriteFile(mountpoint, layout.AuthorizedKeysFile, []byte(sshKey+"\n"), 0o600)
}

// --- Dropbear ed25519 private-key file format ---------------------------
//
// PROVENANCE: determined by reading dropbear's own real, current
// upstream source directly (github.com/mkj/dropbear, MIT-licensed) -
// not guessed, not taken from any third-party description of the
// format:
//   - src/gensignkey.c's signkey_generate(): the on-disk file is
//     exactly the buf_put_priv_key() buffer, written VERBATIM via
//     buf_writefile() - no magic, no header, no version field, no
//     wrapper of any kind around it.
//   - src/ed25519.c's buf_put_ed25519_priv_key(): the buffer's own
//     contents for an ed25519 key specifically (key type string, a
//     combined-length int, then the raw seed and public key bytes).
//   - src/buffer.c's buf_putint()/buf_putstring(): SSH-wire-format
//     integers and strings both use a 4-byte BIG-ENDIAN length/value
//     (STORE32H) - the standard SSH wire encoding per RFC 4251's own
//     "uint32"/"string" data types, not a dropbear-specific choice.
//   - src/curve25519.c's dropbear_ed25519_make_key(): confirms the
//     32-byte "priv" field is a plain RFC 8032 Ed25519 SEED
//     (genrandom() into it, then SHA-512+clamp+scalarbase to derive
//     the actual signing scalar and public key) - the exact same seed
//     format Go's own crypto/ed25519.NewKeyFromSeed() takes, and
//     exactly what an ed25519.PrivateKey's own first 32 bytes already
//     are (Go's PrivateKey type is defined as seed||publicKey, the
//     same two-part layout).
//
// This is an INDEPENDENT Go implementation of that file FORMAT,
// written from those observed facts - no dropbear source is copied,
// adapted, ported, or referenced by this file at build or run time.
//
// File layout, 83 bytes total, no padding or separators between
// fields:
//
//	uint32 BE   11    ("ssh-ed25519" string length)
//	[11]byte    "ssh-ed25519"
//	uint32 BE   64    (combined seed+pubkey length, CURVE25519_LEN*2)
//	[32]byte    seed        - ed25519.PrivateKey[:32]
//	[32]byte    public key  - ed25519.PrivateKey[32:]
const dropbearEd25519KeyTypeName = "ssh-ed25519"

// encodeDropbearEd25519PrivateKey serializes priv (a real Go
// ed25519.PrivateKey, Seed()+PublicKey already concatenated in
// exactly the layout this format needs) into a dropbear-format
// private-key file's own bytes - see this section's own provenance
// comment above for exactly where every field and its encoding came
// from.
func encodeDropbearEd25519PrivateKey(priv ed25519.PrivateKey) []byte {
	if len(priv) != ed25519.PrivateKeySize {
		panic(fmt.Sprintf("encodeDropbearEd25519PrivateKey: got a %d-byte key, want exactly ed25519.PrivateKeySize (%d)", len(priv), ed25519.PrivateKeySize))
	}
	buf := make([]byte, 0, 4+len(dropbearEd25519KeyTypeName)+4+ed25519.PrivateKeySize)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(dropbearEd25519KeyTypeName)))
	buf = append(buf, dropbearEd25519KeyTypeName...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(ed25519.PrivateKeySize))
	buf = append(buf, priv...) // priv is already seed(32)||pubkey(32), same layout the format wants
	return buf
}

// GenerateHostKey creates a fresh dropbear-format ed25519 host key at
// EFI/ALPINE/ssh_host_ed25519_key - a native Go implementation
// (crypto/ed25519 key generation + encodeDropbearEd25519PrivateKey's
// own from-scratch, independently-implemented serialization, see that
// function's own provenance comment) rather than shelling out to the
// `dropbearkey` binary: this is a boot/recovery introspection and
// install tool, which must not depend on a separately-installed
// userspace package merely to generate its own rescue-SSH host
// identity (the same reasoning that already took `xz`/`mount`/
// `blkid` out of this project's production Go - see
// internal/initrdinfo's own header comment). Goes through WriteFile,
// the same atomic (temp file in the same directory -> fsync -> rename
// -> fsync the directory) primitive every other writer in this
// package uses - no separate hand-rolled temp-file dance needed now
// that this function controls the key bytes directly, unlike the old
// dropbearkey-must-write-its-own-file constraint.
func GenerateHostKey(mountpoint string) error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generating an ed25519 key: %w", err)
	}
	return WriteFile(mountpoint, layout.SSHHostEd25519KeyFile, encodeDropbearEd25519PrivateKey(priv), 0o600)
}
