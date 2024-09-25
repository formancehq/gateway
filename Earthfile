VERSION 0.8

IMPORT github.com/formancehq/earthly:tags/v0.16.1 AS core



FROM core+base-image

sources:
    WORKDIR /src
    COPY go.* .
    COPY --dir internal .
    COPY --dir pkg .
    COPY main.go Caddyfile .
    SAVE ARTIFACT /src

compile:
    FROM core+builder-image
    COPY (+sources/*) /src
    WORKDIR /src
    ARG VERSION=latest
    DO --pass-args core+GO_COMPILE --VERSION=$VERSION

build-image:
    FROM core+final-image
    ENTRYPOINT ["/usr/bin/caddy"]
    CMD ["run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
    COPY Caddyfile /etc/caddy/Caddyfile
    COPY (+compile/main) /usr/bin/caddy
    ARG REPOSITORY=ghcr.io
    ARG tag=latest
    DO core+SAVE_IMAGE --COMPONENT=gateway --REPOSITORY=${REPOSITORY} --TAG=$tag

deploy:
    COPY (+sources/*) /src
    LET tag=$(tar cf - /src | sha1sum | awk '{print $1}')
    WAIT
        BUILD --pass-args +build-image --tag=$tag
    END
    FROM --pass-args core+vcluster-deployer-image
    RUN kubectl patch Versions.formance.com default -p "{\"spec\":{\"gateway\": \"${tag}\"}}" --type=merge

deploy-staging:
    BUILD --pass-args core+deployer-module --MODULE=gateway
lint:
    FROM core+builder-image
    COPY (+sources/*) /src
    COPY --pass-args +tidy/go.* .
    WORKDIR /src
    DO --pass-args core+GO_LINT
    SAVE ARTIFACT internal AS LOCAL internal
    SAVE ARTIFACT pkg AS LOCAL pkg
    SAVE ARTIFACT main.go AS LOCAL main.go

tests:
    FROM core+builder-image
    COPY (+sources/*) /src
    WORKDIR /src
    DO --pass-args core+GO_TESTS

pre-commit:
    WAIT
      BUILD --pass-args +tidy
    END
    BUILD --pass-args +lint

openapi:
    COPY ./openapi.yaml .
    SAVE ARTIFACT ./openapi.yaml

tidy:
    FROM core+builder-image
    COPY --pass-args (+sources/src) /src
    WORKDIR /src
    DO --pass-args core+GO_TIDY

release:
    FROM core+builder-image
    ARG mode=local
    COPY --dir . /src
    DO core+GORELEASER --mode=$mode