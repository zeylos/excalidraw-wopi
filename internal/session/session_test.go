package session

import (
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testParams() MintParams {
	return MintParams{
		FileID:      "file-1",
		WOPISrc:     "https://drive.example/api/v1.0/wopi/files/file-1",
		UserID:      "user-1",
		UserName:    "Ada Lovelace",
		CanWrite:    true,
		AccessToken: "drive-access-token",
		ExpiresAt:   time.Now().Add(10 * time.Hour).Truncate(time.Second),
	}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	m, err := New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	p := testParams()

	raw, err := m.Mint(p)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	claims, err := m.Verify(raw)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	if claims.FileID != p.FileID {
		t.Errorf("FileID = %q, want %q", claims.FileID, p.FileID)
	}
	if claims.WOPISrc != p.WOPISrc {
		t.Errorf("WOPISrc = %q, want %q", claims.WOPISrc, p.WOPISrc)
	}
	if claims.UserID != p.UserID {
		t.Errorf("UserID = %q, want %q", claims.UserID, p.UserID)
	}
	if claims.UserName != p.UserName {
		t.Errorf("UserName = %q, want %q", claims.UserName, p.UserName)
	}
	if claims.CanWrite != p.CanWrite {
		t.Errorf("CanWrite = %v, want %v", claims.CanWrite, p.CanWrite)
	}
	if claims.AccessToken != p.AccessToken {
		t.Errorf("AccessToken = %q, want %q", claims.AccessToken, p.AccessToken)
	}
	if !claims.ExpiresAt.Equal(p.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", claims.ExpiresAt, p.ExpiresAt)
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	m, err := New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	raw, err := m.Mint(testParams())
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	parts[2] = flipFirstByte(t, parts[2])
	tampered := strings.Join(parts, ".")

	if _, err := m.Verify(tampered); err == nil {
		t.Error("Verify() with a tampered signature: want an error, got nil")
	}
}

// TestSealUnsealRejectsTamperedCiphertext exercises the AES-GCM layer
// directly, underneath the JWT signature: a byte flip in the sealed token
// must fail the GCM authentication tag check on its own, independent of
// the outer JWT signature that also covers the tok claim.
func TestSealUnsealRejectsTamperedCiphertext(t *testing.T) {
	m, err := New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sealed, err := m.seal("drive-access-token", "file-1")
	if err != nil {
		t.Fatalf("seal() error = %v", err)
	}

	tampered := flipFirstByte(t, sealed)
	if _, err := m.unseal(tampered, "file-1"); err == nil {
		t.Error("unseal() with a tampered ciphertext: want an error, got nil")
	}
}

// TestUnsealRejectsMismatchedFileID checks that the seal is bound to the
// file id as AAD, so unsealing with a different file id must fail even
// though the ciphertext itself is untouched.
func TestUnsealRejectsMismatchedFileID(t *testing.T) {
	m, err := New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sealed, err := m.seal("drive-access-token", "file-1")
	if err != nil {
		t.Fatalf("seal() error = %v", err)
	}

	if _, err := m.unseal(sealed, "file-2"); err == nil {
		t.Error("unseal() with a mismatched file id: want an error, got nil")
	}
	if _, err := m.unseal(sealed, "file-1"); err != nil {
		t.Errorf("unseal() with the matching file id: %v", err)
	}
}

func TestVerifyRejectsTamperedSealedTokenClaim(t *testing.T) {
	m, err := New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	raw, err := m.Mint(testParams())
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	// The payload sits between the two dots of header.payload.signature.
	// Flipping a byte there changes the tok claim (or another claim), so
	// it exercises the JWT signature check that covers the whole payload,
	// tok included.
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	parts[1] = flipFirstByte(t, parts[1])
	tampered := strings.Join(parts, ".")

	if _, err := m.Verify(tampered); err == nil {
		t.Error("Verify() with a tampered payload: want an error, got nil")
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	m, err := New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	p := testParams()
	p.ExpiresAt = time.Now().Add(-time.Minute)

	raw, err := m.Mint(p)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	if _, err := m.Verify(raw); err == nil {
		t.Error("Verify() with an expired token: want an error, got nil")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	m1, err := New([]byte("secret number one, plenty of entropy"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	m2, err := New([]byte("a completely different secret value"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	raw, err := m1.Mint(testParams())
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	if _, err := m2.Verify(raw); err == nil {
		t.Error("Verify() with the wrong key: want an error, got nil")
	}
}

// TestVerifyRejectsAlgConfusion checks that a token signed with a
// different algorithm than the HS256 Verify expects must be rejected,
// even though its claims are otherwise well-formed.
func TestVerifyRejectsAlgConfusion(t *testing.T) {
	m, err := New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	claims := tokenClaims{
		FileID: "file-1",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign RS256 token: %v", err)
	}

	if _, err := m.Verify(raw); err == nil {
		t.Error("Verify() with an RS256-signed token: want an error, got nil")
	}
}

// TestVerifyRejectsNoneAlg checks that a token that claims the "none"
// algorithm and carries no signature must be rejected, not accepted as
// an unsigned but otherwise trusted token.
func TestVerifyRejectsNoneAlg(t *testing.T) {
	m, err := New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	claims := tokenClaims{
		FileID: "file-1",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none-alg token: %v", err)
	}

	if _, err := m.Verify(raw); err == nil {
		t.Error("Verify() with a none-alg token: want an error, got nil")
	}
}

// TestVerifyRejectsMissingExpiry checks that a token with no exp claim at
// all must be rejected, not treated as never expiring.
func TestVerifyRejectsMissingExpiry(t *testing.T) {
	m, err := New([]byte("a test secret with enough entropy"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sealed, err := m.seal("drive-access-token", "file-1")
	if err != nil {
		t.Fatalf("seal() error = %v", err)
	}
	claims := tokenClaims{FileID: "file-1", SealedToken: sealed}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.signingKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	if _, err := m.Verify(raw); err == nil {
		t.Error("Verify() with no exp claim: want an error, got nil")
	}
}

// flipFirstByte flips one bit in the first character of a base64url
// segment, staying inside the base64url alphabet so the string still
// decodes, only to different bytes. It flips the first character, not
// the last: a base64 group's trailing symbol can carry padding bits that
// a non-strict decoder ignores, so a flip there sometimes decodes to the
// same bytes as the original, silently defeating the test. The first
// symbol always encodes real data, for any input length.
func flipFirstByte(t *testing.T, s string) string {
	t.Helper()
	if s == "" {
		t.Fatal("cannot flip a byte in an empty string")
	}
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}
