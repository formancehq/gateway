set dotenv-load

default:
  @just --list

pre-commit: tidy lint 
pc: pre-commit

lint:
  @golangci-lint run --fix --build-tags it --timeout 5m

tidy:
  @go mod tidy

[group('test')]
tests:
  @go test -race -covermode=atomic \
    -coverprofile coverage.txt \
    -tags it \
    ./...

[group('releases')]
release-local:
    @goreleaser release --nightly --skip=publish --clean

[group('releases')]
release-ci:
    @goreleaser release --nightly --clean

[group('releases')]
release:
    @goreleaser release --clean

[group('deploy')]
deploy-staging TAG='' COMPONENT='gateway':
  #!/usr/bin/env bash
  set -euo pipefail

  if [ -z "{{TAG}}" ]; then
    echo "Error: TAG is required"
    exit 1
  fi

  if [ -z "${AUTH_TOKEN:-}" ]; then
    echo "Error: AUTH_TOKEN environment variable is not set"
    exit 1
  fi

  APPLICATION="staging-eu-west-1-hosting-regions"
  SERVER="argocd.internal.formance.cloud"

  echo "Updating {{COMPONENT}} tag to {{TAG}} on $APPLICATION..."
  argocd --auth-token="$AUTH_TOKEN" --server="$SERVER" --grpc-web app set "$APPLICATION" \
    --parameter versions.files.default.{{COMPONENT}}="{{TAG}}"

  echo "Syncing application $APPLICATION..."
  argocd --auth-token="$AUTH_TOKEN" --server="$SERVER" --grpc-web app sync "$APPLICATION"

  echo "Successfully deployed {{COMPONENT}} tag {{TAG}} to staging"
