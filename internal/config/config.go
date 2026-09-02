// Package config loads and validates the service configuration.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const envPrefix = "EXCALIDRAW_WOPI_"

// DefaultListenAddr is the LISTEN_ADDR default. The healthcheck mode in
// cmd/excalidraw-wopi reuses it, so the two stay in step.
const DefaultListenAddr = ":8080"

const (
	defaultPublicURL         = "http://localhost:8080"
	defaultProofKeyPath      = "/var/lib/excalidraw-wopi/proof-key.pem"
	defaultMaxImageBytes     = 10 * 1024 * 1024
	defaultMaxSceneBytes     = 50 * 1024 * 1024
	defaultSocketBufferBytes = 60 * 1024 * 1024
	// minSocketBufferBytes is SOCKET_BUFFER_BYTES's floor: an operator can
	// shrink the pre-auth transport read limit independently of
	// MAX_SCENE_BYTES, but a buffer under this size cannot carry a
	// socket.io handshake.
	minSocketBufferBytes     = 4 * 1024
	sessionSecretRandomBytes = 32
	minSessionSecretBytes    = 32
)

// Config holds every setting the service reads at boot.
type Config struct {
	ListenAddr        string
	PublicURL         string
	SessionSecret     string
	ProofKeyPath      string
	MaxImageBytes     int64
	MaxSceneBytes     int64
	SocketBufferBytes int64
	// AllowedWOPIOrigins lists the WOPI host origins (scheme://host[:port])
	// that /launch accepts a WOPISrc from. Empty means /launch refuses
	// every request; see EXCALIDRAW_WOPI_WOPI_ALLOWED_ORIGINS.
	AllowedWOPIOrigins []string
	// DNSPeers is the raw "<host>:<port>" DNS peer spec; the internal/peers
	// package resolves it. Empty disables multi-replica routing.
	DNSPeers string
	// DNSSelf is this replica's own advertised URL. Required when DNSPeers
	// is set.
	DNSSelf string
}

// Load reads the config from environment variables and validates it.
// It reads the session secret from SESSION_SECRET_FILE when that
// variable is set, and it rejects SESSION_SECRET and
// SESSION_SECRET_FILE together. It generates a random SessionSecret
// and logs a warning when neither is set.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr:         getEnv("LISTEN_ADDR", DefaultListenAddr),
		PublicURL:          getEnv("PUBLIC_URL", defaultPublicURL),
		SessionSecret:      getEnv("SESSION_SECRET", ""),
		ProofKeyPath:       getEnv("PROOF_KEY_PATH", defaultProofKeyPath),
		AllowedWOPIOrigins: parseOriginList(getEnv("WOPI_ALLOWED_ORIGINS", "")),
		DNSPeers:           getEnv("DNS_PEERS", ""),
		DNSSelf:            getEnv("DNS_SELF", ""),
	}
	sessionSecretFile := getEnv("SESSION_SECRET_FILE", "")

	if len(cfg.AllowedWOPIOrigins) == 0 {
		slog.Warn("no WOPI allowed origins configured; /launch will refuse every request",
			"env", envPrefix+"WOPI_ALLOWED_ORIGINS")
	}

	var err error
	if cfg.MaxImageBytes, err = getEnvInt64("MAX_IMAGE_BYTES", defaultMaxImageBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxSceneBytes, err = getEnvInt64("MAX_SCENE_BYTES", defaultMaxSceneBytes); err != nil {
		return Config{}, err
	}
	if cfg.SocketBufferBytes, err = getEnvInt64("SOCKET_BUFFER_BYTES", defaultSocketBufferBytes); err != nil {
		return Config{}, err
	}

	if cfg.SessionSecret != "" && sessionSecretFile != "" {
		return Config{}, fmt.Errorf("set only one of %sSESSION_SECRET or %sSESSION_SECRET_FILE", envPrefix, envPrefix)
	}

	if cfg.SessionSecret == "" && sessionSecretFile != "" {
		secret, err := os.ReadFile(sessionSecretFile)
		if err != nil {
			return Config{}, fmt.Errorf("%sSESSION_SECRET_FILE: read %q: %w", envPrefix, sessionSecretFile, err)
		}
		// A mounted Secret file usually ends with a newline that is not part of the secret.
		trimmed := strings.TrimSpace(string(secret))
		if trimmed == "" {
			return Config{}, fmt.Errorf("%sSESSION_SECRET_FILE: %q holds no secret", envPrefix, sessionSecretFile)
		}
		if len(trimmed) < minSessionSecretBytes {
			return Config{}, fmt.Errorf("%sSESSION_SECRET_FILE: the secret must be at least %d bytes", envPrefix, minSessionSecretBytes)
		}
		cfg.SessionSecret = trimmed
	}

	if cfg.SessionSecret == "" {
		secret, err := randomSecret()
		if err != nil {
			return Config{}, fmt.Errorf("generate random session secret: %w", err)
		}
		cfg.SessionSecret = secret
		slog.Warn("no session secret set, generated a random one for this run; sessions do not survive a restart",
			"env", envPrefix+"SESSION_SECRET")
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate checks cfg and normalizes PublicURL and DNSSelf by stripping a
// trailing slash, so route building elsewhere can always append a path
// unchanged.
func (c *Config) validate() error {
	if err := c.validateRequiredFields(); err != nil {
		return err
	}
	if err := c.validateSizeLimits(); err != nil {
		return err
	}
	return c.validatePeers()
}

// validateRequiredFields checks ListenAddr, PublicURL, and ProofKeyPath, and
// normalizes PublicURL by stripping a trailing slash.
func (c *Config) validateRequiredFields() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("%sLISTEN_ADDR must not be empty", envPrefix)
	}
	if c.PublicURL == "" {
		return fmt.Errorf("%sPUBLIC_URL must not be empty", envPrefix)
	}
	parsedURL, err := url.Parse(c.PublicURL)
	if err != nil {
		return fmt.Errorf("%sPUBLIC_URL: invalid URL %q: %w", envPrefix, c.PublicURL, err)
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return fmt.Errorf("%sPUBLIC_URL must be an absolute URL, got %q", envPrefix, c.PublicURL)
	}
	c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	if c.ProofKeyPath == "" {
		return fmt.Errorf("%sPROOF_KEY_PATH must not be empty", envPrefix)
	}
	return nil
}

