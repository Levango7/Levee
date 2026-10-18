// ingress.go is the webhook receiver layer the platform adapters delegate
// to. Each adapter's file says the same thing — "signature verification /
// token refresh belong to the ingress layer" — and until this file that
// layer did not exist: the ParseXCallback helpers shipped with zero
// production callers, so a message typed in a chat client reached nobody.
//
// The ingress has exactly one job: prove a callback really came from the
// platform it claims, then hand the canonical IncomingMessage to the bot
// (which routes it through the command dispatcher and replies). The proof
// is a MAC under the bot's signing secret — and the three platforms differ
// in what the MAC covers, a difference that matters for replay:
//
//   - Slack     X-Slack-Signature: v0=hex(hmac_sha256("v0:"+secret, "v0:"+ts+":"+body))
//     The request BODY is inside the MAC, so a captured (ts, signature)
//     pair can only replay the identical body.
//   - DingTalk  sign:              hex(hmac_sha256(secret, ts+"\n"+secret))
//   - Feishu    X-Lark-Signature:  hex(hmac_sha256(ts+"\n"+secret, secret))
//     These two sign ONLY the timestamp. Within the freshness window a
//     captured pair is valid for ANY body: an observer of one legitimate
//     callback could substitute payloads into it for up to an hour — a
//     different run id, a different user.
//
// The ingress therefore makes every verified (platform, timestamp,
// signature) triple single-use (replay.go): a replayed signature is
// refused even when it carries the original body. The cost is bounded and
// deliberate — a platform redelivery repeats the original ts+signature and
// gets 401, which is safe because the first delivery either acked (the
// platform stops there) or crashed mid-dispatch, and decision verbs are
// idempotent at the approval state machine (ErrDuplicateDecision): the
// recoverable action for a lost command is clicking again.
//
// A callback endpoint that accepts decisions without this proof would let
// anyone who can reach the port approve changes as anyone, which is why a
// bot with no secret REFUSES to mount an ingress rather than mounting one
// that trusts everybody (see CallbackHandler).
package chatops

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Ingress error sentinels. Callers may use errors.Is regardless of the
// wrapping the handlers apply.
var (
	// ErrCallbackSecretMissing is returned by NewCallbackHandler when the
	// bot declares no signing secret. An unauthenticated decision endpoint
	// is worse than a missing one, so this is a hard stop.
	ErrCallbackSecretMissing = errors.New("chatops: callback ingress requires a signing secret")

	// ErrCallbackBadSignature is returned when the MAC does not match:
	// wrong secret, or a body altered in flight.
	ErrCallbackBadSignature = errors.New("chatops: callback signature mismatch")

	// ErrCallbackStaleTimestamp is returned when the platform's timestamp
	// is outside the freshness window. It exists so a captured callback
	// cannot be replayed at leisure.
	ErrCallbackStaleTimestamp = errors.New("chatops: callback timestamp outside freshness window")

	// ErrCallbackReplay is returned when a (timestamp, signature) pair is
	// presented a second time within its freshness window. DingTalk and
	// Feishu sign the timestamp alone, so without the single-use guard a
	// captured pair would stay valid for any body until the window closed;
	// the guard refuses all repeats, including Slack's body-bound ones.
	ErrCallbackReplay = errors.New("chatops: callback replay refused (signature already used)")
)

// maxCallbackBytes bounds a callback body. Platform callbacks carry a
// command and a card payload; anything past this is not a slash command
// and is refused before it is read into memory.
const maxCallbackBytes = 1 << 20

// Freshness windows per platform, in seconds, following each platform's
// own replay guidance: Slack rejects anything older than five minutes,
// DingTalk and Feishu allow an hour of clock skew. They are fields rather
// than constants so a deployment can tighten them.
type ingressWindow struct {
	slack    time.Duration
	ddFeishu time.Duration
}

func defaultIngressWindow() ingressWindow {
	return ingressWindow{
		slack:    5 * time.Minute,
		ddFeishu: time.Hour,
	}
}

