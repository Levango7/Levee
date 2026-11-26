# Log Notify Plugin

This is an example **notify** plugin for LEVEE. It appends each delivery
notification to a local log file, demonstrating the fourth plugin type
(`channel` / `gate` / `module` / `notify`). It needs no external credentials,
so the whole notify path can be exercised locally.

## Structure

```
log_notify/
├── plugin.yaml       # Plugin manifest (type: notify)
├── config.yaml       # Log destination + format
├── main.go           # Plugin entry point (JSON-over-stdio)
└── README.md         # This file
```

## Manifest (plugin.yaml)

`type: notify` and `entry_point: log-notify` (must match the built binary
name).

## Configuration (config.yaml)

| Key      | Type   | Default                | Description                     |
|----------|--------|------------------------|---------------------------------|
| `path`   | string | `/tmp/levee-notify.log` | Log file destination            |
| `format` | string | `text`                 | `text` or `json` log lines      |

## Wire protocol

The host sends one JSON request per line; the plugin answers exactly one line
per request. For `notify`/`init`/`close` the host only inspects `ok`/`error`.
The `meta` handshake reuses the probe-result shape.

```jsonc
// meta -> {"ok":true,"result":{"passed":true,"message":"log-notify","details":{"name":"log-notify","version":"1.0.0","type":"notify","description":"..."}}}
// init -> {"ok":true}
// notify params: {"event":"run_completed","run_id":"run-1","level":"info","title":"...","body":"...","recipients":["ops"],"metadata":{...}}
// notify -> {"ok":true}  (or {"ok":false,"error":"..."} on failure)
// close -> {"ok":true}
```

## Usage

Build and install:

```sh
go build -o examples/plugins/log_notify/log-notify ./examples/plugins/log_notify
levee plugin install ./examples/plugins/log_notify
levee plugin enable log-notify
levee plugin test log-notify       # meta handshake smoke test
```

Once enabled, `levee serve` registers the plugin as a notifier; notifications
matching its routing rules are appended to the configured log file.

## Notes

- One notification is always one log line (embedded newlines are escaped).
- Delivery failures are returned as `ok:false` so they surface in run evidence.
- This is a demonstration notifier — it writes to a local file and has no
  authentication or rate limiting; do not use it as a production alert channel.
