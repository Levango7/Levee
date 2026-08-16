package push

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NewDeepLinkGenerator --------------------------------------------------

func TestNewDeepLinkGenerator_Defaults(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "https://levee.example.com")
	assert.Equal(t, "levee", g.scheme)
	assert.Equal(t, "https://levee.example.com", g.baseURL)
	assert.Equal(t, DefaultDeepLinkTTL, g.currentTTL())
}

func TestNewDeepLinkGenerator_EmptySchemeOK(t *testing.T) {
	g := NewDeepLinkGenerator("", "")
	link, err := g.GenerateShowLink("run-1")
	require.NoError(t, err)
	assert.Contains(t, link.URL, "levee://") // default scheme
}

// --- GenerateApprovalLink --------------------------------------------------

func TestGenerateApprovalLink_Success(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "https://levee.example.com")
	link, err := g.GenerateApprovalLink("run-42", "alice")
	require.NoError(t, err)
	assert.Equal(t, ActionApprove, link.Action)
	assert.Equal(t, "run-42", link.ResourceID)
	assert.NotEmpty(t, link.Token)
	assert.True(t, link.Expiry.After(time.Now()))
	assert.Contains(t, link.URL, "levee://approve/run-42")
	assert.Contains(t, link.URL, "token=")
}

func TestGenerateApprovalLink_EmptyRunID(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	_, err := g.GenerateApprovalLink("", "alice")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty run id")
}

func TestGenerateApprovalLink_EmptyUserID(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	_, err := g.GenerateApprovalLink("run-1", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty user id")
}

// --- GenerateRejectLink ----------------------------------------------------

func TestGenerateRejectLink_Success(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	link, err := g.GenerateRejectLink("run-1", "bob")
	require.NoError(t, err)
	assert.Equal(t, ActionReject, link.Action)
	assert.Contains(t, link.URL, "levee://reject/run-1")
}

// --- GenerateShowLink ------------------------------------------------------

func TestGenerateShowLink_Success(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "https://levee.example.com")
	link, err := g.GenerateShowLink("run-99")
	require.NoError(t, err)
	assert.Equal(t, ActionShow, link.Action)
	// Show links use the baseURL for non-mobile fallback.
	assert.Contains(t, link.URL, "https://levee.example.com/changes/run-99")
}

func TestGenerateShowLink_EmptyRunID(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	_, err := g.GenerateShowLink("")
	require.Error(t, err)
}

func TestGenerateShowLink_AllowsEmptyUserID(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	_, err := g.GenerateShowLink("run-1")
	require.NoError(t, err)
}

// --- ValidateToken ---------------------------------------------------------

func TestValidateToken_Success(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	link, err := g.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)

	runID, userID, action, err := g.ValidateToken(link.Token)
	require.NoError(t, err)
	assert.Equal(t, "run-1", runID)
	assert.Equal(t, "alice", userID)
	assert.Equal(t, ActionApprove, action)
}

func TestValidateToken_SingleUse(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	link, err := g.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)

	_, _, _, err = g.ValidateToken(link.Token)
	require.NoError(t, err)

	// Second call must fail.
	_, _, _, err = g.ValidateToken(link.Token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestValidateToken_UnknownToken(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	_, _, _, err := g.ValidateToken("never-issued")
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestValidateToken_EmptyToken(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	_, _, _, err := g.ValidateToken("")
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestValidateToken_Expired(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	g.SetTTL(1 * time.Millisecond)
	link, err := g.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond)
	_, _, _, err = g.ValidateToken(link.Token)
	assert.ErrorIs(t, err, ErrTokenExpired)
}

func TestValidateToken_DistinguishesActions(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	approveLink, _ := g.GenerateApprovalLink("run-1", "alice")
	rejectLink, _ := g.GenerateRejectLink("run-1", "alice")

	_, _, action, err := g.ValidateToken(approveLink.Token)
	require.NoError(t, err)
	assert.Equal(t, ActionApprove, action)

	_, _, action, err = g.ValidateToken(rejectLink.Token)
	require.NoError(t, err)
	assert.Equal(t, ActionReject, action)
}

// --- GenerateToken ---------------------------------------------------------

func TestGenerateToken_Unique(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		tok := g.GenerateToken()
		assert.NotEmpty(t, tok)
		assert.False(t, seen[tok], "token %q duplicated at iteration %d", tok, i)
		seen[tok] = true
	}
}

func TestGenerateToken_HexEncoded(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	tok := g.GenerateToken()
	// 32 bytes -> 64 hex chars.
	assert.Len(t, tok, 64)
	for _, c := range tok {
		assert.True(t, strings.ContainsRune("0123456789abcdef", c),
			"token contains non-hex char %q", c)
	}
}

// --- CleanupExpired --------------------------------------------------------

func TestCleanupExpired_RemovesExpiredTokens(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	g.SetTTL(1 * time.Millisecond)
	_, err := g.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)
	_, err = g.GenerateApprovalLink("run-2", "bob")
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond)
	removed := g.CleanupExpired()
	assert.Equal(t, 2, removed)
	// Second call is a no-op.
	assert.Equal(t, 0, g.CleanupExpired())
}

func TestCleanupExpired_KeepsLiveTokens(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	_, err := g.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)
	assert.Equal(t, 0, g.CleanupExpired())
}

// --- SetTTL ----------------------------------------------------------------

func TestSetTTL_AffectsGeneratedLinks(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	g.SetTTL(5 * time.Second)
	link, err := g.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(5*time.Second), link.Expiry, 1*time.Second)
}

// --- URL format ------------------------------------------------------------

func TestGenerateApprovalLink_URLFormat(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	link, err := g.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)
	// Format: levee://approve/run-1?token=<64hex>
	assert.True(t, strings.HasPrefix(link.URL, "levee://approve/run-1?token="))
}

func TestGenerateRejectLink_URLFormat(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	link, err := g.GenerateRejectLink("run-7", "bob")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(link.URL, "levee://reject/run-7?token="))
}

func TestGenerateShowLink_URLFormatWithBaseURL(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "https://levee.example.com")
	link, err := g.GenerateShowLink("run-3")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(link.URL, "https://levee.example.com/changes/run-3?token="))
}

func TestGenerateShowLink_URLFormatWithoutBaseURL(t *testing.T) {
	g := NewDeepLinkGenerator("levee", "")
	link, err := g.GenerateShowLink("run-3")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(link.URL, "levee://show/run-3?token="))
}