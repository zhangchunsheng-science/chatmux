package wecom

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// WeCom callback crypto, per the official spec:
// key = Base64Decode(EncodingAESKey + "="), AES-256-CBC, iv = key[:16],
// plaintext = random(16) + msg_len(4, big endian) + msg + receiveid,
// PKCS#7 padded. Signature = SHA1 of sorted(token, timestamp, nonce, msg).

type cbCrypto struct {
	token      string
	aesKey     []byte
	receiveID  string // CorpID for app callbacks
}

func newCBCrypto(token, encodingAESKey, receiveID string) (*cbCrypto, error) {
	key, err := base64.StdEncoding.DecodeString(encodingAESKey + "=")
	if err != nil {
		return nil, fmt.Errorf("wecom: bad EncodingAESKey: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("wecom: EncodingAESKey must decode to 32 bytes, got %d", len(key))
	}
	return &cbCrypto{token: token, aesKey: key, receiveID: receiveID}, nil
}

func (c *cbCrypto) signature(timestamp, nonce, encrypted string) string {
	parts := []string{c.token, timestamp, nonce, encrypted}
	sort.Strings(parts)
	h := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(h[:])
}

func (c *cbCrypto) decrypt(encryptedB64 string) (plaintext string, err error) {
	data, err := base64.StdEncoding.DecodeString(encryptedB64)
	if err != nil {
		return "", err
	}
	if len(data) < aes.BlockSize || len(data)%aes.BlockSize != 0 {
		return "", fmt.Errorf("wecom: ciphertext not block-aligned")
	}
	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return "", err
	}
	pt := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, c.aesKey[:aes.BlockSize]).CryptBlocks(pt, data)
	pt = pkcs7Unpad(pt)
	if len(pt) < 20 {
		return "", fmt.Errorf("wecom: plaintext too short")
	}
	msgLen := int(binary.BigEndian.Uint32(pt[16:20]))
	if 20+msgLen > len(pt) {
		return "", fmt.Errorf("wecom: bad msg length")
	}
	msg := pt[20 : 20+msgLen]
	recv := string(pt[20+msgLen:])
	if c.receiveID != "" && recv != c.receiveID {
		return "", fmt.Errorf("wecom: receiveid mismatch (%s != %s)", recv, c.receiveID)
	}
	return string(msg), nil
}

func (c *cbCrypto) encrypt(plaintext string) (encryptedB64 string, err error) {
	random := make([]byte, 16)
	if _, err = randRead(random); err != nil {
		return "", err
	}
	msg := []byte(plaintext)
	buf := make([]byte, 0, len(random)+4+len(msg)+len(c.receiveID))
	buf = append(buf, random...)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(msg)))
	buf = append(buf, l[:]...)
	buf = append(buf, msg...)
	buf = append(buf, c.receiveID...)
	padded := pkcs7Pad(buf, aes.BlockSize)
	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return "", err
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, c.aesKey[:aes.BlockSize]).CryptBlocks(ct, padded)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	n := int(b[len(b)-1])
	if n == 0 || n > len(b) {
		return b
	}
	return b[:len(b)-n]
}
