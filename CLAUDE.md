# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

This is the **Kubeshark CLI** — a Go binary (`kubeshark`) that orchestrates Kubeshark's traffic-analysis stack inside a Kubernetes cluster. The CLI itself does not capture traffic. It installs three server-side components into the cluster via Helm and then proxies/port-forwards to them:

- `kubeshark-hub` — API server (container port 80, host port 8898 by default)
- `kubeshark-front` — Web UI (container port 80, host port 8899 by default)
- `kubeshark-worker` — DaemonSet that does the actual packet capture per node

The components themselves live in other repositories. This repo also ships:
- `helm-chart/` — the Helm chart published to `https://helm.kubeshark.co` (chart `appVersion` in `Chart.yaml` is the source of truth for the release version)
- `manifests/` — equivalent raw `kubectl apply -f` YAML for users who don't want Helm

## Build / test / lint

All build is driven by the `Makefile`. Build artifacts go to `./bin/kubeshark_<GOOS>_<GOARCH>`.

```shell
make                              # default = `build` (CGO_ENABLED=0, static, stripped)
make build-debug                  # CGO_ENABLED=1 with -gcflags="all=-N -l" for delve
make build-race                   # CGO_ENABLED=1 with -race
make build-all                    # cross-compile linux/darwin/windows × amd64/arm64
make test                         # go test ./... -race -coverprofile=coverage.out
make lint                         # golangci-lint run
make clean                        # rm -rf ./bin/*
```

Run a single test:
```shell
go test ./config/... -run TestMergeSetFlagNoSeparator -v
```

Version metadata (`Ver`, `Branch`, `GitCommitHash`, `BuildTimestamp`, `Platform`) is injected at build time via `-ldflags -X` into `misc/consts.go`. Don't hard-code these — let the Makefile populate them. Pass `VER=1.2.3` to `make` to set the version string.

CI (`.github/workflows/`): `test.yml` runs `make test` on PRs to master, `linter.yml` runs golangci-lint, `release.yml` triggers on tags and runs `make build-all` + GoReleaser (Homebrew tap), `helm.yml` publishes the chart on tags.

## High-level architecture

Entry point is `kubeshark.go` → `cmd.Execute()`. The CLI is built on **cobra**; each top-level command is one file under `cmd/`:

| Command | File | What it does |
|---|---|---|
| `tap [POD_REGEX]` | `cmd/tap.go` + `cmd/tapRunner.go` | Default flow. Installs Helm release, watches hub/front pods, sets up proxy, opens browser. |
| `tap --pcap <tar.gz>` | `cmd/tapPcapRunner.go` | Alternate flow. Runs hub/front/worker as **local Docker containers** instead of in K8s. |
| `proxy` | `cmd/proxy.go` + `cmd/proxyRunner.go` | Re-establish proxy/port-forward to an already-installed release. |
| `clean` | `cmd/clean.go` | `helm uninstall` the release. |
| `console` | `cmd/console.go` | WebSocket client to `/scripts/logs` on the hub. |
| `scripts` | `cmd/scripts.go` | `fsnotify` watch of `scripting.source` directory; POST/PUT/DELETE scripts to hub. |
| `pro` | `cmd/pro.go` | License acquisition; spins up a local gin server on port 5252 to receive the license callback. |
| `logs` | `cmd/logs.go` | Dumps cluster pod logs to a zip via `misc/fsUtils`. |
| `config` | `cmd/config.go` | Print/regenerate `~/.kubeshark/config.yaml`. |
| `version` | `cmd/version.go` | Print `misc.Ver`. |

Three subsystems do most of the work. Read these together to understand the tap flow:

1. **`kubernetes/`** — wraps `client-go`. `Provider` (`provider.go`) holds the clientset and rest config. `watch.go` + `podWatchHelper.go` + `eventWatchHelper.go` provide a generic restart-debounced watch loop. `proxy.go` implements two ways to reach the in-cluster services: a local HTTP proxy (`StartProxy`) that rewrites paths to `/api/v1/namespaces/<ns>/services/<svc>:80/proxy`, and `NewPortForward` as fallback. Resource names live in `kubernetes/consts.go` (everything is prefixed `kubeshark-`).

2. **`kubernetes/helm/helm.go`** — installs the Helm chart from the OCI/HTTP repo `https://helm.kubeshark.co`. Critically, **it serializes the entire `config.Config` struct as the Helm values** (JSON marshal → unmarshal into `map[string]interface{}` → pass to `client.Run`). This means the YAML tags on `ConfigStruct` directly become Helm value paths.

3. **`internal/connect/hub.go`** — HTTP client that talks to the deployed `kubeshark-hub` pod once the proxy/port-forward is up. POSTs the pod regex, license, scripting env, and individual scripts. All calls retry on transient errors with `DefaultSleep` between attempts.

The `tap()` function in `cmd/tapRunner.go` ties it all together: validate, set Docker registry/tag (`docker/images.go`), build a `Provider`, list target pods, `helm.NewHelmDefault().Install()`, then launch goroutines `watchHubPod` / `watchFrontPod` / `watchHubEvents`. Once both hub and front pods are `PodRunning`, `postHubStarted` and `postFrontStarted` start the proxies and (re-)post regex/scripts/license to the hub. `utils.WaitForTermination` blocks until SIGINT.

## Configuration system (important when touching `config/`)

There is a single `config.Config` (type `ConfigStruct` in `config/configStruct.go`). It nests sub-configs from `config/configStructs/` (`TapConfig`, `LogsConfig`, `ScriptingConfig`, `ConfigConfig`). Defaults are populated by the `creasty/defaults` library reading `default:"..."` struct tags.

Three things merge into `Config` in this order (later wins):
1. `default:` struct tags
2. YAML file: first `./kubeshark.yaml` in cwd, then `~/.kubeshark/config.yaml`
3. CLI flags

The flag→config mapping in `config/config.go` (`initFlag`, `mergeFlag`) is **reflection-based and non-obvious**:
- A flag like `--proxy-hub-port` on the `tap` command splits into the path `["tap", "proxy", "hub", "port"]` (command name first, then flag name split on `-`) and walks `ConfigStruct` matching `yaml:"..."` tags at each level.
- The generic `--set` flag takes dotted paths, e.g. `--set tap.proxy.hub.port=8888` or `--set scripting.source=./scripts`.
- Several commands (`clean`, `console`, `pro`, `proxy`, `scripts`) have their `cmdName` rewritten to `"tap"` inside `InitConfig` so their flags resolve against the `tap` subtree. Preserve this behavior when adding a new command that reuses tap flags.
- Fields tagged `readonly:""` (e.g. `Config.Regenerate`) are zeroed out by `setZeroForReadonlyFields` when generating the default config template — they're CLI-only and shouldn't be written to disk.

When adding a new config field: pick a `yaml` tag, add a matching `default` tag, mirror it in `helm-chart/values.yaml` if it should be passed through to the chart (the chart receives the JSON-marshalled `Config` as values).

## Conventions

- **Logging**: use `github.com/rs/zerolog/log` with structured fields (`.Str()`, `.Int()`, `.Err()`). The console writer is configured in `kubeshark.go`.
- **Constants for resource names**: never hard-code `"kubeshark-hub"` etc. — use the constants in `kubernetes/consts.go`. For the program name itself (`"kubeshark"` / `"Kubeshark"`), use `misc.Program` / `misc.Software`.
- **Coverage must not decrease** (per `CONTRIBUTING.md`). REST APIs follow the Google JSON Style Guide.
- The `bin/` directory is the build output and is gitignored — don't commit it.
