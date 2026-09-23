package espconfig

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unidoc/alpine-zfsboot/internal/layout"
)

func TestWriteFile_WritesAndVerifies(t *testing.T) {
	mnt := t.TempDir()
	content := []byte("hello, alpine-zfsboot\n")

	if err := WriteFile(mnt, "EFI/ALPINE/CMDLINE", content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := VerifyFile(mnt, "EFI/ALPINE/CMDLINE", content); err != nil {
		t.Errorf("VerifyFile after a correct write: %v", err)
	}

	info, err := os.Stat(filepath.Join(mnt, "EFI/ALPINE/CMDLINE"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", info.Mode().Perm())
	}
}

// TestWriteFile_ChmodFailureDoesNotAbortTheWrite is the regression
// test for a real, plausible (though not provable in this sandbox - no
// loop-device/root access to mount a real vfat filesystem) failure
// mode a full source audit flagged: Linux's fat_setattr() is
// documented to reject (EPERM) many chmod calls on a real vfat mount,
// and bootenv.MountESP never passes an explicit fmask=/dmask=/umask=
// option - if that theory is right, treating chmod as fatal would make
// EVERY real espconfig.WriteFile call fail on a real ESP, the single
// most severe possible form of this bug. Whatever the real kernel
// behavior turns out to be, this proves the code is now correct
// EITHER way: WriteFile must still succeed (content written, verified,
// durable) even when chmod itself fails.
func TestWriteFile_ChmodFailureDoesNotAbortTheWrite(t *testing.T) {
	orig := chmodFile
	chmodFile = func(*os.File, os.FileMode) error {
		return os.ErrPermission
	}
	defer func() { chmodFile = orig }()

	mnt := t.TempDir()
	content := []byte("hello, alpine-zfsboot\n")
	if err := WriteFile(mnt, "EFI/ALPINE/CMDLINE", content, 0o644); err != nil {
		t.Fatalf("WriteFile with a forced chmod failure: want success (best-effort chmod), got: %v", err)
	}
	if err := VerifyFile(mnt, "EFI/ALPINE/CMDLINE", content); err != nil {
		t.Errorf("VerifyFile after a write with a forced chmod failure: %v", err)
	}
}

func TestWriteFile_OverwritesExisting(t *testing.T) {
	mnt := t.TempDir()
	if err := WriteFile(mnt, "EFI/ALPINE/config", []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(mnt, "EFI/ALPINE/config", []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(mnt, "EFI/ALPINE/config", []byte("new content")); err != nil {
		t.Errorf("VerifyFile after overwrite: %v", err)
	}
}

func TestWriteFile_NoStrayTempFilesLeftBehind(t *testing.T) {
	mnt := t.TempDir()
	if err := WriteFile(mnt, "EFI/ALPINE/config", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(mnt, "EFI/ALPINE"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".alpine-zfsboot-") {
			t.Errorf("stray temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "config" {
		t.Errorf("directory contents = %v, want exactly one entry named \"config\"", entries)
	}
}

func TestVerifyFile_DetectsMismatch(t *testing.T) {
	mnt := t.TempDir()
	if err := WriteFile(mnt, "EFI/ALPINE/CMDLINE", []byte("actual"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(mnt, "EFI/ALPINE/CMDLINE", []byte("expected")); err == nil {
		t.Error("VerifyFile against mismatched content: want an error, got nil")
	}
}

func TestVerifyFile_MissingFile(t *testing.T) {
	mnt := t.TempDir()
	if err := VerifyFile(mnt, "EFI/ALPINE/NOPE", []byte("x")); err == nil {
		t.Error("VerifyFile on a missing file: want an error, got nil")
	}
}

func TestWritePayloadAndVerify(t *testing.T) {
	mnt := t.TempDir()
	kernel, initrd, cmdline := []byte("kernel-bytes"), []byte("initrd-bytes"), []byte("root=ZFS=zroot/ROOT/alpine ro\n")

	if err := WritePayload(mnt, kernel, initrd, cmdline); err != nil {
		t.Fatalf("WritePayload: %v", err)
	}
	if err := VerifyPayload(mnt, kernel, initrd, cmdline); err != nil {
		t.Errorf("VerifyPayload after a correct write: %v", err)
	}

	for _, rel := range []string{layout.KernelFile, layout.InitrdFile, layout.CmdlineFile} {
		if _, err := os.Stat(filepath.Join(mnt, rel)); err != nil {
			t.Errorf("expected %s to exist: %v", rel, err)
		}
	}

	if err := VerifyPayload(mnt, []byte("wrong"), initrd, cmdline); err == nil {
		t.Error("VerifyPayload with a wrong kernel: want an error, got nil")
	}
}

func TestConfigRender(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "all empty produces an empty file",
			cfg:  Config{},
			want: "",
		},
		{
			name: "only the fields that are set get a line, in a fixed order",
			cfg: Config{
				Net:      "static",
				SSHPort:  "2222",
				IPv4:     "static",
				SSHAllow: "0.0.0.0/0",
			},
			want: "alpine-zfsboot.net=static\n" +
				"alpine-zfsboot.ipv4=static\n" +
				"alpine-zfsboot.ssh.port=2222\n" +
				"alpine-zfsboot.ssh.allow=0.0.0.0/0\n",
		},
		{
			name: "every field set",
			cfg: Config{
				Net: "static", IPv4: "static", IPv4Address: "10.0.0.5/24", IPv4Gateway: "10.0.0.1",
				IPv6: "static", IPv6Address: "fd00::5/64", IPv6Gateway: "fe80::1",
				SSHListen: "0.0.0.0", SSHPort: "22", SSHAllow: "10.0.0.0/8",
			},
			want: "alpine-zfsboot.net=static\n" +
				"alpine-zfsboot.ipv4=static\n" +
				"alpine-zfsboot.ipv4.address=10.0.0.5/24\n" +
				"alpine-zfsboot.ipv4.gateway=10.0.0.1\n" +
				"alpine-zfsboot.ipv6=static\n" +
				"alpine-zfsboot.ipv6.address=fd00::5/64\n" +
				"alpine-zfsboot.ipv6.gateway=fe80::1\n" +
				"alpine-zfsboot.ssh.listen=0.0.0.0\n" +
				"alpine-zfsboot.ssh.port=22\n" +
				"alpine-zfsboot.ssh.allow=10.0.0.0/8\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(tc.cfg.render())
			if got != tc.want {
				t.Errorf("render() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteConfigAndVerify(t *testing.T) {
	mnt := t.TempDir()
	cfg := Config{Net: "static", IPv4: "dhcp", SSHPort: "22"}

	if err := WriteConfig(mnt, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if err := VerifyConfig(mnt, cfg); err != nil {
		t.Errorf("VerifyConfig after a correct write: %v", err)
	}
	if err := VerifyConfig(mnt, Config{Net: "off"}); err == nil {
		t.Error("VerifyConfig against a different config: want an error, got nil")
	}
}

func TestWriteAuthorizedKeys(t *testing.T) {
	mnt := t.TempDir()
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... user@host"

	if err := WriteAuthorizedKeys(mnt, key); err != nil {
		t.Fatalf("WriteAuthorizedKeys: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(mnt, layout.AuthorizedKeysFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != key+"\n" {
		t.Errorf("authorized_keys content = %q, want %q", got, key+"\n")
	}
	info, err := os.Stat(filepath.Join(mnt, layout.AuthorizedKeysFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestGenerateHostKey is now a pure Go correctness check, not an
// exec.LookPath-gated one - GenerateHostKey no longer shells out to
// `dropbearkey` at all (see its own doc comment for why and the
// hardening ledger for the real round-trip verification against a
// real dropbearkey binary this rewrite was proven against). File size
// is exact (83 bytes - see encodeDropbearEd25519PrivateKey's own
// provenance comment for the field-by-field accounting), not just
// "non-empty".
func TestGenerateHostKey(t *testing.T) {
	mnt := t.TempDir()
	if err := GenerateHostKey(mnt); err != nil {
		t.Fatalf("GenerateHostKey: %v", err)
	}
	info, err := os.Stat(filepath.Join(mnt, layout.SSHHostEd25519KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	if info.Size() != 83 {
		t.Errorf("generated host key file is %d bytes, want exactly 83", info.Size())
	}
}

// TestGenerateHostKey_TwoCallsProduceDifferentKeys is a real, cheap
// sanity check that key generation is actually using fresh randomness
// each call, not silently reusing the same key material (a real,
// simple way this kind of rewrite could go wrong: e.g. a package-level
// rand.Reader accidentally seeded once, or a copy-paste that reuses a
// fixed seed).
func TestGenerateHostKey_TwoCallsProduceDifferentKeys(t *testing.T) {
	mnt1, mnt2 := t.TempDir(), t.TempDir()
	if err := GenerateHostKey(mnt1); err != nil {
		t.Fatal(err)
	}
	if err := GenerateHostKey(mnt2); err != nil {
		t.Fatal(err)
	}
	k1, err := os.ReadFile(filepath.Join(mnt1, layout.SSHHostEd25519KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := os.ReadFile(filepath.Join(mnt2, layout.SSHHostEd25519KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k2) {
		t.Fatal("two separate GenerateHostKey calls produced byte-identical keys")
	}
}

// TestGenerateHostKey_RealDropbearkeyCanParseIt is the direct proof
// this project's own independently-implemented file format (see
// encodeDropbearEd25519PrivateKey's own provenance comment) is
// genuinely readable by real dropbear code, not just internally
// self-consistent: `dropbearkey -y -f <file>` parses the file with
// dropbear's OWN real signkey/buffer code and prints the public key it
// derives - the strongest verification available short of running a
// full dropbear SSH server against it. `dropbearkey` is a real,
// external, oracle/fixture tool here (this project's own CI has it
// available for exactly this purpose - see this project's own
// self-containment audit: external tools are allowed as TEST oracles,
// never as production runtime dependencies), never invoked by
// GenerateHostKey itself.
func TestGenerateHostKey_RealDropbearkeyCanParseIt(t *testing.T) {
	if _, err := exec.LookPath("dropbearkey"); err != nil {
		t.Skip("dropbearkey not available in this environment (test-oracle only, not a production dependency)")
	}
	mnt := t.TempDir()
	if err := GenerateHostKey(mnt); err != nil {
		t.Fatalf("GenerateHostKey: %v", err)
	}
	keyPath := filepath.Join(mnt, layout.SSHHostEd25519KeyFile)

	out, err := exec.Command("dropbearkey", "-y", "-f", keyPath).CombinedOutput()
	if err != nil {
		t.Fatalf("real dropbearkey could not parse our generated key: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "ssh-ed25519 ") {
		t.Errorf("dropbearkey -y output didn't contain a recognizable public key line: %s", out)
	}
}

// TestGenerateHostKey_MatchesRealDropbearkeyGeneratedFileShape is the
// same oracle proof in the OTHER direction: a key generated by REAL
// dropbearkey is exactly 83 bytes and decodes, under this project's
// own documented field layout, to a seed whose Go-derived public key
// (crypto/ed25519.NewKeyFromSeed) matches the public key bytes
// embedded in that same real file - proving this project's documented
// understanding of the format (not just its own writer's output)
// matches what real dropbear code itself actually produces.
func TestGenerateHostKey_MatchesRealDropbearkeyGeneratedFileShape(t *testing.T) {
	if _, err := exec.LookPath("dropbearkey"); err != nil {
		t.Skip("dropbearkey not available in this environment (test-oracle only, not a production dependency)")
	}
	keyPath := filepath.Join(t.TempDir(), "real-dropbear-key")
	if out, err := exec.Command("dropbearkey", "-t", "ed25519", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("real dropbearkey could not generate a fixture key: %v (%s)", err, out)
	}

	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 83 {
		t.Fatalf("real dropbearkey's own output is %d bytes, want exactly 83 - this project's documented format is wrong", len(data))
	}
	typeLen := binary.BigEndian.Uint32(data[0:4])
	if string(data[4:4+typeLen]) != dropbearEd25519KeyTypeName {
		t.Fatalf("key type field = %q, want %q", data[4:4+typeLen], dropbearEd25519KeyTypeName)
	}
	off := int(4 + typeLen)
	combLen := binary.BigEndian.Uint32(data[off : off+4])
	if combLen != uint32(ed25519.PrivateKeySize) {
		t.Fatalf("combined seed+pubkey length field = %d, want %d", combLen, ed25519.PrivateKeySize)
	}
	off += 4
	seed := data[off : off+32]
	embeddedPub := data[off+32 : off+64]

	derivedPub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !bytes.Equal(derivedPub, embeddedPub) {
		t.Error("public key derived (via Go's own crypto/ed25519) from the real dropbearkey file's own embedded seed does not match that same file's own embedded public key bytes")
	}
}

// TestEncodeDropbearEd25519PrivateKey_WrongSizePanics is the
// malformed/boundary-input test for the one real way this function
// can be misused - a caller passing something that isn't a real,
// complete ed25519.PrivateKey. A panic (not a silently-wrong short
// write) is the correct behavior for what is, in practice, a
// programmer error at every real call site (the only caller is
// GenerateHostKey, immediately after a successful
// ed25519.GenerateKey) - never reachable with attacker-controlled
// input.
func TestEncodeDropbearEd25519PrivateKey_WrongSizePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("encodeDropbearEd25519PrivateKey with a wrong-size key: want a panic, got none")
		}
	}()
	encodeDropbearEd25519PrivateKey(make([]byte, 10))
}

func TestEncodeDropbearEd25519PrivateKey_Deterministic(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := encodeDropbearEd25519PrivateKey(priv)
	b := encodeDropbearEd25519PrivateKey(priv)
	if !bytes.Equal(a, b) {
		t.Error("encodeDropbearEd25519PrivateKey given the SAME key material twice produced different bytes - the encoder itself must be pure, all randomness belongs in key generation only")
	}
}
