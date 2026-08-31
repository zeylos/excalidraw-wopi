// Package proof manages the RSA keypair that signs WOPI proof headers, and
// it renders the public parts that the discovery XML publishes.
//
// Key file format: a proof key file holds two concatenated PEM blocks,
// PKCS8-encoded, current key first and old key second. A file with only
// one block means the old key equals the current key, the state right
// after first-start generation. This format needs no extra parser and
// round-trips through any PEM-aware tool.
package proof

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/zeylos/excalidraw-wopi/internal/config"
)

// EnvPEMVar is the environment variable that injects the proof key PEM
// content directly, for a Kubernetes secret mount. Its content wins over
// cfg.ProofKeyPath when both are set.
const EnvPEMVar = "EXCALIDRAW_WOPI_PROOF_KEY_PEM"

const rsaKeyBits = 2048

const pemBlockType = "PRIVATE KEY"

// minRSAKeyBits is the smallest proof key modulus size this package
// accepts. Drive's proof check does not enforce a minimum, so a weak
// injected key (env var or file) must be rejected here instead.
const minRSAKeyBits = 2048

// KeySet holds the current and the old RSA proof key. A WOPI host
// accepts a signature made with either one, so a deployment can rotate
// the current key without breaking in-flight requests signed under the
// old one.
type KeySet struct {
	Current *rsa.PrivateKey
	Old     *rsa.PrivateKey
}

// PublicKeyParts holds the fields the discovery XML's <proof-key> element
// publishes for one key: the base64 SubjectPublicKeyInfo DER, and the
// base64 big-endian modulus and exponent.
type PublicKeyParts struct {
	Value    string
	Modulus  string
	Exponent string
}

// Load builds the proof KeySet. It reads the PEM content from EnvPEMVar
// when that variable is set. Otherwise it reads cfg.ProofKeyPath, and
// when that file is absent, it generates a new RSA key, sets the old key
// equal to the current key, and persists both to cfg.ProofKeyPath.
func Load(cfg config.Config) (*KeySet, error) {
	if pemContent, ok := os.LookupEnv(EnvPEMVar); ok {
		return parseKeySet([]byte(pemContent))
	}

	data, err := os.ReadFile(cfg.ProofKeyPath)
	if err == nil {
		return parseKeySet(data)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read proof key file %s: %w", cfg.ProofKeyPath, err)
	}

	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, fmt.Errorf("generate proof key: %w", err)
	}
	ks := &KeySet{Current: key, Old: key}

	persisted, err := persist(cfg.ProofKeyPath, ks)
	if err != nil {
		return nil, fmt.Errorf("persist proof key file %s: %w", cfg.ProofKeyPath, err)
	}
	return persisted, nil
}

func parseKeySet(data []byte) (*KeySet, error) {
	current, rest, err := decodeNextKey(data)
	if err != nil {
		return nil, fmt.Errorf("decode proof key: %w", err)
	}
	if current == nil {
		return nil, errors.New("decode proof key: no PEM block found")
	}

	old, _, err := decodeNextKey(rest)
	if err != nil {
		return nil, fmt.Errorf("decode old proof key: %w", err)
	}
	if old == nil {
		old = current
	}

	return &KeySet{Current: current, Old: old}, nil
}

// decodeNextKey reads the next PEM block from data. It returns a nil key,
// not an error, when data holds no further block, so a one-block file
// (the first-start case, current == old) parses cleanly; parseKeySet
// itself turns a nil first block into an error, since that call site has
// no fallback.
func decodeNextKey(data []byte) (*rsa.PrivateKey, []byte, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, rest, nil
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, rest, err
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, rest, fmt.Errorf("proof key is a %T, want *rsa.PrivateKey", key)
	}
	if rsaKey.N.BitLen() < minRSAKeyBits {
		return nil, rest, fmt.Errorf("proof key is %d bits, want at least %d", rsaKey.N.BitLen(), minRSAKeyBits)
	}
	if err := rsaKey.Validate(); err != nil {
		return nil, rest, fmt.Errorf("proof key failed validation: %w", err)
	}
	return rsaKey, rest, nil
}

// persist writes ks to path and returns the KeySet now durably on disk.
// It writes a temp file in the same directory, then links it into place:
// os.Link fails instead of silently overwriting when path already
// exists, unlike os.Rename, so persist can tell a losing concurrent
// first-start race (two replicas generating a key at once) from a plain
// write. The loser discards its own generated key and adopts the
// winner's, so every replica converges on the one key that made it to
// disk.
func persist(path string, ks *KeySet) (*KeySet, error) {
	pemBytes, err := encodeKeySet(ks)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create proof key directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".proof-key-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp proof key file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the link below moves it into place

	if _, err := tmp.Write(pemBytes); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write temp proof key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temp proof key file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return nil, fmt.Errorf("chmod temp proof key file: %w", err)
	}

	if err := os.Link(tmpPath, path); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("link proof key file into place: %w", err)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read winning proof key file %s: %w", path, readErr)
		}
		winner, parseErr := parseKeySet(data)
		if parseErr != nil {
			return nil, fmt.Errorf("parse winning proof key file %s: %w", path, parseErr)
		}
		return winner, nil
	}
	return ks, nil
}

func encodeKeySet(ks *KeySet) ([]byte, error) {
	var out []byte
	for _, key := range []*rsa.PrivateKey{ks.Current, ks.Old} {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("marshal proof key: %w", err)
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: pemBlockType, Bytes: der})...)
	}
	return out, nil
}

// CurrentPublicParts returns the discovery XML fields for the current key.
func (ks *KeySet) CurrentPublicParts() (PublicKeyParts, error) {
	return publicKeyParts(&ks.Current.PublicKey)
}

// OldPublicParts returns the discovery XML fields for the old key.
func (ks *KeySet) OldPublicParts() (PublicKeyParts, error) {
	return publicKeyParts(&ks.Old.PublicKey)
}

func publicKeyParts(pub *rsa.PublicKey) (PublicKeyParts, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return PublicKeyParts{}, fmt.Errorf("marshal public key: %w", err)
	}

	return PublicKeyParts{
		Value:    base64.StdEncoding.EncodeToString(spki),
		Modulus:  base64.StdEncoding.EncodeToString(pub.N.Bytes()),
		Exponent: base64.StdEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}, nil
}
