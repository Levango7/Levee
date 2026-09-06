package audit

// Redaction vocabulary & walker tests (SA-009 / SA-010): boundary-suffix
// matching, the extended word table, config-provided extras, struct/map/slice
// traversal rules (visibility, known leaves, depth cap, cycle guard) and the
// "only convert on hit" shape guarantee.

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- matching rules ----------------------------------------------------------

func TestIsSensitive_ExactAndBoundary(t *testing.T) {
	hits := []string{
		"password", "PASSWORD", "db_password", "db-password", "user_passwd",
		"token", "access_token", "refresh-token", "auth_code", "ssh_key",
		"cert", "tls_cert", "certificate", "connection_string",
		"private_key", "api_key", "x_api_key", "secret", "my_secret",
		"credential", "passphrase",
	}
	for _, k := range hits {
		assert.True(t, isSensitive(k), "%q should be sensitive", k)
	}

	misses := []string{
		"sort_key", "monkey", "keyring", "keys", // bare "key" is exact-only
		"passwordstore", "keymaster", // suffix without a word boundary
		"host", "username", "ca", "namespace",
	}
	for _, k := range misses {
		assert.False(t, isSensitive(k), "%q should NOT be sensitive", k)
	}
}

func TestSetSensitiveFields_ExtrasMergeAndClear(t *testing.T) {
	defer SetSensitiveFields(nil) // restore the global registry

	SetSensitiveFields([]string{"  LicensePlate ", ""})
	assert.True(t, isSensitive("licenseplate"), "extra names match case-insensitively")
	assert.True(t, isSensitive("user_licenseplate"), "extras inherit the boundary-suffix rule")
	assert.False(t, isSensitive("host"))

	// The bare-"key" exact-only exception applies to user extras too.
	SetSensitiveFields([]string{"key"})
	assert.False(t, isSensitive("sort_key"), "a user-added bare key must stay exact-only")
	assert.False(t, isSensitive("licenseplate"), "Set replaces the previous extras snapshot")

	SetSensitiveFields(nil)
	assert.False(t, isSensitive("key2"))
}

func TestRedact_ExtendedVocabulary(t *testing.T) {
	in := map[string]any{
		"passphrase":        "p",
		"auth_code":         "a",
		"refresh_token":     "r",
		"access_token":      "ac",
		"ssh_key":           "s",
		"cert":              "c",
		"certificate":       "ce",
		"connection_string": "cs",
		"db_password":       "dp",
		"access-token":      "at",
	}
	out := Redact(in)
	for k := range in {
		assert.Equal(t, redactedValue, out[k], "field %q should be redacted", k)
	}
}

// --- struct traversal ---------------------------------------------------------

type dbConfig struct {
	Host     string `json:"host"`
	Password string `json:"password"`
	SortKey  int    `json:"sort_key"`
}

type tlsConfig struct {
	CertPath  string `json:"cert_path"`
	Token     string `json:"token"`
	privateIP string //nolint:unused // unexported: invisible to the walker
}

type baseEmbedded struct {
	Region   string `json:"region"`
	APIToken string `json:"api_token"`
}

// BaseEmbedded is the exported twin: encoding/json promotes fields of
// unexported embedded structs, but the walker's CanInterface visibility bar
// (the design's "json.Marshal visible" approximation) cannot READ those
// fields without unsafe — so the walker skips them. Tests use the exported
// form; the divergence is documented on jsonFieldInfo.
type BaseEmbedded struct {
	Region   string `json:"region"`
	APIToken string `json:"api_token"`
}

type embeddedConfig struct {
	BaseEmbedded
	Name  string `json:"name"`
	Token string `json:"token"`
}

type taggedEmbeddedConfig struct {
	BaseEmbedded `json:"meta"`
}

type dashConfig struct {
	Visible string `json:"visible"`
	Hidden  string `json:"-"`
	Token   string `json:"token"` // hit: forces the struct→map conversion
}

type omitConfig struct {
	Name  string `json:"name"`
	Token string `json:"token,omitempty"`
}

func TestRedact_StructHitConvertsToMap(t *testing.T) {
	in := map[string]any{"conn": dbConfig{Host: "h1", Password: "s3cr3t", SortKey: 7}}
	out := Redact(in)

	m, ok := out["conn"].(map[string]any)
	require.True(t, ok, "struct with a hit must become a map")
	assert.Equal(t, "h1", m["host"])
	assert.Equal(t, redactedValue, m["password"])
	assert.Equal(t, 7, m["sort_key"], "sort_key must NOT be redacted (bare-key exact rule)")
}

