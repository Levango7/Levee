// replay.go is the single-use guard behind the callback ingress. The three
// platforms differ in what their signature covers, and the difference is a
// replay surface: Slack binds the request body into its MAC, so a captured
// (timestamp, signature) pair can only replay the identical body. DingTalk
// and Feishu sign the timestamp alone, so within the freshness window the
// same pair is valid for ANY body — an observer of one legitimate callback
// could substitute payloads into it for up to an hour (rename the run, swap
// the user). The guard closes that gap uniformly: every (platform,
// timestamp, signature) triple this handler has verified is refused on its
// second presentation, whatever the body.
//
// Cost model: an entry lives one freshness window plus a minute of clock
// slack — a triple older than its window fails the freshness check anyway,
// so remembering it longer is pure waste — and the LRU capacity bounds
// memory under flood. Verification runs BEFORE the guard (see
// CallbackHandler), so an unauthenticated flood cannot evict real entries:
// it never reaches the guard.
package chatops

import (
	"container/list"
	"sync"
	"time"
)

// replayGuardCapacity bounds the guard's memory. Chat callback volume is
// human-click volume; 4096 verified callbacks inside one freshness window
// is far past any real deployment, and eviction of the oldest entry is the
// documented degradation, not an error.
const replayGuardCapacity = 4096

// replayGuard remembers verified callback signatures and refuses their
// second presentation within the ttl. It is safe for concurrent use.
type replayGuard struct {
	ttl time.Duration
	cap int
	// now is swappable so tests can drive expiry without sleeping.
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*list.Element // key -> element in order
	order   *list.List               // front = most recently seen
}

type replayEntry struct {
	key      string
	deadline time.Time
}

func newReplayGuard(ttl time.Duration, capacity int) *replayGuard {
	return &replayGuard{
		ttl:     ttl,
		cap:     capacity,
		now:     time.Now,
		entries: make(map[string]*list.Element, capacity),
		order:   list.New(),
	}
}

// first reports whether (p, ts, sig) is presented for the first time within
// the ttl. A nil guard is disabled and accepts everything: the nil case
// exists so a call site built without a guard needs no branching.
func (g *replayGuard) first(p Platform, ts, sig string) bool {
	if g == nil {
		return true
	}
	key := string(p) + "\x00" + ts + "\x00" + sig
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()

	if el, ok := g.entries[key]; ok {
		if now.Before(el.Value.(*replayEntry).deadline) {
			g.order.MoveToFront(el)
			return false
		}
		// Expired: its timestamp is stale, so the caller's freshness check
		// refuses a replay regardless; drop the dead entry and treat the
		// key as unseen.
		g.order.Remove(el)
		delete(g.entries, key)
	}

	for g.order.Len() >= g.cap {
		oldest := g.order.Back()
		if oldest == nil {
			break
		}
		g.order.Remove(oldest)
		delete(g.entries, oldest.Value.(*replayEntry).key)
	}

	g.entries[key] = g.order.PushFront(&replayEntry{key: key, deadline: now.Add(g.ttl)})
	return true
}
