// Package secret encrypts tenant secrets (api_hash, bot tokens, proxy
// credentials, TDLib db keys) with a single AES-256-GCM master key supplied
// via env. The master key is the one secret that legitimately stays in the
// environment; everything else lives encrypted in Postgres.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

type Box struct {
	gcm cipher.AEAD
}

// New builds a Box from a base64-encoded 32-byte key.
// Generate one with: openssl rand -base64 32
func New(masterKeyB64 string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil {
		return nil, errors.New("MASTER_KEY must be base64")
	}
	if len(key) != 32 {
		return nil, errors.New("MASTER_KEY must decode to 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{gcm: gcm}, nil
}

// Encrypt returns nonce||ciphertext.
func (b *Box) Encrypt(plain []byte) ([]byte, error) {
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.gcm.Seal(nonce, nonce, plain, nil), nil
}

// Decrypt reverses Encrypt.
func (b *Box) Decrypt(ct []byte) ([]byte, error) {
	ns := b.gcm.NonceSize()
	if len(ct) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return b.gcm.Open(nil, ct[:ns], ct[ns:], nil)
}
