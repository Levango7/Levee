// notifier.go is the execution half of the notifier plugin type: it adapts
// an enabled registry record (TypeNotifier) into a notify.Notifier the
// serving NotificationManager can register — the first engine consumption
// point of the plugin system. One delivery = one short-lived process
// speaking the JSON-line protocol: init (the config captured at install
// time), notify (the message), close.
package plugin

import (
	"context"
	"fmt"

	"github.com/nexus/levee/internal/notify"
)

// PluginNotifier delivers notify.Message to a notifier plugin sub-process.
// The zero value is not usable; construct with NewPluginNotifier.
type PluginNotifier struct {
	name       string
	binaryPath string
	configYAML string
}

// NewPluginNotifier builds a channel adapter from an enabled registry
// record. It does NOT validate the binary here — loading happens per send
// (resolvePluginBinary), so a binary removed after registration surfaces as
// a per-delivery error rather than a stale boot-time snapshot.
func NewPluginNotifier(rec *RegistryRecord) (*PluginNotifier, error) {
	if rec == nil {
		return nil, fmt.Errorf("plugin: notifier: nil registry record")
	}
	if rec.Type != TypeNotifier {
		return nil, wrapError(rec.Name, fmt.Errorf("%w: record is %q, want %q", ErrInvalidPluginType, rec.Type, TypeNotifier))
	}
	if rec.State != StateEnabled {
		return nil, wrapError(rec.Name, fmt.Errorf("%w: record state is %q", ErrPluginNotEnabled, rec.State))
	}
	return &PluginNotifier{
		name:       rec.Name,
		binaryPath: rec.BinaryPath,
		configYAML: rec.ConfigYAML,
	}, nil
}

// Name implements notify.Notifier: the plugin's registry name is the
// channel's unique key.
func (p *PluginNotifier) Name() string { return p.name }

// Send delivers one message: spawn, init (registry config), notify, close.
// A plugin refusal (ok:false) or protocol breach surfaces as the error;
// NotificationManager aggregates those into its delivery result.
func (p *PluginNotifier) Send(ctx context.Context, msg notify.Message) error {
	bin, err := resolvePluginBinary(p.binaryPath)
	if err != nil {
		return wrapError(p.name, err)
	}

	sendCtx, cancel := context.WithTimeout(ctx, notifyDeadline)
	defer cancel()

	proc, err := startPluginProc(sendCtx, bin)
	if err != nil {
		return wrapError(p.name, err)
	}
	defer proc.close()

	// 1. init: hand over the config captured at install time, so the
	//    operator's `plugin install` choices govern delivery (endpoints,
	//    secrets references, format options) without re-declaring them.
	if p.configYAML != "" {
		cfg, err := parseConfigYAML(p.configYAML)
		if err != nil {
			return wrapError(p.name, fmt.Errorf("stored config: %w", err))
		}
		resp, err := proc.request(HostRequest{Action: "init", Config: cfg})
		if err != nil {
			return wrapError(p.name, err)
		}
		if !resp.OK {
			return wrapError(p.name, fmt.Errorf("init refused: %s", resp.Error))
		}
	}

	// 2. notify: the message itself, mapped onto the NotifyMessage shape
	//    the plugin interface declares.
	resp, err := proc.request(HostRequest{Action: "notify", Params: notifyParams(msg)})
	if err != nil {
		return wrapError(p.name, err)
	}
	if !resp.OK {
		return wrapError(p.name, fmt.Errorf("delivery refused: %s", resp.Error))
	}
	return nil
}

// notifyParams maps a notify.Message onto the wire shape of
// plugin.NotifyMessage. The field names are the plugin interface's JSON
// contract (types.go), so a plugin written against NotifierPlugin reads
// exactly what the host sent.
func notifyParams(msg notify.Message) map[string]any {
	recipients := make([]string, 0, len(msg.Recipients))
	for _, r := range msg.Recipients {
		recipients = append(recipients, r.ID)
	}
	params := map[string]any{
		"event":      msg.Event,
		"run_id":     msg.RunID,
		"level":      msg.Level.String(),
		"title":      msg.Title,
		"body":       msg.Body,
		"recipients": recipients,
	}
	if len(msg.Metadata) > 0 {
		md := make(map[string]any, len(msg.Metadata))
		for k, v := range msg.Metadata {
			md[k] = v
		}
		params["metadata"] = md
	}
	return params
}
