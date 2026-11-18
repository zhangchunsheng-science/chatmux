package wecom

import (
	"encoding/base64"
	"testing"
)

// Round-trip: encrypt then decrypt returns the original plaintext.
func TestCryptoRoundTrip(t *testing.T) {
	// 43-char EncodingAESKey (decodes to 32 bytes with padding '=')
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	key = key[:43]
	c, err := newCBCrypto("token", key, "wwcorp123")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.encrypt(`{"hello":"世界"}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.decrypt(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"hello":"世界"}` {
		t.Fatalf("round trip mismatch: %q", got)
	}
}

func TestSignatureDeterministic(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))[:43]
	c, _ := newCBCrypto("tok", key, "ww")
	a := c.signature("1700000000", "n1", "cipher")
	b := c.signature("1700000000", "n1", "cipher")
	if a != b {
		t.Fatal("signature must be deterministic")
	}
	if c.signature("1700000000", "n2", "cipher") == a {
		t.Fatal("different nonce must change signature")
	}
}

func TestDecryptRejectsGarbage(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))[:43]
	c, _ := newCBCrypto("tok", key, "ww")
	if _, err := c.decrypt("!!!notbase64!!!"); err == nil {
		t.Fatal("garbage must fail")
	}
}