func TestRedact_StructWithoutHitsKeepsOriginalValue(t *testing.T) {
	type plainConfig struct {
		Host    string `json:"host"`
		SortKey int    `json:"sort_key"`
	}
	in := map[string]any{"conn": plainConfig{Host: "h1", SortKey: 1}}
	out := Redact(in)
	// No sensitive NAME in the subtree → the value passes through untouched
	// (no struct→map drift for secret-free payloads).
	assert.IsType(t, plainConfig{}, out["conn"])

	// A sensitive name present but empty still counts as a hit: the field
	// WOULD marshal ("password":""), so the copy must redact it.
	out2 := Redact(map[string]any{"conn": dbConfig{Host: "h1"}})
	m, ok := out2["conn"].(map[string]any)
	require.True(t, ok, "an empty-but-present password field is still a hit")
	assert.Equal(t, redactedValue, m["password"])
}

func TestRedact_UnexportedAndDashSkipped(t *testing.T) {
	out := Redact(map[string]any{
		"tls":  tlsConfig{CertPath: "c.pem", Token: "t", privateIP: "10.0.0.1"},
		"dash": dashConfig{Visible: "v", Hidden: "shh", Token: "t"},
	})

	tls, ok := out["tls"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "c.pem", tls["cert_path"], "cert_path must NOT hit (ends with _path)")
	assert.Equal(t, redactedValue, tls["token"])
	_, hasPrivate := tls["privateIP"]
	assert.False(t, hasPrivate, "unexported fields must be skipped")

	dash, ok := out["dash"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "v", dash["visible"])
	_, hasHidden := dash["Hidden"]
	_, hasHiddenField := dash["shh"]
	assert.False(t, hasHidden || hasHiddenField, "json:\"-\" fields must be skipped")
}

func TestRedact_EmbeddedPromotion(t *testing.T) {
	in := map[string]any{"cfg": embeddedConfig{
		BaseEmbedded: BaseEmbedded{Region: "eu", APIToken: "tok-1"},
		Name:         "svc",
	}}
	out := Redact(in)
	m, ok := out["cfg"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "eu", m["region"], "embedded fields must be promoted like json.Marshal does")
	assert.Equal(t, redactedValue, m["api_token"])
	assert.Equal(t, "svc", m["name"])

	tagged := Redact(map[string]any{"cfg": taggedEmbeddedConfig{
		BaseEmbedded: BaseEmbedded{Region: "us", APIToken: "tok-2"},
	}})
	tm, ok := tagged["cfg"].(map[string]any)
	require.True(t, ok)
	meta, ok := tm["meta"].(map[string]any)
	require.True(t, ok, "tagged embedded struct must be keyed, not promoted")
	assert.Equal(t, redactedValue, meta["api_token"])
	assert.Equal(t, "us", meta["region"])
}

func TestRedact_EmbeddedUnchangedMergedWhenParentRebuilt(t *testing.T) {
	// The embedded subtree has NO hit but a sibling (token) does: the promoted
	// keys must still appear (flattened from the original), matching
	// json.Marshal.
	out := Redact(map[string]any{"cfg": embeddedConfig{
		BaseEmbedded: BaseEmbedded{Region: "eu"},
		Name:         "svc",
		Token:        "boom",
	}})
	m, ok := out["cfg"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "eu", m["region"])
	assert.Equal(t, "svc", m["name"])
	assert.Equal(t, redactedValue, m["token"])
}

// innerSecret is an unexported embedded struct type: json.Marshal promotes
// its exported fields even though reflect cannot read them. The walker must
// not let the promoted token leak.
type innerSecret struct {
	Token string `json:"token"`
}

type unexportedHolder struct {
	innerSecret
	Name string `json:"name"`
}

type innerSafe struct {
	Region string `json:"region"`
}

type unexportedSafeHolder struct {
	innerSafe
	Name string `json:"name"`
}

func TestRedact_UnexportedEmbeddedSensitiveSubtreeOmitted(t *testing.T) {
	out := Redact(map[string]any{"h": unexportedHolder{
		innerSecret: innerSecret{Token: "leak-me"},
		Name:        "visible",
	}})
	m, ok := out["h"].(map[string]any)
	require.True(t, ok, "sensitive promoted name must force the parent rebuild")
	assert.Equal(t, "visible", m["name"])
	_, hasToken := m["token"]
	assert.False(t, hasToken, "unreadable subtree with a sensitive name must be omitted, never leaked")

	// Marshalling the result must not contain the secret either way.
	buf, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, string(buf), "leak-me")

	// Secret-free unexported embedded subtree: untouched pass-through.
	out2 := Redact(map[string]any{"h": unexportedSafeHolder{innerSafe: innerSafe{Region: "eu"}, Name: "n"}})
	assert.IsType(t, unexportedSafeHolder{}, out2["h"])
}

