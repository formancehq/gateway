# Formance Gateway

The Formance Gateway is a reverse proxy built on [Caddy](https://caddyserver.com/).
It embeds the standard Caddy modules together with a set of custom plugins and is
configured through the [`Caddyfile`](./Caddyfile).

Based on the current `Caddyfile`, the gateway listens on port `80` and:

- adds a tracing span named `gateway` to incoming requests;
- runs the custom `audit` plugin, which can publish audit events to Kafka or NATS
  (NATS is enabled in the shipped configuration);
- reverse proxies requests matching `/api/ledger*` to the Ledger service on
  `127.0.0.1:3068`;
- serves a `/versions` endpoint through the custom `versions` plugin, which gathers
  and reports the version and health of the configured downstream components;
- responds `502 Bad Gateway` to any other `/api/*` route.

The binary itself (see [`main.go`](./main.go)) simply registers the standard Caddy
modules and the gateway plugins, then hands control to Caddy's command-line entry
point.

## Prerequisites

- [Nix](https://nixos.org/) with flakes enabled.

The development environment (Go toolchain, `just`, `golangci-lint`, and related
tooling) is provided by the dev shell defined in [`flake.nix`](./flake.nix), so no
other toolchain needs to be installed.

## Development

Enter the development shell:

```sh
nix develop
```

Run the linter:

```sh
nix develop -c just lint
```

Run the tests:

```sh
nix develop -c just tests
```

Run `just` (or `nix develop -c just`) with no arguments to list all available
recipes.

## Repository layout

- [`Caddyfile`](./Caddyfile) — Caddy configuration describing the gateway's
  routing, plugins, and reverse-proxy behavior.
- [`openapi.yaml`](./openapi.yaml) — OpenAPI specification for the Gateway API,
  including the `/versions` endpoint.
- [`pkg/`](./pkg) — Go source for the custom Caddy plugins (`audit` and
  `versions`).
- [`test/`](./test) — end-to-end tests for the gateway.
