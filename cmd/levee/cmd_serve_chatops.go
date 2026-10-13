// cmd_serve_chatops.go — the inbound half of the ChatOps wiring.
//
// Until this file the ChatOps layer was outbound-only: approval lifecycle
// events were projected into the chat room, but the verbs behind the cards
// (通过 / 驳回) were the command router's built-in no-ops, so a click in a
// chat client answered "approve: no handler registered". Two things close
// that loop:
//
//  1. The decision verbs (approve / reject / status) are registered on the
//     manager's router, backed by the approval service the change service
//     itself decides over — not a second instance over the same store, so
//     the DecisionObserver fan-out (the room sees "1/2 approved") still
//     fires.
//  2. Every bot with a signing secret gets a webhook ingress mounted as a
//     PUBLIC route: the platform signs what it sends and cannot carry a
//     LEVEE bearer token, and the MAC check in chatops.CallbackHandler is
//     what stands in for one. A bot without a secret keeps its outbound
//     mirror and simply gets no inbound route (logged, not silent) — that
//     is a deliberate non-breaking choice for visibility-only deployments,
//     and it is why the ingress is per-bot rather than a boot-wide rule.
package main

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/notify/chatopsbridge"
)

// chatOpsCallbackRoute is one mounted ingress: the path a platform calls
// and the handler that verifies and dispatches its callback.
type chatOpsCallbackRoute struct {
	Bot     string
	Path    string
	Handler http.Handler
}

// wireChatOpsDecisions registers the approve / reject / status verbs on the
// manager's command router. It is a no-op when mgr is nil (ChatOps
// disabled), and the identity tables are assembled per platform because a
// user id is only unique within its platform.
func wireChatOpsDecisions(mgr *chatops.BotManager, cfg config.ChatOpsConfig,
	appr *approval.Service, changeSvc *grpc.ChangeService) {
	if mgr == nil {
		return
	}
	tables := make(map[chatops.Platform]map[string]string)
	for _, b := range cfg.Bots {
		if len(b.IdentityMap) == 0 {
			continue
		}
		platform, err := chatops.ParsePlatform(b.Platform)
		if err != nil {
			continue // the bot itself was already refused for this reason
		}
		table := tables[platform]
		if table == nil {
			table = make(map[string]string)
			tables[platform] = table
		}
		for k, v := range b.IdentityMap {
			table[k] = v
		}
	}

	decisions := chatopsbridge.NewDecisionRouter(appr, changeSvc, chatopsbridge.MapIdentity(tables))
	for verb, handler := range decisions.Handlers() {
		mgr.Router().Register(verb, handler)
	}
	log.Info("chatops inbound decisions wired", "verbs", mgr.Router().Verbs(),
		"identity_platforms", len(tables))
}

// chatOpsCallbackRoutes builds the webhook ingress route for every bot that
// declares a signing secret. Bots without one are skipped with a named log
// line: an outbound-only mirror keeps working, and the absence of an
// inbound path is stated rather than discovered by a click that nothing
// answers.
func chatOpsCallbackRoutes(mgr *chatops.BotManager, cfg config.ChatOpsConfig) ([]chatOpsCallbackRoute, error) {
	if mgr == nil {
		return nil, nil
	}
	var routes []chatOpsCallbackRoute
	for _, b := range cfg.Bots {
		bot, err := mgr.Get(b.Name)
		if err != nil {
			return nil, fmt.Errorf("chatops callback route for %q: %w", b.Name, err)
		}
		if b.Secret == "" {
			log.Warn("chatops inbound path not mounted: no signing secret",
				"bot", b.Name, "platform", b.Platform,
				"hint", "set notify.chatops.bots[].secret to receive /levee commands from this platform")
			continue
		}
		handler, err := chatops.CallbackHandler(bot, b.Secret, nil)
		if err != nil {
			return nil, fmt.Errorf("chatops callback route for %q: %w", b.Name, err)
		}
		routes = append(routes, chatOpsCallbackRoute{
			Bot:     b.Name,
			Path:    chatOpsCallbackPath(b.Name),
			Handler: handler,
		})
	}
	return routes, nil
}

// wireServeChatOpsInbound registers the decision verbs and mounts every
// signed bot's callback ingress. Split out of runServe (which sits at the
// gocyclo threshold) for the same reason buildEngineServeOptions was: the
// behaviour is identical, the budget is spent where it is readable.
func wireServeChatOpsInbound(gw *grpc.Gateway, mgr *chatops.BotManager, cfg config.ChatOpsConfig,
	appr *approval.Service, changeSvc *grpc.ChangeService) error {
	if mgr == nil {
		return nil
	}
	wireChatOpsDecisions(mgr, cfg, appr, changeSvc)
	routes, err := chatOpsCallbackRoutes(mgr, cfg)
	if err != nil {
		return err
	}
	for _, route := range routes {
		gw.SetPublicRoute(route.Path, route.Handler)
		log.Info("chatops inbound ingress mounted", "bot", route.Bot, "path", route.Path)
	}
	return nil
}

// chatOpsCallbackPath is the ingress path for one bot. The name is
// url-escaped because it is also the bot's registration key and may contain
// characters a mux pattern would read as wildcards.
func chatOpsCallbackPath(name string) string {
	return "/chatops/callback/" + url.PathEscape(name)
}
