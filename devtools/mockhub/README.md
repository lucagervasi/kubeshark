# mockhub

A development-only stand-in for the real `kubeshark-hub`. It implements every
endpoint the Kubeshark CLI (`internal/connect.Connector`) talks to, **receives**
whatever the CLI sends, and **displays** it — logged with structured fields,
pretty-printed to stdout, and streamed to any connected `kubeshark console`
client.

Use it to see exactly what the CLI sends to the Hub without a real cluster.

## Run

```shell
go run ./devtools/mockhub --port 8898        # 8898 is the CLI's default Hub port
go run ./devtools/mockhub --port 8898 --debug # verbose (includes health probes)
```

Then point the CLI at it. Anything that reuses the `tap` proxy port works, e.g.:

```shell
kubeshark console --proxy-hub-port 8898
```

## Endpoints mirrored

| Method | Path | What the CLI sends |
|---|---|---|
| `GET` | `/echo` | reachability / health probe |
| `POST` | `/pods/worker` | a serialized `v1.Pod` |
| `POST` | `/pods/regex` | `{regex, namespaces}` |
| `POST` | `/license` | `{license}` |
| `POST` | `/scripts/env` | scripting env map |
| `POST` | `/scripts` | a `Script` — replies with `{"index": N}` |
| `PUT` | `/scripts/{index}` | an updated `Script` |
| `DELETE` | `/scripts/{index}` | — |
| `POST` | `/scripts/done` | — |
| `WS` | `/scripts/logs` | console stream (mockhub pushes received payloads here) |

Any other request is logged under `unhandled` and answered with `200` so the
CLI's ret/reachability loops keep working.

> This tool is intentionally **not** wired into the `kubeshark` binary — it's a
> separate `main` package under `devtools/`.
