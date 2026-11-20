// Package gmcrypto provides thin, audited wrappers around the ShangMi (SM)
// primitives used by LEVEE's opt-in national-crypto mode.
//
// Scope (2026-11-20):
//   - SM3 (GM/T 0004-2012): 32-byte hash, exposed as hash.Hash + Sum.
//   - SM4 (GM/T 0002-2012): 128-bit block cipher, exposed as cipher.Block so
//     callers can compose standard AEAD modes (GCM) or CBC+MAC themselves.
//
// This package deliberately does NOT provide:
//   - TLCP / GM/T 0024 (国密 TLS) — requires a full transport replacement.
//   - SM2 signatures — go-jose has no SM2 alg; needs protocol design.
//   - A "密评" claim — using SM3/SM4 primitives alone is not compliance.
//
// The upstream module is pure Go (CGO_ENABLED=0 safe), matching LEVEE's
// static build contract.
package gmcrypto

import (
	"crypto/cipher"
	"hash"

	"github.com/emmansun/gmsm/sm3"
	"github.com/emmansun/gmsm/sm4"
)

const (
	// SM3Size is the digest size in bytes.
	SM3Size = sm3.Size
	// SM4KeySize is the SM4 key size in bytes (128 bit).
	SM4KeySize = 16
	// SM4BlockSize is the SM4 block size in bytes.
	SM4BlockSize = sm4.BlockSize
)

// NewSM3 returns a fresh SM3 hash.
func NewSM3() hash.Hash { return sm3.New() }

// SM3Sum returns the SM3 digest of data.
func SM3Sum(data []byte) [SM3Size]byte { return sm3.Sum(data) }

// NewSM4Cipher returns an SM4 cipher.Block for the given 16-byte key.
func NewSM4Cipher(key []byte) (cipher.Block, error) { return sm4.NewCipher(key) }

// SelfTest runs the upstream known-answer tests for both primitives.
// It is intended for a startup/doctor probe, not per-request use.
func SelfTest() error {
	if err := sm3.SelfTest(); err != nil {
		return err
	}
	return sm4.SelfTest()
}
