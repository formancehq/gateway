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

  # quote() shell-escapes the interpolated values so a crafted TAG/COMPONENT
  # (e.g. a malicious branch name) cannot inject shell commands.
  TAG={{quote(TAG)}}
  COMPONENT={{quote(COMPONENT)}}

  if [ -z "$TAG" ]; then
    echo "Error: TAG is required"
    exit 1
  fi

  if [ -z "${AUTH_TOKEN:-}" ]; then
    echo "Error: AUTH_TOKEN environment variable is not set"
    exit 1
  fi

  # Pass the token via the environment so it never appears in the process
  # arguments (argocd reads ARGOCD_AUTH_TOKEN natively).
  export ARGOCD_AUTH_TOKEN="$AUTH_TOKEN"
  APPLICATION="staging-eu-west-1-hosting-regions"
  SERVER="argocd.internal.formance.cloud"

  echo "Updating $COMPONENT tag to $TAG on $APPLICATION..."
  argocd --server="$SERVER" --grpc-web app set "$APPLICATION" \
    --parameter "versions.files.default.$COMPONENT=$TAG"

  echo "Syncing application $APPLICATION..."
  argocd --server="$SERVER" --grpc-web app sync "$APPLICATION"

  echo "Successfully deployed $COMPONENT tag $TAG to staging"
