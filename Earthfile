VERSION 0.8

ARG core=github.com/formancehq/earthly:main
IMPORT $core AS core

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
    BUILD --pass-args core+deploy-staging

openapi:
    COPY ./openapi.yaml .
    SAVE ARTIFACT ./openapi.yaml

