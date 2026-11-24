package audit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/nexus/levee/internal/gmcrypto"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// Canonical encoding versions for audit hashes and WORM checksums.
//
// V1 (legacy): fields joined with the "|" separator. The encoding is
// ambiguous because a field value that itself contains "|" produces the same
// byte stream as a different field split, allowing collision-style confusion
// between records.
//
// V2 (current): every field is encoded as "<decimal-length>:<field>" and the
// encodings are concatenated without any separator. The length prefix makes
// the field boundaries unambiguous regardless of field content.
//
// All newly computed hashes and checksums use V2. Records produced before V2
// was introduced are still accepted by the verification paths (see
// canonicalV1 and the legacy fallbacks in verifyChecksum /
// HashChainBuilder.Verify / ChainVerifier.Verify), which detect them by
// recomputing the V1 digest.

// legacySeparator is the V1 field delimiter ("|"), kept private so it can
// never be reintroduced into new digests.
const legacySeparator = "|"

// minHMACKeyLen is the minimum accepted length (in bytes) of the raw
// passphrase supplied via LEVEE_AUDIT_HMAC_KEY.
const minHMACKeyLen = 16

// EnvHMACKey names the environment variable that supplies the optional raw
// HMAC passphrase. When set (and at least minHMACKeyLen bytes long), all
// audit digests become HMAC-SHA256(key, payload) instead of bare SHA-256;
// when unset, plain SHA-256 is used and a one-time warning is logged on
// first use.
const EnvHMACKey = "LEVEE_AUDIT_HMAC_KEY"

var (
	keyOnce sync.Once
	hmacKey []byte
)

// keyEpochPrefixLen is the number of hex chars of the key fingerprint embedded
// in keyed digest values ("k" + 8 hex + ":"). Short enough to keep digests
// compact, long enough that an accidental key collision is negligible.
const keyFingerprintHexLen = 8

// keyEpochID returns the short fingerprint of the active HMAC key, or "" when
// unkeyed. It is derived from the key bytes themselves so different keys get
// different epochs without any operator-managed epoch number.
func keyEpochID() string {
	auditKey() // ensure resolution
	if len(hmacKey) == 0 {
		return ""
	}
	s := sha256.Sum256(hmacKey)
	return hex.EncodeToString(s[:])[:keyFingerprintHexLen]
}

// auditKey lazily resolves the HMAC key from the environment exactly once.
// A nil result means "unkeyed" (plain SHA-256). A key shorter than
// minHMACKeyLen is warned about and ignored.
func auditKey() []byte {
	keyOnce.Do(func() {
		raw := os.Getenv(EnvHMACKey)
		if raw == "" {
			log.Warn("audit: " + EnvHMACKey + " not set; audit checksums use unkeyed SHA-256 (set a >=16-byte passphrase for tamper-proof keyed digests)")
			return
		}
		if len(raw) < minHMACKeyLen {
			log.Warn("audit: " + EnvHMACKey + " shorter than " + strconv.Itoa(minHMACKeyLen) + " bytes; ignoring weak key and using unkeyed SHA-256")
			return
		}
		hmacKey = []byte(raw)
	})
	return hmacKey
}

// Keyed reports whether a valid HMAC key is configured. Exposed mainly so
// callers (and tests via a fresh process environment) can introspect the
// effective digest mode.
func Keyed() bool { return len(auditKey()) > 0 }

// KeyEpochID returns the short fingerprint of the active HMAC key (the epoch
// tag embedded in keyed digests), or "" when unkeyed. Exposed so the REST
// chain-mode endpoint can report which key epoch new rows are written under.
func KeyEpochID() string { return keyEpochID() }

// ResetKeyForTest re-arms the lazy HMAC key resolution so a test can
// exercise both digest modes inside one process.
//
// It is exported because the key is resolved exactly once per process (the
// first digest decides), and the gateway's GET /audit/chain-mode endpoint has
// to be tested in BOTH modes — that test lives in another package, so a
// package-private hook would not reach it.
//
// Production code must never call it: flipping the key mid-process would make
// digests computed before the flip inconsistent with digests computed after
// it, and an audit chain whose hashes change meaning halfway is worse than no
// chain at all.
func ResetKeyForTest() {
	keyOnce = sync.Once{}
	hmacKey = nil
}

