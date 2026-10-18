package chatops

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- single-use signatures through CallbackHandler --------------------------
//
// DingTalk and Feishu sign the timestamp alone; the replay guard makes every
// verified (platform, ts, sig) triple single-use so a captured callback
// cannot be replayed with substituted payloads.

func TestDingtalkCallback_ReplayedSignatureWithDifferentBodyRefused(t *testing.T) {
	bot := newCaptureBot("dd-bot", PlatformDingtalk)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	ts := nowMilli()
	headers := map[string]string{
		"timestamp": ts,
		"sign":      signDDish(testSecret, ts),
	}

	first := postCallback(t, h, headers,
		`{"text":{"content":"@bot /levee approve run-7"},"senderStaffId":"123","conversationId":"cid-1"}`)
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())

	// The attack this closes: same captured signature, substituted payload —
	// a different run approved by a different (unmapped) user.
	attack := postCallback(t, h, headers,
		`{"text":{"content":"@bot /levee approve run-999"},"senderStaffId":"999","conversationId":"cid-1"}`)
	assert.Equal(t, http.StatusUnauthorized, attack.Code)
	assert.Contains(t, attack.Body.String(), ErrCallbackReplay.Error())

	msgs := bot.received()
	require.Len(t, msgs, 1, "only the original delivery may reach the router")
	assert.Equal(t, "/levee approve run-7", msgs[0].Text)
	assert.Equal(t, "123", msgs[0].User)
}

func TestDingtalkCallback_SameBodyReplayAlsoRefused(t *testing.T) {
	bot := newCaptureBot("dd-bot", PlatformDingtalk)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	ts := nowMilli()
	headers := map[string]string{
		"timestamp": ts,
		"sign":      signDDish(testSecret, ts),
	}
	body := `{"text":{"content":"@bot /levee approve run-7"},"senderStaffId":"123"}`

	require.Equal(t, http.StatusOK, postCallback(t, h, headers, body).Code)
	rec := postCallback(t, h, headers, body)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"single-use is uniform: a repeated pair is refused even with the original body")
	assert.Empty(t, bot.received()[1:], "the replay must not dispatch twice")
}

func TestFeishuCallback_ReplayedSignatureRefused(t *testing.T) {
	bot := newCaptureBot("fs-bot", PlatformFeishu)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	ts := nowMilli()
	headers := map[string]string{
		"X-Lark-Request-Timestamp": ts,
		"X-Lark-Signature":         signDDish(testSecret, ts),
	}
	body := `{"event":{"message":{"content":"{\"text\":\"/levee approve run-9\"}"},"sender":{"sender_id":{"open_id":"ou_1"}}}}`

	require.Equal(t, http.StatusOK, postCallback(t, h, headers, body).Code)

	rec := postCallback(t, h, headers,
		`{"event":{"message":{"content":"{\"text\":\"/levee reject run-9\"}"},"sender":{"sender_id":{"open_id":"ou_1"}}}}`)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	msgs := bot.received()
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Text, "approve", "the substituted reject must never dispatch")
}

func TestSlackCallback_ReplayedSignatureRefused(t *testing.T) {
	bot := newCaptureBot("slack-bot", PlatformSlack)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	body := `{"command":"/levee","text":"approve run-42","user_id":"U1"}`
	ts := nowSec()
	sig, _ := signSlack(testSecret, ts, []byte(body))
	headers := map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         sig,
	}

	require.Equal(t, http.StatusOK, postCallback(t, h, headers, body).Code)
	rec := postCallback(t, h, headers, body)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"Slack's MAC binds the body, but the single-use rule is uniform across platforms")
	require.Len(t, bot.received(), 1)
}

func TestDingtalkCallback_FailedVerificationDoesNotConsumeSignature(t *testing.T) {
	bot := newCaptureBot("dd-bot", PlatformDingtalk)
	h, err := CallbackHandler(bot, testSecret, nil)
	require.NoError(t, err)

	ts := nowMilli()
	body := `{"text":{"content":"@bot /levee approve run-7"},"senderStaffId":"123"}`

	// A forged attempt on the same ts slot must not poison the guard: the
	// guard only records pairs that PASSED verification, so garbage never
	// reaches it (and cannot evict real entries either).
	forged := postCallback(t, h, map[string]string{"timestamp": ts, "sign": "deadbeef"}, body)
	require.Equal(t, http.StatusUnauthorized, forged.Code)

	rec := postCallback(t, h, map[string]string{"timestamp": ts, "sign": signDDish(testSecret, ts)}, body)
	assert.Equal(t, http.StatusOK, rec.Code, "the legitimate first use must still be accepted")
	require.Len(t, bot.received(), 1)
}

// --- replayGuard unit tests (white box, fake clock) -------------------------

func TestReplayGuard_FirstUseTrueThenFalse(t *testing.T) {
	g := newReplayGuard(time.Minute, 8)
	assert.True(t, g.first(PlatformDingtalk, "1000", "aa"), "first presentation is unseen")
	assert.False(t, g.first(PlatformDingtalk, "1000", "aa"), "second presentation is a replay")
	assert.True(t, g.first(PlatformDingtalk, "1000", "bb"), "a different signature is a different request")
	assert.True(t, g.first(PlatformSlack, "1000", "aa"), "a different platform is a different request")
}

func TestReplayGuard_EntryExpiresAfterTTL(t *testing.T) {
	now := time.Unix(1700000000, 0)
	g := newReplayGuard(time.Minute, 8)
	g.now = func() time.Time { return now }

	require.True(t, g.first(PlatformFeishu, "1000", "aa"), "first presentation is unseen")

	now = now.Add(2 * time.Minute) // past the ttl
	assert.True(t, g.first(PlatformFeishu, "1000", "aa"),
		"an expired entry is treated as unseen; the caller's freshness check refuses stale timestamps anyway")
}

func TestReplayGuard_LRUEvictsOldest(t *testing.T) {
	g := newReplayGuard(time.Hour, 2)
	now := time.Unix(1700000000, 0)
	g.now = func() time.Time { return now }

	require.True(t, g.first(PlatformDingtalk, "1", "aa"))
	require.True(t, g.first(PlatformDingtalk, "2", "bb"))
	require.True(t, g.first(PlatformDingtalk, "3", "cc"), "capacity 2 must admit a third distinct request")

	// "1" was evicted: presenting it again is treated as first use.
	assert.True(t, g.first(PlatformDingtalk, "1", "aa"))
	// "3" was refreshed by nothing, but it is still inside ttl and capacity:
	assert.False(t, g.first(PlatformDingtalk, "3", "cc"))
}

func TestReplayGuard_NilGuardAcceptsEverything(t *testing.T) {
	var g *replayGuard
	assert.True(t, g.first(PlatformSlack, "1", "aa"))
	assert.True(t, g.first(PlatformSlack, "1", "aa"), "nil guard is disabled: no replay refusal")
}
