package audit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"sync"

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
// canonicalVersion is the version used for all newly computed hashes and
// checksums. Records produced before V2 was introduced are still accepted by
// the verification paths (see canonicalV1 and the legacy fallbacks in
// verifyChecksum / HashChainBuilder.Verify / ChainVerifier.Verify), which
// detect them by recomputing the V1 digest.
const canonicalVersion = 2

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
	keyOnce     sync.Once
	hmacKey     []byte
	hmacKeyRead bool
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

// digest returns the lower-case hex encoding of the keyed (or unkeyed)
// SHA-256 digest of payload. This is the single funnel through which every
// audit digest is computed, so keying and canonicalization stay consistent
// across the WORM checksum and the hash chain.
func digest(payload string) string {
	if key := auditKey(); len(key) > 0 {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(payload))
		return hex.EncodeToString(mac.Sum(nil))
	}
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
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
	return digest(canonicalV2(contentFields(trace)...))
}

// legacyChecksum recomputes the pre-V2 (pipe-separated) content checksum.
// It accepts nil-safe input like ComputeChecksum.
func legacyChecksum(trace *state.Trace) string {
	return digest(canonicalV1(contentFields(trace)...))
}

// ComputeHashV2 returns the canonical (V2) chained hash of a trace record
// bound to prevHash: the prevHash becomes the first length-prefixed field.
func ComputeHashV2(trace *state.Trace, prevHash string) string {
	fields := append([]string{prevHash}, contentFields(trace)...)
	return digest(canonicalV2(fields...))
}

// legacyHash recomputes the pre-V2 (pipe-separated) chained hash.
func legacyHash(trace *state.Trace, prevHash string) string {
	fields := append([]string{prevHash}, contentFields(trace)...)
	return digest(canonicalV1(fields...))
}
