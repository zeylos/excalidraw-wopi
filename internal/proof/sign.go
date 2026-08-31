package proof

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"strconv"
	"strings"
	"time"
)

// dotnetEpochTicks is the number of .NET DateTime ticks (100 ns units)
// between 0001-01-01 00:00:00 (the .NET epoch) and 1970-01-01 00:00:00
// UTC (the Unix epoch).
const dotnetEpochTicks int64 = 621355968000000000

// Signer is the interface other packages depend on to sign WOPI requests,
// so callers do not need the concrete KeySet type.
type Signer interface {
	SignRequest(accessToken, url string, ts time.Time) (proof, proofOld, timestamp string)
}

// SignRequest signs a WOPI request with both the current and the old
// proof key, over the same signed base and timestamp, so the host accepts
// the request whichever key it still trusts (proof/current, proofOld/
// current, or proof/old).
func (ks *KeySet) SignRequest(accessToken, url string, ts time.Time) (proofB64, proofOldB64, timestamp string) {
	ticks := TimeToTicks(ts)
	base := signedBase(accessToken, url, ticks)

	proofB64 = base64.StdEncoding.EncodeToString(mustSign(ks.Current, base))
	proofOldB64 = base64.StdEncoding.EncodeToString(mustSign(ks.Old, base))
	timestamp = strconv.FormatInt(ticks, 10)
	return proofB64, proofOldB64, timestamp
}

// TimeToTicks converts a time to .NET DateTime ticks, the unit the WOPI
// X-WOPI-Timestamp header uses.
func TimeToTicks(t time.Time) int64 {
	return dotnetEpochTicks + t.UTC().UnixNano()/100
}

// signedBase builds the byte layout the WOPI proof-key specification
// defines: a 4-byte big-endian length prefix before each of the token
// and the uppercased URL, both UTF-8, then a 4-byte length prefix
// (always 8) before the 8-byte big-endian ticks value. The URL is
// uppercased before UTF-8 encoding; Drive's verifier does this step as
// `url.upper().encode("utf-8")`.
func signedBase(accessToken, url string, ticks int64) []byte {
	tokenBytes := []byte(accessToken)
	// strings.ToUpper's simple case mapping can diverge from Python's
	// str.upper() (Drive's verifier) on a few multi-rune expansions, e.g.
	// German straße or ligatures. That never triggers here: every URL
	// this package signs comes from an allowlisted WOPI origin with a
	// UUID path, so it is pure ASCII (see TestSignedURLsAreASCII).
	urlBytes := []byte(strings.ToUpper(url))

	buf := make([]byte, 0, 4+len(tokenBytes)+4+len(urlBytes)+4+8)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(tokenBytes)))
	buf = append(buf, tokenBytes...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(urlBytes)))
	buf = append(buf, urlBytes...)
	buf = binary.BigEndian.AppendUint32(buf, 8)
	buf = binary.BigEndian.AppendUint64(buf, uint64(ticks))
	return buf
}

// mustSign panics on error. RSASSA-PKCS1-v1_5 signing over a well-formed
// SHA-256 digest with a key this package itself generated or parsed does
// not fail; a panic here means a deeper bug, not a runtime condition to
// route to callers as an error return.
func mustSign(key *rsa.PrivateKey, data []byte) []byte {
	digest := sha256.Sum256(data)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return sig
}

var _ Signer = (*KeySet)(nil)