func TestRedact_OmitEmptyFollowsMarshal(t *testing.T) { // Zero-value omitempty field: json.Marshal omits it, so nothing exists to
	// redact — and its absence must not trigger conversion.
	out := Redact(map[string]any{"cfg": omitConfig{Name: "n"}})
	assert.IsType(t, omitConfig{}, out["cfg"])

	// Non-empty omitempty field with a sensitive name: redacted, converted.
	out = Redact(map[string]any{"cfg": omitConfig{Name: "n", Token: "t"}})
	m, ok := out["cfg"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, redactedValue, m["token"])
}

func TestRedact_NestedPointerAndSliceOfStructs(t *testing.T) {
	type step struct {
		Name     string  `json:"name"`
		Secret   string  `json:"secret"`
		Children []*step `json:"children,omitempty"`
	}
	in := map[string]any{"steps": []step{
		{Name: "a", Secret: "s1"},
		{Name: "b", Children: []*step{{Name: "b1", Secret: "s2"}}},
	}}
	out := Redact(in)
	list, ok := out["steps"].([]any)
	require.True(t, ok)
	first, ok := list[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, redactedValue, first["secret"])
	second, ok := list[1].(map[string]any)
	require.True(t, ok)
	kids, ok := second["children"].([]any)
	require.True(t, ok)
	kid, ok := kids[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, redactedValue, kid["secret"])
}

// --- known leaves -------------------------------------------------------------

type LeafBase struct {
	When    time.Time       `json:"when"`
	Raw     json.RawMessage `json:"raw"`
	Err     error           `json:"err"`
	Salt    [16]byte        `json:"salt"`
	Marshal json.Marshaler  `json:"marshal"`
}

type holderWithHit struct {
	LeafBase
	Token string `json:"token"`
}

func TestRedact_KnownLeavesNotExpanded(t *testing.T) {
	holder := LeafBase{
		When: time.Unix(100, 0).UTC(),
		Raw:  json.RawMessage(`{"a":1}`),
		Err:  errors.New("boom"),
	}
	// Without any hit the whole struct passes through untouched.
	out := Redact(map[string]any{"h": holder})
	assert.IsType(t, LeafBase{}, out["h"])

	// With a hit elsewhere in the struct, marshal-visible leaves are preserved
	// verbatim and the array is not expanded element-wise.
	out2 := Redact(map[string]any{"h": holderWithHit{LeafBase: holder, Token: "t"}})
	m, ok := out2["h"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, redactedValue, m["token"])
	assert.Equal(t, holder.When, m["when"], "time.Time must stay a leaf")
	assert.Equal(t, holder.Salt, m["salt"], "[16]byte must stay a leaf")
	assert.Equal(t, holder.Err, m["err"], "error must stay a leaf")
	assert.Equal(t, holder.Raw, m["raw"], "json.RawMessage must stay a leaf")
}

func TestRedact_MapTypedFields(t *testing.T) {
	type bag struct {
		Env map[string]string `json:"env"`
	}
	out := Redact(map[string]any{"b": bag{Env: map[string]string{"HOST": "h", "API_TOKEN": "t"}}})
	m, ok := out["b"].(map[string]any)
	require.True(t, ok)
	env, ok := m["env"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "h", env["HOST"])
	assert.Equal(t, redactedValue, env["API_TOKEN"])
}

// --- guards --------------------------------------------------------------------

type cyclicNode struct {
	Name  string      `json:"name"`
	Next  *cyclicNode `json:"next"`
	Token string      `json:"token"`
}

func TestRedact_CycleGuard(t *testing.T) {
	a := &cyclicNode{Name: "a", Token: "t"}
	a.Next = a // direct cycle through a pointer field
	out := Redact(map[string]any{"n": a})
	m, ok := out["n"].(map[string]any)
	require.True(t, ok, "cycle must not stop the token hit")
	assert.Equal(t, redactedValue, m["token"])

	cyclic := map[string]any{"name": "loop"}
	cyclic["self"] = cyclic // map cycle
	require.NotPanics(t, func() { _ = Redact(cyclic) })
}

func TestRedact_DepthCap(t *testing.T) {
	// Nest a token beyond maxRedactDepth: the walker must terminate and simply
	// stop descending (documented behaviour of the depth cap).
	var cur any = map[string]any{"token": "deep"}
	for i := 0; i < maxRedactDepth+6; i++ {
		cur = map[string]any{"level": cur}
	}
	require.NotPanics(t, func() { _ = Redact(map[string]any{"root": cur}) })
}
