package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aldrinleal/agent-manager-public/config"
)

func writeKey(t *testing.T) string {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "id_test")

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// Fingerprints must be known before anything has been dialed — otherwise
// `agentmgr hosts` and GET /hosts report no key at all until the first
// connection happens to occur.
func TestFingerprintsAvailableBeforeAnyDial(t *testing.T) {
	kr, err := NewKeyRing(&config.Config{SSHKeyFile: writeKey(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}

	fps := kr.Fingerprints()

	if len(fps) != 1 {
		t.Fatalf("got %d fingerprints before dialing, want 1", len(fps))
	}
	if !strings.HasPrefix(fps[0], "SHA256:") {
		t.Errorf("fingerprint = %q", fps[0])
	}
}

// The fingerprint is all that may leave; the key itself never does.
func TestFingerprintsNeverLeakKeyMaterial(t *testing.T) {
	path := writeKey(t)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	kr, err := NewKeyRing(&config.Config{SSHKeyFile: path}, nil)
	if err != nil {
		t.Fatal(err)
	}

	body := strings.Join(kr.Fingerprints(), "\n")

	for _, line := range strings.Split(string(raw), "\n") {
		if len(line) > 20 && strings.Contains(body, line) {
			t.Fatal("private key material appeared in Fingerprints() output")
		}
	}
}

func TestInlineKeyWins(t *testing.T) {
	inline, err := os.ReadFile(writeKey(t))
	if err != nil {
		t.Fatal(err)
	}

	kr, err := NewKeyRing(&config.Config{SSHKey: string(inline), SSHKeyFile: writeKey(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}

	methods, err := kr.AuthMethods("")
	if err != nil {
		t.Fatal(err)
	}

	if len(methods) != 1 {
		t.Errorf("got %d auth methods, want 1", len(methods))
	}

	// Both the inline key and the default file are loaded and reported.
	if len(kr.Fingerprints()) != 2 {
		t.Errorf("fingerprints = %v, want both keys", kr.Fingerprints())
	}
}

func TestNoKeyAnywhereIsAnError(t *testing.T) {
	kr, err := NewKeyRing(&config.Config{SSHKeyFile: "/nonexistent/key", SSHUseAgent: false}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := kr.AuthMethods(""); err == nil {
		t.Fatal("expected an error when no key is available")
	}
}

// The server never prompts: an encrypted key with no passphrase configured
// must fail with a message naming the env var, not hang on a terminal read.
func TestEncryptedKeyWithoutPassphraseFailsWithGuidance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enc")

	// A real OpenSSH encrypted key header is enough to reach the
	// PassphraseMissingError path.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	der, _ := x509.MarshalPKCS8PrivateKey(priv)

	block, err := x509.EncryptPEMBlock(rand.Reader, "PRIVATE KEY", der, []byte("hunter2"), x509.PEMCipherAES256)
	if err != nil {
		t.Skipf("EncryptPEMBlock unavailable: %v", err)
	}

	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = NewKeyRing(&config.Config{SSHKeyFile: path, SSHUseAgent: false}, nil)
	if err == nil {
		t.Fatal("expected an error for an encrypted key with no passphrase")
	}

	if !strings.Contains(err.Error(), "AGENTMGR_SSH_KEY_PASSPHRASE") {
		t.Errorf("error should name the env var, got: %v", err)
	}
}