// Digest algorithm modes. The mode is encoded as a PREFIX on the hex digest,
// never as a hashed canonical field, so historical (unprefixed) rows keep their
// original SHA-256 meaning and remain verifiable without any migration.
//
//	unprefixed 64-hex  -> SHA-256 / HMAC-SHA256 (historical + default)
//	"sm3:" + 64-hex    -> SM3 / HMAC-SM3 (opt-in; GM/T 0004-2012)
//
// The prefix identifies the HASH FAMILY only. The V1/V2 canonical-encoding
// fallback is an orthogonal dimension handled by legacyHash/legacyChecksum.
const (
	digestModeSHA256 = "sha256"
	digestModeSM3    = "sm3"

	prefixSM3     = "sm3:"
	prefixHMACSM3 = "hmac-sm3:"

	// keyEpochTag is "k" + keyFingerprintHexLen hex chars + ":". It is embedded
	// in keyed digests to record WHICH key produced them, so verification can
	// tell "operator changed the key" apart from "someone rewrote the chain".
	keyEpochTagPrefix = "k"
	keyEpochTagLen    = len(keyEpochTagPrefix) + keyFingerprintHexLen + 1 // "k" + 8 hex + ":"
)

// keyEpochTag returns the full epoch tag for the active key (e.g. "k1a2b3c4d:"),
// or "" when unkeyed.
func keyEpochTag() string {
	id := keyEpochID()
	if id == "" {
		return ""
	}
	return keyEpochTagPrefix + id + ":"
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CurrentDigestMode is the process-wide configured digest mode. It is resolved
// lazily from the environment (LEVEE_AUDIT_DIGEST_MODE) so it can be read by the
// REST chain-mode endpoint and by writers; it never affects verification of an
// existing row (which uses the row's own stored prefix — see digestForHash).
const EnvDigestMode = "LEVEE_AUDIT_DIGEST_MODE"

var (
	digestModeOnce sync.Once
	currentMode    string
)

func configuredDigestMode() string {
	digestModeOnce.Do(func() {
		raw := os.Getenv(EnvDigestMode)
		switch raw {
		case "", digestModeSHA256:
			currentMode = digestModeSHA256
		case digestModeSM3:
			currentMode = digestModeSM3
		default:
			// Unknown mode is a hard misconfiguration: fall back to the safe
			// default but emit a loud warning. Validation at config load also
			// rejects it for YAML-driven deployments.
			log.Warn("audit: unknown " + EnvDigestMode + " value " + strconv.Quote(raw) + "; using sha256")
			currentMode = digestModeSHA256
		}
	})
	return currentMode
}

// CurrentDigestMode reports the configured hash family ("sha256"|"sm3").
func CurrentDigestMode() string { return configuredDigestMode() }

// SetDigestMode sets the process-wide hash family from configuration. Like the
// HMAC key it resolves exactly once and must be called at startup BEFORE any
// digest is produced; later calls are no-ops. Passing "" or an unknown value
// selects the safe sha256 default (config validation rejects unknown values).
func SetDigestMode(mode string) {
	digestModeOnce.Do(func() {
		switch mode {
		case digestModeSM3:
			currentMode = digestModeSM3
		default:
			currentMode = digestModeSHA256
		}
	})
}

// ResetDigestModeForTest re-arms lazy mode resolution. Production must not call.
func ResetDigestModeForTest() {
	digestModeOnce = sync.Once{}
	currentMode = ""
}

// digest returns the lower-case hex encoding of the keyed (or unkeyed) SHA-256
// digest of payload. This is the single funnel through which every audit digest
// is computed, so keying and canonicalization stay consistent across the WORM
// checksum and the hash chain. It always uses the SHA-256 family (no prefix),
// so historical V1/V2 fallbacks and every existing caller keep their behavior.
func digest(payload string) string { return digestWithMode(payload, digestModeSHA256) }

// digestWithMode computes a digest with an explicit hash family. For sha256 it
// returns the bare 64-hex string (identical to digest, no prefix). For sm3 it
// prefixes the hex so the family is self-describing and verifiable per-row.
// The HMAC key (if configured) is applied uniformly across both families.
func digestWithMode(payload, mode string) string {
	key := auditKey()
	switch mode {
	case digestModeSM3:
		var sum []byte
		if len(key) > 0 {
			mac := hmac.New(gmcrypto.NewSM3, key)
			mac.Write([]byte(payload))
			sum = mac.Sum(nil)
			return prefixHMACSM3 + keyEpochTag() + hex.EncodeToString(sum)
		}
		s := gmcrypto.SM3Sum([]byte(payload))
		sum = s[:]
		return prefixSM3 + hex.EncodeToString(sum)
	case digestModeSHA256, "":
		if len(key) > 0 {
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(payload))
			return keyEpochTag() + hex.EncodeToString(mac.Sum(nil))
		}
		s := sha256.Sum256([]byte(payload))
		return hex.EncodeToString(s[:])
	default:
		// Unknown mode is a configuration error, not data. configuredDigestMode()
		// already warns + falls back to sha256 for the process mode; be consistent
		// here instead of panicking (CONTRIBUTING.md §2.3 forbids panic).
		log.Warn("audit: unknown digest mode " + strconv.Quote(mode) + "; falling back to sha256")
		if len(key) > 0 {
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(payload))
			return keyEpochTag() + hex.EncodeToString(mac.Sum(nil))
		}
		s := sha256.Sum256([]byte(payload))
		return hex.EncodeToString(s[:])
	}
}