// CallbackHandler returns an http.Handler that verifies a platform callback
// and routes it through bot.HandleMessage (which dispatches the command
// and sends the reply back to the channel). It refuses to build when
// secret is empty: the caller must treat that as a boot failure, not skip
// the route.
//
// The handler authenticates by MAC, never by bearer token: the platform
// signs what it sends and cannot carry a LEVEE credential. Mount it on a
// route that is exempt from bearer auth (see Gateway.SetPublicRoute) —
// the exemption is safe precisely because of the check performed here.
func CallbackHandler(bot Bot, secret string, window *ingressWindow) (http.Handler, error) {
	if secret == "" {
		return nil, fmt.Errorf("chatops: callback ingress for bot %q: %w", bot.Name(), ErrCallbackSecretMissing)
	}
	w := defaultIngressWindow()
	if window != nil {
		w = *window
	}
	// Guard ttl = the longest freshness window + a minute of clock slack:
	// an older triple fails freshness regardless, so remembering it longer
	// is pure waste.
	guardTTL := w.slack
	if w.ddFeishu > guardTTL {
		guardTTL = w.ddFeishu
	}
	guard := newReplayGuard(guardTTL+time.Minute, replayGuardCapacity)
	p := bot.Platform()
	return http.HandlerFunc(func(wr http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(wr, r.Body, maxCallbackBytes))
		if err != nil {
			http.Error(wr, "callback too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err := verifyCallback(p, secret, w, r, body); err != nil {
			http.Error(wr, err.Error(), http.StatusUnauthorized)
			return
		}
		// Single-use signatures, AFTER verification: only pairs that proved
		// themselves occupy guard slots, so an unauthenticated flood cannot
		// evict real entries. A repeated pair is refused even with the
		// original body — DingTalk and Feishu sign the timestamp alone, so
		// without this a captured pair could carry substituted payloads for
		// the whole freshness window.
		ts, sig := callbackSignature(p, r)
		if !guard.first(p, ts, sig) {
			http.Error(wr, ErrCallbackReplay.Error(), http.StatusUnauthorized)
			return
		}
		// Feishu (and Slack's Events API) prove endpoint ownership with a
		// challenge before any real traffic: echo it, nothing to dispatch.
		if challenge := decodeChallenge(body); challenge != "" {
			wr.Header().Set("Content-Type", "application/json")
			wr.WriteHeader(http.StatusOK)
			_, _ = wr.Write([]byte(`{"challenge":` + strconv.Quote(challenge) + `}`))
			return
		}
		msg, err := parseCallback(p, body)
		if err != nil {
			http.Error(wr, err.Error(), http.StatusBadRequest)
			return
		}
		// HandleMessage routes the command and sends the reply back to the
		// channel. Its error (an unknown verb) is already answered
		// in-channel; the platform only needs an ack to stop retrying.
		_ = bot.HandleMessage(r.Context(), msg)
		wr.WriteHeader(http.StatusOK)
	}), nil
}

// parseCallback dispatches to the platform parser. Verification has already
// bound the body to the platform, so a mismatch here means the platform
// sent a shape this build does not understand.
func parseCallback(p Platform, body []byte) (IncomingMessage, error) {
	switch p {
	case PlatformSlack:
		return ParseSlackCallback(body)
	case PlatformDingtalk:
		return ParseDingtalkCallback(body)
	case PlatformFeishu:
		return ParseFeishuCallback(body)
	default:
		return IncomingMessage{}, fmt.Errorf("chatops: parse callback for %q: %w", p, ErrUnknownPlatform)
	}
}

// callbackSignature reads the (timestamp, signature) header pair a platform
// signs with. The freshness and MAC checks in verify* and the replay guard
// key must agree on this pair, so it is read in exactly one place.
func callbackSignature(p Platform, r *http.Request) (ts, sig string) {
	switch p {
	case PlatformSlack:
		return r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature")
	case PlatformDingtalk:
		return r.Header.Get("timestamp"), r.Header.Get("sign")
	case PlatformFeishu:
		return r.Header.Get("X-Lark-Request-Timestamp"), r.Header.Get("X-Lark-Signature")
	default:
		return "", ""
	}
}

