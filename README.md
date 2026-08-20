# Formance Gateway

The Formance Gateway is a reverse proxy built on [Caddy](https://caddyserver.com/).
The `main.go` entrypoint simply embeds Caddy (`caddycmd.Main`), loading the
standard Caddy modules together with the custom plugins defined in
[`pkg/plugins`](pkg/plugins). Its runtime behavior is entirely driven by the
[`Caddyfile`](Caddyfile).

As configured in the `Caddyfile`, the gateway listens on port `80` and:

- emits tracing spans named `gateway`;
- runs the custom `audit` plugin, which can publish audit events to Kafka and/or
  NATS (see [`pkg/plugins/audit.go`](pkg/plugins/audit.go));
- reverse-proxies requests under `/api/ledger*` to `127.0.0.1:3068`;
- serves a `/versions` endpoint via the custom `versions` plugin, which queries
  the configured sub-services and returns their version and health as JSON (see
  [`pkg/plugins/versions.go`](pkg/plugins/versions.go));
- responds with `502 Bad Gateway` to any other `/api/*` route.

## Prerequisites

- [Nix](https://nixos.org/) with flakes enabled.

The development environment is provided by [`flake.nix`](flake.nix). Its dev
shell bundles Go and the tooling used by this repository (`just`,
`golangci-lint`, `ginkgo`, `goreleaser-pro`, `mockgen`, and more), so no other
toolchain needs to be installed locally.

## Development

Enter the dev shell:

```sh
nix develop
```

Common tasks are exposed as [`just`](https://github.com/casey/just) recipes (see
[`Justfile`](Justfile)). Run `just` (or `just --list`) to list them.

Run the linter (`golangci-lint`):

```sh
nix develop -c just lint
```

Run the tests:

```sh
nix develop -c just tests
```

The `tests` recipe runs `go test` with the race detector and the `it` build tag
across the whole module, writing a coverage profile to `coverage.txt`.

## Repository layout

- [`Caddyfile`](Caddyfile) — Caddy configuration that defines the gateway's
  routes and behavior.
- [`openapi.yaml`](openapi.yaml) — OpenAPI specification for the Gateway API,
  documenting the `/versions` endpoint.
- [`pkg/`](pkg) — the custom Caddy plugins (`audit` and `versions`) loaded by
  `main.go`.
- [`test/`](test) — end-to-end tests (`test/e2e`, written with Ginkgo), built
  with the `it` tag.
