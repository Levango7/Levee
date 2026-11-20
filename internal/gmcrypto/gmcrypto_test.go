package gmcrypto

import (
	"crypto/cipher"
	"encoding/hex"
	"testing"
)

func TestSM3KnownAnswer(t *testing.T) {
	// GM/T 0004-2012 test vector: SM3("abc")
	sum := SM3Sum([]byte("abc"))
	got := hex.EncodeToString(sum[:])
	const want = "66c7f0f462eeedd9d1f2d46bdc10e4e24167c4875cf2f7a2297da02b8f4ba8e0"
	if got != want {
		t.Fatalf("SM3(abc) = %s, want %s", got, want)
	}
}

func TestNewSM3MatchesSum(t *testing.T) {
	data := []byte("levee national crypto probe")
	h := NewSM3()
	if _, err := h.Write(data); err != nil {
		t.Fatal(err)
	}
	a := h.Sum(nil)
	sum := SM3Sum(data)
	b := sum[:]
	if hex.EncodeToString(a) != hex.EncodeToString(b) {
		t.Fatalf("hash.Hash and Sum disagree")
	}
}

func TestNewSM4Cipher(t *testing.T) {
	key := make([]byte, SM4KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	block, err := NewSM4Cipher(key)
	if err != nil {
		t.Fatal(err)
	}
	if block.BlockSize() != SM4BlockSize {
		t.Fatalf("block size = %d, want %d", block.BlockSize(), SM4BlockSize)
	}
	// SM4 can drive standard AEAD modes because it is a 128-bit block cipher.
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("SM4 cannot be used with GCM: %v", err)
	}
	if aead.NonceSize() != 12 {
		t.Fatalf("nonce size = %d, want 12", aead.NonceSize())
	}
}

func TestSelfTest(t *testing.T) {
	if err := SelfTest(); err != nil {
		t.Fatalf("self test: %v", err)
	}
}