// decodeChallenge extracts a Feishu url_verification challenge (Slack's
// Events API uses the same field name). An empty body or one without a
// challenge is not an error: most callbacks have none.
func decodeChallenge(body []byte) string {
	var probe struct {
		Challenge string `json:"challenge"`
		Type      string `json:"type"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	if probe.Type == "url_verification" {
		return probe.Challenge
	}
	return ""
}

// --- signature verification -------------------------------------------------

// verifyCallback checks the platform MAC and the timestamp freshness. Both
// must pass: the MAC proves origin, the timestamp bounds replay.
func verifyCallback(p Platform, secret string, w ingressWindow, r *http.Request, body []byte) error {
	switch p {
	case PlatformSlack:
		return verifySlackCallback(secret, w.slack, r, body)
	case PlatformDingtalk:
		return verifyDingtalkCallback(secret, w.ddFeishu, r)
	case PlatformFeishu:
		return verifyFeishuCallback(secret, w.ddFeishu, r)
	default:
		return fmt.Errorf("chatops: verify callback for %q: %w", p, ErrUnknownPlatform)
	}
}

// verifySlackCallback implements Slack's v0 signing scheme:
// basestring = "v0:{timestamp}:{body}", signed with the key "v0:{secret}".
func verifySlackCallback(secret string, window time.Duration, r *http.Request, body []byte) error {
	ts, sig := callbackSignature(PlatformSlack, r)
	if ts == "" || sig == "" {
		return fmt.Errorf("chatops: slack callback: missing signature headers")
	}
	if err := checkFreshness(ts, window, 1); err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte("v0:"+secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return fmt.Errorf("chatops: slack callback: %w", ErrCallbackBadSignature)
	}
	return nil
}

// verifyDingtalkCallback implements DingTalk's robot signature:
// sign = hex(hmac_sha256(timestamp + "\n" + secret, secret)).
func verifyDingtalkCallback(secret string, window time.Duration, r *http.Request) error {
	ts, sig := callbackSignature(PlatformDingtalk, r)
	if ts == "" || sig == "" {
		return fmt.Errorf("chatops: dingtalk callback: missing signature headers")
	}
	if err := checkFreshness(ts, window, 1000); err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(ts+"\n"+secret))
	mac.Write([]byte(secret))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return fmt.Errorf("chatops: dingtalk callback: %w", ErrCallbackBadSignature)
	}
	return nil
}

// verifyFeishuCallback implements Feishu's event signature, the same
// construction as DingTalk's with different header names:
// signature = hex(hmac_sha256(timestamp + "\n" + secret, secret)).
func verifyFeishuCallback(secret string, window time.Duration, r *http.Request) error {
	ts, sig := callbackSignature(PlatformFeishu, r)
	if ts == "" || sig == "" {
		return fmt.Errorf("chatops: feishu callback: missing signature headers")
	}
	if err := checkFreshness(ts, window, 1000); err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(ts+"\n"+secret))
	mac.Write([]byte(secret))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return fmt.Errorf("chatops: feishu callback: %w", ErrCallbackBadSignature)
	}
	return nil
}

// checkFreshness parses a platform timestamp (seconds, or milliseconds when
// perSecond is 1000) and refuses one outside ±window of now. A malformed
// timestamp is refused, not ignored: an attacker controls the header, so
// "unparseable" must fail closed.
func checkFreshness(raw string, window time.Duration, perSecond int) error {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("chatops: callback timestamp %q: %w", raw, ErrCallbackStaleTimestamp)
	}
	at := time.Unix(n/int64(perSecond), (n%int64(perSecond))*int64(time.Second)/int64(perSecond))
	skew := time.Since(at)
	if skew < -window || skew > window {
		return fmt.Errorf("chatops: callback timestamp %q (skew %s): %w", raw, skew.Round(time.Second), ErrCallbackStaleTimestamp)
	}
	return nil
}