// legacyKeyedDigest recomputes the PRE-EPOCH keyed SHA-256 digest: bare hex,
// no "k<fp>:" prefix. Rows written before the key-epoch work stored exactly
// this value, so verification of historical keyed chains must use this shape —
// using digestWithMode (which now prefixes) would report a false tamper.
func legacyKeyedDigest(payload string) string {
	key := auditKey()
	if len(key) == 0 {
		return "" // unkeyed historical rows are handled by digestWithMode(sha256)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// digestForCurrent computes a digest with the process-configured mode. Writers
// use this so newly produced rows can opt into SM3 while historical rows stay
// untouched.
func digestForCurrent(payload string) string {
	return digestWithMode(payload, configuredDigestMode())
}

// traceCanonical returns the canonical V2 payload used for a trace's chained
// hash: prevHash followed by the record's content fields. It is the exact byte
// string that ComputeHashV2 hashes; exposing it lets verification recompute a
// row under the algorithm family declared by its own stored hash.
func traceCanonical(trace *state.Trace, prevHash string) string {
	fields := append([]string{prevHash}, contentFields(trace)...)
	return canonicalV2(fields...)
}

// auditCanonical returns the canonical V2 payload for an audit row's hash.
func auditCanonical(a *state.Audit, prevHash string) string {
	fields := append([]string{prevHash}, auditContentFields(a)...)
	return canonicalV2(fields...)
}

// verifyTraceHash recomputes a trace's chained hash under the algorithm family
// declared by its own stored hash prefix (so a mixed chain verifies each row
// independently), then falls back to the legacy V1 encoding (always SHA-256).
// Returns whether it matched, whether the legacy encoding is what matched (for
// the LegacyCount visibility metric), and the family-correct expected value.
func verifyTraceHash(t *state.Trace, prevHash string) (match, legacy bool, expected string, err error) {
	expected, err = recomputeCanonical(traceCanonical(t, prevHash), t.CurrHash)
	if err != nil {
		return false, false, "", err
	}
	if expected == t.CurrHash {
		return true, false, expected, nil
	}
	if t.CurrHash == legacyHash(t, prevHash) {
		return true, true, expected, nil
	}
	return false, false, expected, nil
}

// verifyAuditHash is the audit-chain counterpart. There is no V1 fallback for
// the global audit chain (it was introduced with v7), so only the family-aware
// recomputation is attempted.
func verifyAuditHash(a *state.Audit, prevHash string) (match bool, expected string, err error) {
	expected, err = recomputeCanonical(auditCanonical(a, prevHash), a.CurrHash)
	if err != nil {
		return false, "", err
	}
	return expected == a.CurrHash, expected, nil
}

// verifyWORMChecksum verifies a WORM content checksum under the family declared
// by the stored hash, with the legacy V1 checksum (SHA-256) as fallback.
func verifyWORMChecksum(trace *state.Trace) bool {
	if trace == nil {
		return false
	}
	expected, err := recomputeCanonical(canonicalV2(contentFields(trace)...), trace.CurrHash)
	if err == nil && expected == trace.CurrHash {
		return true
	}
	return trace.CurrHash == legacyChecksum(trace)
}

// keyEpochFrom returns the epoch id from a digest body beginning with a
// "k<8hex>:" tag, or "" when there is no tag. A historical keyed body is bare
// hex: epoch "" means "un-tagged", and the caller keeps the legacy behavior
// (recompute with the current key — the un-tagged ratchet).
func keyEpochFrom(body string) string {
	if len(body) >= keyEpochTagLen && strings.HasPrefix(body, keyEpochTagPrefix) && body[keyEpochTagLen-1] == ':' {
		id := body[len(keyEpochTagPrefix) : keyEpochTagLen-1]
		if len(id) == keyFingerprintHexLen && isLowerHex(id) {
			return id
		}
	}
	return ""
}

// checkEpoch verifies that a stored key-epoch tag matches the active key. An
// empty stored epoch means an un-tagged historical keyed digest — those cannot
// be epoch-checked and keep the legacy recompute behavior.
func checkEpoch(storedEpoch string) error {
	if storedEpoch == "" {
		return nil // pre-epoch historical keyed digest
	}
	current := keyEpochID()
	if current == "" {
		return fmt.Errorf("%w: stored key epoch %q but no HMAC key configured", ErrKeyEpochMismatch, storedEpoch)
	}
	if storedEpoch != current {
		return fmt.Errorf("%w: stored %q current %q", ErrKeyEpochMismatch, storedEpoch, current)
	}
	return nil
}

// recomputeCanonical recomputes the digest of an already-canonicalized payload
// using the family AND key epoch declared by the stored hash. The returned
// string (with its prefixes) can be compared directly to the stored value.
// ErrKeyEpochMismatch is returned when the stored epoch tag does not match the
// active key — the verifier maps this to a distinct failure type so "key
// rotated" is never reported as "chain tampered". Unknown prefixes fail closed.
func recomputeCanonical(payload, stored string) (string, error) {
	switch {
	case strings.HasPrefix(stored, prefixHMACSM3):
		body := strings.TrimPrefix(stored, prefixHMACSM3)
		epoch := keyEpochFrom(body)
		if err := checkEpoch(epoch); err != nil {
			return "", err
		}
		key := auditKey()
		if len(key) == 0 {
			return "", fmt.Errorf("audit: stored hash is HMAC-SM3 but %s is not set", EnvHMACKey)
		}
		mac := hmac.New(gmcrypto.NewSM3, key)
		mac.Write([]byte(payload))
		tag := ""
		if epoch != "" {
			tag = keyEpochTagPrefix + epoch + ":"
		}
		return prefixHMACSM3 + tag + hex.EncodeToString(mac.Sum(nil)), nil
	case strings.HasPrefix(stored, prefixSM3):
		// Unkeyed SM3: family is self-describing; a key-epoch tag on an unkeyed
		// SM3 value would be malformed and is rejected below via the colon check.
		if strings.Contains(strings.TrimPrefix(stored, prefixSM3), ":") {
			return "", fmt.Errorf("audit: unexpected key epoch on unkeyed SM3 hash %q", stored)
		}
		s := gmcrypto.SM3Sum([]byte(payload))
		return prefixSM3 + hex.EncodeToString(s[:]), nil
	default:
		// Possibly a keyed SHA-256 WITH an epoch tag ("k<8hex>:<hex>").
		if len(stored) >= keyEpochTagLen && strings.HasPrefix(stored, keyEpochTagPrefix) && stored[keyEpochTagLen-1] == ':' {
			epoch := keyEpochFrom(stored)
			if err := checkEpoch(epoch); err != nil {
				return "", err
			}
			key := auditKey()
			if len(key) == 0 {
				return "", fmt.Errorf("audit: stored hash is keyed but %s is not set", EnvHMACKey)
			}
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(payload))
			return keyEpochTagPrefix + epoch + ":" + hex.EncodeToString(mac.Sum(nil)), nil
		}
		// A colon elsewhere means an unrecognised family prefix: fail closed.
		if strings.Contains(stored, ":") {
			return "", fmt.Errorf("audit: unknown digest prefix in %q", stored)
		}
		// Bare hex: historical row written BEFORE key epochs. It may be either
		// unkeyed SHA-256 or keyed HMAC-SHA256 (no tag to tell them apart). Try
		// BOTH under the bare (no-prefix) shape:
		//   1. unkeyed  -> digestWithMode(sha256) with no key
		//   2. keyed    -> legacyKeyedDigest (bare HMAC with the current key)
		// The caller compares the returned string to the stored value. We
		// return the keyed form when a key is configured (preserving the
		// documented unkeyed->keyed ratchet), but ALSO accept the bare form via
		// the legacy fallback below so a SAME-KEY pre-epoch keyed chain keeps
		// verifying instead of being misread as tampering (D-8 regression fix).
		if legacy := legacyKeyedDigest(payload); legacy != "" && legacy == stored {
			return legacy, nil
		}
		return digestWithModeNoEpoch(payload), nil
	}
}

// digestWithModeNoEpoch is digestWithMode(sha256) WITHOUT the epoch prefix —
// the bare historical shape for an unkeyed row.
func digestWithModeNoEpoch(payload string) string {
	if key := auditKey(); len(key) > 0 {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(payload))
		return hex.EncodeToString(mac.Sum(nil))
	}
	s := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(s[:])
}

