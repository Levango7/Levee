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
//	unprefixed 64-hex       -> unkeyed SHA-256 (historical; written before a key)
//	"hmac-sha256:" + 64-hex -> HMAC-SHA256 under LEVEE_AUDIT_HMAC_KEY
//	"sm3:" + 64-hex         -> unkeyed SM3 (opt-in; GM/T 0004-2012)
//	"hmac-sm3:" + 64-hex    -> HMAC-SM3 under LEVEE_AUDIT_HMAC_KEY
//
// The prefix identifies BOTH the hash family and the keying era, which is what
// lets verification answer "was this row written before or after keying?
// without any migration or backfill. The V1/V2 canonical-encoding fallback is
// an orthogonal dimension handled by legacyHash/legacyChecksum.
const (
	digestModeSHA256 = "sha256"
	digestModeSM3    = "sm3"

	prefixHMACSHA256 = "hmac-sha256:"
	prefixSM3        = "sm3:"
	prefixHMACSM3    = "hmac-sm3:"
)

// keyEra classifies a stored digest by whether it carries the key's guarantee.
// It reads the prefix only: the value is self-describing, so classification
// never depends on the current configuration. A row with no prefix was written
// before keying.
type keyEraValue string

const (
	eraKeyed   keyEraValue = "keyed"
	eraUnkeyed keyEraValue = "unkeyed"
	eraUnknown keyEraValue = "unknown"
)

// keyEra reports the keying era declared by a stored digest value. An empty
// hash (a chain that was never built) reports eraUnknown so callers do not
// count it as either era. An unrecognised prefix also reports eraUnknown:
// recomputeCanonical fails closed on it, and counting it as unkeyed would
// understate the risk.
func keyEra(stored string) keyEraValue {
	switch {
	case stored == "":
		return eraUnknown
	case strings.HasPrefix(stored, prefixHMACSHA256),
		strings.HasPrefix(stored, prefixHMACSM3):
		return eraKeyed
	case strings.HasPrefix(stored, prefixSM3):
		return eraUnkeyed
	case strings.Contains(stored, ":"):
		return eraUnknown
	default:
		return eraUnkeyed
	}
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

// digest returns the lower-case hex encoding of the UNKEYED SHA-256 digest of
// payload. This is the single funnel through which every audit digest is
// computed, so keying and canonicalization stay consistent across the WORM
// checksum and the hash chain.
//
// It always uses the SHA-256 family and always runs UNKEYED, so historical
// V1/V2 fallbacks keep their original meaning regardless of whether a key is
// configured now. Callers that must follow the current configuration use
// digestForCurrent instead; callers that need the keyed form use
// digestWithMode directly.
func digest(payload string) string { return unkeyedSHA256(payload) }

// unkeyedSHA256 is the unkeyed SHA-256 of payload as bare lower-case hex. It is
// the definition of the historical (pre-keying) era, so it must never consult
// the configured key.
func unkeyedSHA256(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

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
			return prefixHMACSM3 + hex.EncodeToString(sum)
		}
		s := gmcrypto.SM3Sum([]byte(payload))
		sum = s[:]
		return prefixSM3 + hex.EncodeToString(sum)
	case digestModeSHA256, "":
		if len(key) > 0 {
			// Keyed SHA-256 carries its era as a prefix. Without it a keyed row
			// and a historical unkeyed row are both bare 64-hex, and the verifier
			// cannot tell "predates the key" from "was rewritten" — which made
			// adding a key to a deployment with history fail every historical row.
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(payload))
			return prefixHMACSHA256 + hex.EncodeToString(mac.Sum(nil))
		}
		return unkeyedSHA256(payload)
	default:
		// Unknown mode is a programming error, not data: there is no safe
		// silent fallback. Panic so it fails loudly in tests and never writes
		// a digest whose family cannot be verified later.
		panic(fmt.Sprintf("audit: unknown digest mode %q", mode))
	}
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

// recomputeCanonical recomputes the digest of an already-canonicalized payload
// using the family declared by the stored hash's prefix. It returns the
// recomputed string (including the same prefix) so callers can compare it to
// the stored value directly. Unknown prefixes fail closed.
func recomputeCanonical(payload, stored string) (string, error) {
	switch {
	case strings.HasPrefix(stored, prefixHMACSHA256):
		// Keyed SHA-256: a key MUST be configured to verify; if it was removed,
		// the chain cannot be recomputed and must not silently fall back to the
		// unkeyed algorithm (that would make every keyed row verifiable by
		// anyone who can strip the prefix).
		key := auditKey()
		if len(key) == 0 {
			return "", fmt.Errorf("audit: stored hash is HMAC-SHA256 but %s is not set", EnvHMACKey)
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(payload))
		return prefixHMACSHA256 + hex.EncodeToString(mac.Sum(nil)), nil
	case strings.HasPrefix(stored, prefixHMACSM3):
		// Keyed SM3: a key MUST be configured to verify; if it was removed,
		// the chain cannot be recomputed (same property as keyed SHA-256).
		key := auditKey()
		if len(key) == 0 {
			return "", fmt.Errorf("audit: stored hash is HMAC-SM3 but %s is not set", EnvHMACKey)
		}
		mac := hmac.New(gmcrypto.NewSM3, key)
		mac.Write([]byte(payload))
		return prefixHMACSM3 + hex.EncodeToString(mac.Sum(nil)), nil
	case strings.HasPrefix(stored, prefixSM3):
		// Unkeyed SM3: must NOT silently switch to keyed just because a key is
		// now configured — that would be the D-8 failure mode at the algorithm
		// level. Recompute with unkeyed SM3 regardless of current key state.
		s := gmcrypto.SM3Sum([]byte(payload))
		return prefixSM3 + hex.EncodeToString(s[:]), nil
	default:
		// A colon means the value declares a family prefix; if it is not one
		// we recognise, fail closed rather than guess (a corrupted/tampered
		// hash must not be silently treated as historical SHA-256).
		if strings.Contains(stored, ":") {
			return "", fmt.Errorf("audit: unknown digest prefix in %q", stored)
		}
		// No prefix: a row written before keying. Recompute it as UNKEYED
		// SHA-256 regardless of the current key, because that is what it was
		// hashed with — following the current key here is exactly the D-8
		// failure mode (adding a key would fail every historical row).
		//
		// The consequence is deliberate and must stay documented: an attacker
		// who can write rows AND knows the payload could forge an unprefixed
		// row. That is precisely the situation the key exists to prevent, which
		// is why the unkeyed era is REPORTED by VerifyTraces rather than
		// silently absorbed — see VerifyResult.UnkeyedEraCount.
		return unkeyedSHA256(payload), nil
	}
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
