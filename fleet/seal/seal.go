// Package seal derives a key from the admin passphrase and seals secrets at rest.
package seal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/nacl/secretbox"
)

// Key is a 256-bit sealing key.
type Key [32]byte

// ErrTampered is returned when sealed data does not authenticate.
var ErrTampered = errors.New("sealed data is corrupt or the key is wrong")

const nonceSize = 24

// NewSalt returns 16 random bytes.
func NewSalt() ([]byte, error) {
	b := make([]byte, 16)
	_, err := io.ReadFull(rand.Reader, b)
	return b, err
}

// Derive derives the key from a passphrase with argon2id (64 MiB, 3 passes, 4 lanes).
func Derive(passphrase string, salt []byte) Key {
	var k Key
	copy(k[:], argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 4, 32))
	return k
}

// Seal encrypts plain with a fresh nonce: nonce || box.
func (k Key) Seal(plain []byte) ([]byte, error) {
	var nonce [nonceSize]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, err
	}
	key := [32]byte(k)
	return secretbox.Seal(nonce[:], plain, &nonce, &key), nil
}

// Open decrypts data produced by Seal.
func (k Key) Open(sealed []byte) ([]byte, error) {
	if len(sealed) < nonceSize+secretbox.Overhead {
		return nil, ErrTampered
	}
	var nonce [nonceSize]byte
	copy(nonce[:], sealed[:nonceSize])
	key := [32]byte(k)
	out, ok := secretbox.Open(nil, sealed[nonceSize:], &nonce, &key)
	if !ok {
		return nil, ErrTampered
	}
	return out, nil
}

// WriteKeyFile writes the key hex-encoded with mode 0600.
func WriteKeyFile(path string, k Key) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(hex.EncodeToString(k[:])+"\n"), 0o600)
}

// ReadKeyFile reads a key written by WriteKeyFile.
func ReadKeyFile(path string) (Key, error) {
	var k Key
	b, err := os.ReadFile(path)
	if err != nil {
		return k, err
	}
	raw, err := hex.DecodeString(string(trimNL(b)))
	if err != nil || len(raw) != len(k) {
		return k, errors.New("seal key file is malformed")
	}
	copy(k[:], raw)
	return k, nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
