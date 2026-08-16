# HTTP Probe Gate Plugin

This is an example gate plugin for LEVEE that performs HTTP health checks
against a target URL. It demonstrates the plugin structure and the
GatePlugin interface.

## Structure

```
http_probe/
├── plugin.yaml       # Plugin manifest (metadata)
├── config.yaml       # Plugin configuration (defaults)
├── main.go           # Plugin entry point
└── README.md         # This file
```

## Manifest (plugin.yaml)

The manifest declares the plugin's metadata: name, version, type, author,
description and host-version compatibility range.

## Configuration (config.yaml)

The configuration is a YAML map passed to the plugin's `Init` method.
The following keys are recognised:

| Key           | Type   | Default | Description                        |
|---------------|--------|---------|------------------------------------|
| `timeout`     | string | `10s`   | HTTP request timeout               |
| `retry`       | int    | `3`     | Number of retries on failure       |
| `expect_code` | int    | `200`   | Expected HTTP status code          |
| `expect_body` | string | ``      | Substring expected in response body |

## Usage

Install the plugin:

```sh
levee plugin install ./examples/plugins/http_probe
```

Enable the plugin:

```sh
levee plugin enable http-probe
```

The plugin can then be referenced in a workflow's gate definition:

```yaml
gates:
  pre_apply:
    - name: http-probe
      params:
        url: "https://example.com/health"
        expect_code: 200
```