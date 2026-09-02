package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// clearEnv unsets every config env var, so a test starts from a known state
// regardless of what the host shell exports. t.Setenv registers the
// restore for cleanup; the Unsetenv call after it clears the var for the
// test itself, since t.Setenv cannot set a variable to "absent".
func clearEnv(t *testing.T) {
	t.Helper()
	names := []string{
		"LISTEN_ADDR",
		"PUBLIC_URL",
		"SESSION_SECRET",
		"SESSION_SECRET_FILE",
		"PROOF_KEY_PATH",
		"MAX_IMAGE_BYTES",
		"MAX_SCENE_BYTES",
		"SOCKET_BUFFER_BYTES",
		"WOPI_ALLOWED_ORIGINS",
		"PEERS",
		"SELF",
		"DNS_PEERS",
		"DNS_SELF",
	}
	for _, n := range names {
		key := envPrefix + n
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}

	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.PublicURL != defaultPublicURL {
		t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, defaultPublicURL)
	}
	if cfg.ProofKeyPath != defaultProofKeyPath {
		t.Errorf("ProofKeyPath = %q, want %q", cfg.ProofKeyPath, defaultProofKeyPath)
	}
	if cfg.MaxImageBytes != defaultMaxImageBytes {
		t.Errorf("MaxImageBytes = %d, want %d", cfg.MaxImageBytes, defaultMaxImageBytes)
	}
	if cfg.MaxSceneBytes != defaultMaxSceneBytes {
		t.Errorf("MaxSceneBytes = %d, want %d", cfg.MaxSceneBytes, defaultMaxSceneBytes)
	}
	if cfg.SocketBufferBytes != defaultSocketBufferBytes {
		t.Errorf("SocketBufferBytes = %d, want %d", cfg.SocketBufferBytes, defaultSocketBufferBytes)
	}
	if cfg.SessionSecret == "" {
		t.Error("SessionSecret must not be empty when unset; Load must generate one")
	}
}

func TestLoadOverrides(t *testing.T) {
	clearEnv(t)

	t.Setenv(envPrefix+"LISTEN_ADDR", "127.0.0.1:9090")
	t.Setenv(envPrefix+"PUBLIC_URL", "https://example.org")
	const fixedSecret = "a-fixed-secret-with-enough-entropy-bytes"
	t.Setenv(envPrefix+"SESSION_SECRET", fixedSecret)
	t.Setenv(envPrefix+"PROOF_KEY_PATH", "/etc/excalidraw-wopi/proof-key.pem")
	t.Setenv(envPrefix+"MAX_IMAGE_BYTES", "1000")
	t.Setenv(envPrefix+"MAX_SCENE_BYTES", "2000")
	t.Setenv(envPrefix+"SOCKET_BUFFER_BYTES", "5000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, "127.0.0.1:9090")
	}
	if cfg.PublicURL != "https://example.org" {
		t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, "https://example.org")
	}
	if cfg.SessionSecret != fixedSecret {
		t.Errorf("SessionSecret = %q, want %q", cfg.SessionSecret, fixedSecret)
	}
	if cfg.ProofKeyPath != "/etc/excalidraw-wopi/proof-key.pem" {
		t.Errorf("ProofKeyPath = %q, want %q", cfg.ProofKeyPath, "/etc/excalidraw-wopi/proof-key.pem")
	}
	if cfg.MaxImageBytes != 1000 {
		t.Errorf("MaxImageBytes = %d, want 1000", cfg.MaxImageBytes)
	}
	if cfg.MaxSceneBytes != 2000 {
		t.Errorf("MaxSceneBytes = %d, want 2000", cfg.MaxSceneBytes)
	}
	if cfg.SocketBufferBytes != 5000 {
		t.Errorf("SocketBufferBytes = %d, want 5000", cfg.SocketBufferBytes)
	}
}

func TestLoadRejectsInvalidInteger(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"MAX_IMAGE_BYTES", "not-a-number")

	if _, err := Load(); err == nil {
		t.Error("Load() must return an error for a non-integer size limit")
	}
}

// TestLoadAllowsSocketBufferSmallerThanSceneLimit checks that
// SOCKET_BUFFER_BYTES only has to clear minSocketBufferBytes: an operator
// can shrink the pre-auth transport read limit independently of
// MAX_SCENE_BYTES.
func TestLoadAllowsSocketBufferSmallerThanSceneLimit(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"MAX_IMAGE_BYTES", "1000000")
	t.Setenv(envPrefix+"MAX_SCENE_BYTES", "2000000")
	t.Setenv(envPrefix+"SOCKET_BUFFER_BYTES", "8192")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}
	if cfg.SocketBufferBytes != 8192 {
		t.Errorf("SocketBufferBytes = %d, want 8192", cfg.SocketBufferBytes)
	}
}

func TestLoadRejectsSocketBufferBelowFloor(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"SOCKET_BUFFER_BYTES", "1")

	if _, err := Load(); err == nil {
		t.Error("Load() must reject a socket buffer under the minimum floor")
	}
}

