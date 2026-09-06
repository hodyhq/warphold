// Package seal derives a key from the admin passphrase and seals secrets at rest.
package seal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/nacl/secretbox"
)

// Key is a 256-bit sealing key.
type Key [32]byte

// Opener opens one sealed value. It exists so a long-lived consumer -- the job
// scheduler, the mail sender -- holds a way to reach the CURRENT key instead
// of a copy of the key it was built with. Key is [32]byte, so passing one by
// value freezes it, and after a passphrase rotation the holder would go on
// unsealing with a retired key forever while every ciphertext in the store had
// been re-sealed under the new one.
//
// Key.Open is itself an Opener, so a caller that legitimately has a fixed key
// (a test, or a handler already holding the rotation lock) just passes k.Open.
type Opener func(sealed []byte) ([]byte, error)

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
	return writeSecretFile(path, hex.EncodeToString(k[:])+"\n")
}

// WritePendingKeyFile writes a rotation's new key beside the live one, tagged
// with the salt it was derived from: "hex(key) hex(salt)". The tag is the whole
// point of the file: a restart compares it with the salt in the store to tell
// whether the rotation's transaction committed (salts match, finish the
// rename) or not (salts differ, the pending key is dead).
//
// The tag survives the rename onto seal.key, where it is harmless - the salt is
// public and already in the store, and ReadKeyFile ignores it - and it leaves a
// rotated key file saying which salt it belongs to.
func WritePendingKeyFile(path string, k Key, salt []byte) error {
	return writeSecretFile(path, hex.EncodeToString(k[:])+" "+hex.EncodeToString(salt)+"\n")
}

// writeSecretFile writes content through a 0600 temp file in the same
// directory and renames it over path, so a pre-existing file ends up 0600 too
// (os.WriteFile keeps the permissions of a file that already exists) and a
// partially written secret is never visible.
func writeSecretFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, ".seal-key-*")
	if err != nil {
		return err
	}

	tmp := f.Name()

	defer func() {
		f.Close()      //nolint:errcheck
		os.Remove(tmp) //nolint:errcheck // no-op once the rename succeeded
	}()

	if err := f.Chmod(0o600); err != nil {
		return err
	}

	if _, err := f.WriteString(content); err != nil {
		return err
	}

	if err := f.Sync(); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		return err
	}

	// Best effort: without it the rename can be lost in a crash even though
	// the file's own contents were synced. It is not fatal - not every
	// platform lets a directory be opened and synced - and a lost rename of
	// the pending key is recovered from the salt in the store anyway.
	if d, err := os.Open(dir); err == nil {
		d.Sync()  //nolint:errcheck
		d.Close() //nolint:errcheck
	}

	return nil
}

// ReadKeyFile reads a key written by WriteKeyFile.
func ReadKeyFile(path string) (Key, error) {
	k, _, err := readKeyFile(path)
	return k, err
}

// ReadPendingKeyFile reads a key and salt written by WritePendingKeyFile. A
// file with no salt tag is rejected: only a tagged file can be recovered.
func ReadPendingKeyFile(path string) (Key, []byte, error) {
	k, salt, err := readKeyFile(path)
	if err == nil && len(salt) == 0 {
		return k, nil, errors.New("seal key file carries no salt")
	}

	return k, salt, err
}

func readKeyFile(path string) (Key, []byte, error) {
	var k Key

	b, err := os.ReadFile(path)
	if err != nil {
		return k, nil, err
	}

	fields := strings.Fields(string(b))
	if len(fields) == 0 || len(fields) > 2 {
		return k, nil, errors.New("seal key file is malformed")
	}

	raw, err := hex.DecodeString(fields[0])
	if err != nil || len(raw) != len(k) {
		return k, nil, errors.New("seal key file is malformed")
	}

	copy(k[:], raw)

	if len(fields) == 1 {
		return k, nil, nil
	}

	salt, err := hex.DecodeString(fields[1])
	if err != nil || len(salt) == 0 {
		return k, nil, errors.New("seal key file is malformed")
	}

	return k, salt, nil
}
