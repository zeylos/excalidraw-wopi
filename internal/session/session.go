// Package session mints and verifies the session JWT. The JWT carries
// the launch identity plus the WOPI access token, sealed with
// AES-256-GCM, so the server stays stateless: after a restart, a
// client's existing JWT still unseals to a usable access token.
package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Distinct HKDF info strings keep the signing key and the sealing key
// independent, even though both derive from the same server secret.
const (
	signingKeyInfo = "excalidraw-wopi/session/signing-key/v1"
	sealingKeyInfo = "excalidraw-wopi/session/sealing-key/v1"
	signingKeyLen  = 32
	sealingKeyLen  = 32 // AES-256
)

// Manager mints and verifies session JWTs. The zero value is not usable;
// build one with New.
type Manager struct {
	signingKey []byte
	sealingKey []byte
}

// New derives a Manager's signing and sealing keys from secret with
// HKDF-SHA256. secret is cfg.SessionSecret; it should hold at least 32
// bytes of entropy.
func New(secret []byte) (*Manager, error) {
	signingKey, err := hkdf.Key(sha256.New, secret, nil, signingKeyInfo, signingKeyLen)
	if err != nil {
		return nil, fmt.Errorf("session: derive signing key: %w", err)
	}
	sealingKey, err := hkdf.Key(sha256.New, secret, nil, sealingKeyInfo, sealingKeyLen)
	if err != nil {
		return nil, fmt.Errorf("session: derive sealing key: %w", err)
	}
	return &Manager{signingKey: signingKey, sealingKey: sealingKey}, nil
}

// MintParams carries the values Mint seals into a new session JWT.
type MintParams struct {
	FileID      string
	WOPISrc     string
	UserID      string
	UserName    string
	CanWrite    bool
	AccessToken string
	// ExpiresAt is the WOPI access token's end of life, computed from the
	// launch form's access_token_ttl. The JWT's exp claim matches it, so a
	// session never outlives the token it seals.
	ExpiresAt time.Time
}

// Claims holds the values Verify recovers from a session JWT, with the
// WOPI access token already unsealed.
type Claims struct {
	FileID      string
	WOPISrc     string
	UserID      string
	UserName    string
	CanWrite    bool
	AccessToken string
	ExpiresAt   time.Time
}

// tokenClaims is the wire shape of the JWT claims set.
type tokenClaims struct {
	FileID      string `json:"fid"`
	WOPISrc     string `json:"src"`
	UserID      string `json:"uid"`
	UserName    string `json:"name"`
	CanWrite    bool   `json:"canWrite"`
	SealedToken string `json:"tok"`
	jwt.RegisteredClaims
}

// Mint builds a session JWT for p, HS256-signed, with the WOPI access
// token sealed inside the tok claim.
func (m *Manager) Mint(p MintParams) (string, error) {
	sealed, err := m.seal(p.AccessToken, p.FileID)
	if err != nil {
		return "", fmt.Errorf("session: seal access token: %w", err)
	}

	claims := tokenClaims{
		FileID:      p.FileID,
		WOPISrc:     p.WOPISrc,
		UserID:      p.UserID,
		UserName:    p.UserName,
		CanWrite:    p.CanWrite,
		SealedToken: sealed,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(p.ExpiresAt),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.signingKey)
	if err != nil {
		return "", fmt.Errorf("session: sign token: %w", err)
	}
	return signed, nil
}

// Verify checks raw's signature and expiry, and unseals the WOPI access
// token. It rejects a token signed with the wrong key, a tampered token
// (signature or sealed payload), and an expired token.
func (m *Manager) Verify(raw string) (Claims, error) {
	var claims tokenClaims
	_, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) {
		return m.signingKey, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())
	if err != nil {
		return Claims{}, fmt.Errorf("session: verify token: %w", err)
	}

	// FileID is authenticated by the JWT signature just checked above, so
	// it is safe to use as the seal's AAD here: unseal binds the sealed
	// access token to the file it was minted for.
	accessToken, err := m.unseal(claims.SealedToken, claims.FileID)
	if err != nil {
		return Claims{}, fmt.Errorf("session: unseal access token: %w", err)
	}

	var expiresAt time.Time
	if claims.ExpiresAt != nil {
		expiresAt = claims.ExpiresAt.Time
	}

	return Claims{
		FileID:      claims.FileID,
		WOPISrc:     claims.WOPISrc,
		UserID:      claims.UserID,
		UserName:    claims.UserName,
		CanWrite:    claims.CanWrite,
		AccessToken: accessToken,
		ExpiresAt:   expiresAt,
	}, nil
}

// seal encrypts plaintext with AES-256-GCM under a random nonce, binding
// fileID as additional authenticated data, and returns the nonce and the
// ciphertext, concatenated and base64url-encoded. Binding the file id
// stops a sealed token from one session's tok claim being usable, even in
// principle, as a stand-in for another file's.
func (m *Manager) seal(plaintext, fileID string) (string, error) {
	gcm, err := m.gcm()
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("read nonce: %w", err)
	}

	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(fileID))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// unseal reverses seal. fileID must match the value seal was called
// with, or the GCM authentication tag check fails.
func (m *Manager) unseal(sealed, fileID string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("decode sealed token: %w", err)
	}

	gcm, err := m.gcm()
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("sealed token is shorter than a nonce")
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(fileID))
	if err != nil {
		return "", fmt.Errorf("open sealed token: %w", err)
	}
	return string(plaintext), nil
}

func (m *Manager) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(m.sealingKey)
	if err != nil {
		return nil, fmt.Errorf("build AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build GCM mode: %w", err)
	}
	return gcm, nil
}