func TestLoadRejectsImageLimitAboveSceneLimit(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"MAX_IMAGE_BYTES", "3000")
	t.Setenv(envPrefix+"MAX_SCENE_BYTES", "2000")

	if _, err := Load(); err == nil {
		t.Error("Load() must reject an image limit above the scene limit")
	}
}

func TestLoadRejectsRelativePublicURL(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"PUBLIC_URL", "/not-absolute")

	if _, err := Load(); err == nil {
		t.Error("Load() must reject a PUBLIC_URL that is not absolute")
	}
}

func TestLoadStripsTrailingSlashFromPublicURL(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"PUBLIC_URL", "https://example.org/")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}
	if cfg.PublicURL != "https://example.org" {
		t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, "https://example.org")
	}
}

func TestLoadRejectsShortSessionSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"SESSION_SECRET", "too-short")

	if _, err := Load(); err == nil {
		t.Error("Load() must reject a SESSION_SECRET under 32 bytes")
	}
}

func TestLoadReadsSessionSecretFromFile(t *testing.T) {
	clearEnv(t)
	const secret = "a-fixed-secret-with-enough-entropy-bytes"
	path := filepath.Join(t.TempDir(), "session-secret")
	if err := os.WriteFile(path, []byte(" \t"+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envPrefix+"SESSION_SECRET_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}
	if cfg.SessionSecret != secret {
		t.Errorf("SessionSecret = %q, want %q", cfg.SessionSecret, secret)
	}
}

func TestLoadRejectsSessionSecretAndFileTogether(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "session-secret")
	if err := os.WriteFile(path, []byte("a-fixed-secret-with-enough-entropy-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envPrefix+"SESSION_SECRET", "a-fixed-secret-with-enough-entropy-bytes")
	t.Setenv(envPrefix+"SESSION_SECRET_FILE", path)

	if _, err := Load(); err == nil {
		t.Error("Load() must reject SESSION_SECRET and SESSION_SECRET_FILE set together")
	}
}

func TestLoadRejectsEmptySessionSecretFile(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "session-secret")
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envPrefix+"SESSION_SECRET_FILE", path)

	if _, err := Load(); err == nil {
		t.Error("Load() must reject a SESSION_SECRET_FILE that holds no secret")
	}
}

func TestLoadRejectsMissingSessionSecretFile(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"SESSION_SECRET_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	if _, err := Load(); err == nil {
		t.Error("Load() must reject a SESSION_SECRET_FILE that does not exist")
	}
}

func TestLoadRejectsShortSessionSecretFromFile(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "session-secret")
	if err := os.WriteFile(path, []byte("too-short"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envPrefix+"SESSION_SECRET_FILE", path)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() must reject a SESSION_SECRET_FILE whose content is under 32 bytes")
	}
	if !strings.Contains(err.Error(), "SESSION_SECRET_FILE") {
		t.Errorf("error = %q, want it to mention SESSION_SECRET_FILE", err.Error())
	}
}

func TestLoadParsesAllowedOrigins(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"WOPI_ALLOWED_ORIGINS", " http://localhost:8000 , https://drive.example.org ,, ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}

	want := []string{"http://localhost:8000", "https://drive.example.org"}
	if !reflect.DeepEqual(cfg.AllowedWOPIOrigins, want) {
		t.Errorf("AllowedWOPIOrigins = %v, want %v", cfg.AllowedWOPIOrigins, want)
	}
}

func TestLoadDefaultsToNoAllowedOrigins(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}
	if len(cfg.AllowedWOPIOrigins) != 0 {
		t.Errorf("AllowedWOPIOrigins = %v, want empty", cfg.AllowedWOPIOrigins)
	}
}

func TestLoadDefaultsToPeersDisabled(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}
	if cfg.DNSPeers != "" {
		t.Errorf("DNSPeers = %q, want empty", cfg.DNSPeers)
	}
	if cfg.DNSSelf != "" {
		t.Errorf("DNSSelf = %q, want empty", cfg.DNSSelf)
	}
}

func TestLoadRejectsDNSPeersWithoutDNSSelf(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"DNS_PEERS", "svc.local:8080")

	if _, err := Load(); err == nil {
		t.Error("Load() must reject DNS_PEERS set without DNS_SELF")
	}
}

func TestLoadAcceptsDNSPeersAndDNSSelf(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"DNS_PEERS", "svc.local:8080")
	t.Setenv(envPrefix+"DNS_SELF", "http://a")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}
	if cfg.DNSPeers != "svc.local:8080" {
		t.Errorf("DNSPeers = %q, want %q", cfg.DNSPeers, "svc.local:8080")
	}
	if cfg.DNSSelf != "http://a" {
		t.Errorf("DNSSelf = %q, want %q", cfg.DNSSelf, "http://a")
	}
}

func TestLoadStripsTrailingSlashFromDNSSelf(t *testing.T) {
	clearEnv(t)
	t.Setenv(envPrefix+"DNS_PEERS", "svc.local:8080")
	t.Setenv(envPrefix+"DNS_SELF", "http://a/")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}
	if cfg.DNSSelf != "http://a" {
		t.Errorf("DNSSelf = %q, want %q", cfg.DNSSelf, "http://a")
	}
}