// canonicalV2 encodes fields unambiguously as length-prefixed segments:
// "<len(field)>:<field>" concatenated without separator.
func canonicalV2(fields ...string) string {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteByte(':')
		b.WriteString(f)
	}
	return b.String()
}

// canonicalV1 is the legacy pipe-delimited encoding kept private so
// verification can still recognise pre-V2 records. It must never be used to
// produce new digests outside the legacy fallback checks.
func canonicalV1(fields ...string) string {
	return strings.Join(fields, legacySeparator)
}

// contentFields returns the trace content field set shared by the WORM
// content checksum and the chained hash: ID, RunID, Event, Actor, Detail and
// the timestamp as UnixNano. A nil trace yields the empty record's fields.
func contentFields(t *state.Trace) []string {
	if t == nil {
		t = &state.Trace{}
	}
	return []string{
		t.ID,
		t.RunID,
		t.Event,
		t.Actor,
		t.Detail,
		strconv.FormatInt(t.Timestamp.UnixNano(), 10),
	}
}

// ComputeChecksum returns the canonical (V2) WORM content checksum of a
// trace record: digest over the length-prefixed concatenation of
// ID, RunID, Event, Actor, Detail and Timestamp.UnixNano(). It is exported
// so tooling such as the archiver can stamp missing checksums onto
// pre-existing traces without duplicating the algorithm.
//
// When an HMAC key is configured (EnvHMACKey), the digest is keyed; the
// result remains deterministic for identical inputs within one deployment.
func ComputeChecksum(trace *state.Trace) string {
	return digestForCurrent(canonicalV2(contentFields(trace)...))
}

// legacyChecksum recomputes the pre-V2 (pipe-separated) content checksum.
// It accepts nil-safe input like ComputeChecksum.
func legacyChecksum(trace *state.Trace) string {
	// V1 encoding is historical and always SHA-256 — never follow the current
	// mode, or pre-V2 rows would be mis-verified as tampered after an SM3 switch.
	return digest(canonicalV1(contentFields(trace)...))
}

// ComputeHashV2 returns the canonical (V2) chained hash of a trace record
// bound to prevHash: the prevHash becomes the first length-prefixed field.
func ComputeHashV2(trace *state.Trace, prevHash string) string {
	fields := append([]string{prevHash}, contentFields(trace)...)
	return digestForCurrent(canonicalV2(fields...))
}

// legacyHash recomputes the pre-V2 (pipe-separated) chained hash.
func legacyHash(trace *state.Trace, prevHash string) string {
	// Historical V1 encoding, always SHA-256 (see legacyChecksum).
	fields := append([]string{prevHash}, contentFields(trace)...)
	return digest(canonicalV1(fields...))
}