// validateSizeLimits checks MaxImageBytes, MaxSceneBytes, SocketBufferBytes,
// and SessionSecret against their floors and against each other.
func (c *Config) validateSizeLimits() error {
	if c.MaxImageBytes <= 0 {
		return fmt.Errorf("%sMAX_IMAGE_BYTES must be positive", envPrefix)
	}
	if c.MaxSceneBytes <= 0 {
		return fmt.Errorf("%sMAX_SCENE_BYTES must be positive", envPrefix)
	}
	if c.MaxImageBytes > c.MaxSceneBytes {
		return fmt.Errorf("%sMAX_IMAGE_BYTES must not exceed %sMAX_SCENE_BYTES", envPrefix, envPrefix)
	}
	if c.SocketBufferBytes < minSocketBufferBytes {
		return fmt.Errorf("%sSOCKET_BUFFER_BYTES must be at least %d bytes", envPrefix, minSocketBufferBytes)
	}
	if len(c.SessionSecret) < minSessionSecretBytes {
		return fmt.Errorf("%sSESSION_SECRET must be at least %d bytes", envPrefix, minSessionSecretBytes)
	}
	return nil
}

// validatePeers normalizes DNSSelf by stripping a trailing slash, then
// requires it when DNSPeers is set.
func (c *Config) validatePeers() error {
	c.DNSSelf = strings.TrimSuffix(c.DNSSelf, "/")
	if c.DNSPeers == "" {
		return nil
	}
	if c.DNSSelf == "" {
		return fmt.Errorf("%sDNS_SELF must be set when %sDNS_PEERS is set", envPrefix, envPrefix)
	}
	return nil
}

func getEnv(name, fallback string) string {
	if v, ok := os.LookupEnv(envPrefix + name); ok {
		return v
	}
	return fallback
}

func getEnvInt64(name string, fallback int64) (int64, error) {
	v, ok := os.LookupEnv(envPrefix + name)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s%s: invalid integer %q: %w", envPrefix, name, v, err)
	}
	return n, nil
}

// parseOriginList splits raw on commas, trims whitespace around each
// entry, and drops empty entries. It returns nil for an empty raw, so a
// caller can distinguish "unset" from "set to one empty entry".
func parseOriginList(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func randomSecret() (string, error) {
	buf := make([]byte, sessionSecretRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
