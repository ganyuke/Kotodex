package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
)

// Keys holds the server's secrets. They come from one master key that is
// kept outside the database (KOTODEX_SECRET or secret.key in the data
// directory), so a copied database or a database-only backup can't be used
// to forge image links or read linked MangaDex accounts.
type Keys struct {
	sign []byte      // HMAC key for image links
	aead cipher.AEAD // AES-256-GCM for MangaDex client secrets and refresh tokens
}

const keyFileName = "secret.key"

func loadKeys(keyPath string) (*Keys, error) {
	master, err := masterKey(keyPath)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("kotodex mangadex credentials"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Keys{sign: master, aead: aead}, nil
}

// masterKey reads the key from KOTODEX_SECRET or the key file, creating the
// file on first start.
func masterKey(keyPath string) ([]byte, error) {
	if env := os.Getenv("KOTODEX_SECRET"); env != "" {
		if len(env) < 32 {
			return nil, errors.New("KOTODEX_SECRET must be at least 32 characters")
		}
		sum := sha256.Sum256([]byte(env))
		return sum[:], nil
	}

	if data, err := os.ReadFile(keyPath); err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("%s is damaged (delete it to make a new one, then link MangaDex accounts again)", keyPath)
		}
		if st, err := os.Stat(keyPath); err == nil && st.Mode().Perm()&0o077 != 0 {
			os.Chmod(keyPath, 0o600)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", keyPath, err)
	}
	log.Printf("Created %s (keep a copy outside your database backups)", keyPath)
	return key, nil
}

func (k *Keys) seal(plain string) string {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return base64.RawStdEncoding.EncodeToString(k.aead.Seal(nonce, nonce, []byte(plain), nil))
}

var errWrongKey = errors.New("can't read the saved MangaDex sign-in with this server's key (link the account again)")

func (k *Keys) open(sealed string) (string, error) {
	data, err := base64.RawStdEncoding.DecodeString(sealed)
	n := k.aead.NonceSize()
	if err != nil || len(data) < n {
		return "", errWrongKey
	}
	plain, err := k.aead.Open(nil, data[:n], data[n:], nil)
	if err != nil {
		return "", errWrongKey
	}
	return string(plain), nil
}
