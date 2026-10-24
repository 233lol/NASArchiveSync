package sftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestCipherList(t *testing.T) {
	def, err := cipherList("")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if len(def) != len(SupportedCiphers) || def[0] != DefaultCipher {
		t.Errorf("default list = %v, want %v", def, SupportedCiphers)
	}

	auto, err := cipherList("auto")
	if err != nil || auto[0] != DefaultCipher {
		t.Errorf("auto = %v, %v", auto, err)
	}

	one, err := cipherList("AES256-GCM@OPENSSH.COM")
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if len(one) != 1 || one[0] != "aes256-gcm@openssh.com" {
		t.Errorf("explicit = %v", one)
	}

	if _, err := cipherList("rc4"); err == nil {
		t.Error("expected an error for an unsupported cipher")
	}
}

func TestCleanRoot(t *testing.T) {
	tests := map[string]string{
		"":                 "/",
		"/":                "/",
		"foo":              "/foo",
		"/foo/":            "/foo",
		`\foo\bar`:         "/foo/bar",
		"/foo//bar/../baz": "/foo/baz",
	}
	for in, want := range tests {
		if got := cleanRoot(in); got != want {
			t.Errorf("cleanRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeKnownHostsSkipsBrokenLines(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	key := signer.PublicKey()
	good := knownhosts.Line([]string{"example.test"}, key)

	dir := t.TempDir()
	src := filepath.Join(dir, "known_hosts")
	content := good + "\n" + good + "trailing-garbage\n# comment\n"
	if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	clean, err := sanitizeKnownHosts(src)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	defer os.Remove(clean)

	cb, err := knownhosts.New(clean)
	if err != nil {
		t.Fatalf("knownhosts.New(clean): %v", err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	if err := cb("example.test:22", addr, key); err != nil {
		t.Errorf("clean file rejected the key: %v", err)
	}

	// The original file must still fail, proving the sanitizer did the work.
	if _, err := knownhosts.New(src); err == nil {
		t.Error("expected the malformed original file to be rejected")
	}
}

func TestSanitizeKnownHostsNoEntries(t *testing.T) {
	src := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(src, []byte("not a valid line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sanitizeKnownHosts(src); err == nil {
		t.Error("expected an error when no valid entries remain")
	}
}

func TestRetryableHostKeyError(t *testing.T) {
	if retryableHostKeyError(os.ErrNotExist) {
		t.Error("unrelated errors must not trigger a retry")
	}
	if !retryableHostKeyError(&knownhosts.KeyError{Want: []knownhosts.KnownKey{{Key: nil}}}) {
		t.Error("a key mismatch with candidate keys should retry")
	}
	if retryableHostKeyError(&knownhosts.KeyError{}) {
		t.Error("an unknown host should not retry every algorithm")
	}
	if !retryableHostKeyError(errString("ssh: handshake failed: knownhosts: key mismatch")) {
		t.Error("a wrapped knownhosts error should retry")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestLoadPrivateKeyPassphrase(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	writePEM := func(name string, block *pem.Block) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	enc, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	encPath := writePEM("encrypted", enc)
	if _, err := loadPrivateKey(encPath, "secret"); err != nil {
		t.Errorf("correct passphrase: %v", err)
	}
	if _, err := loadPrivateKey(encPath, "wrong"); err == nil {
		t.Error("expected an error for a wrong passphrase")
	}
	if _, err := loadPrivateKey(encPath, ""); err == nil {
		t.Error("expected an error for a missing passphrase")
	}

	plain, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	plainPath := writePEM("plain", plain)
	// A configured passphrase must not break an unencrypted key.
	if _, err := loadPrivateKey(plainPath, "secret"); err != nil {
		t.Errorf("unencrypted key with a passphrase set: %v", err)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := expandHome("~"); got != home {
		t.Errorf("expandHome(~) = %q, want %q", got, home)
	}
	if got, want := expandHome("~/.ssh/id_ed25519"), filepath.Join(home, ".ssh", "id_ed25519"); got != want {
		t.Errorf("expandHome(~/...) = %q, want %q", got, want)
	}
	if got := expandHome(`C:\keys\id`); got != `C:\keys\id` {
		t.Errorf("plain path changed: %q", got)
	}
}

func TestSupportedCiphersAreDistinct(t *testing.T) {
	seen := make(map[string]bool, len(SupportedCiphers))
	for _, c := range SupportedCiphers {
		if c != strings.ToLower(c) {
			t.Errorf("cipher %q must be lower case", c)
		}
		if seen[c] {
			t.Errorf("duplicate cipher %q", c)
		}
		seen[c] = true
	}
}
