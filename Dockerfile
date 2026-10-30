ARG GO_VERSION=1.26
FROM docker.io/library/golang:${GO_VERSION}-alpine AS build

WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY examples ./examples
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/antispam ./cmd/antispam \
    && mkdir -p /out/runtime

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/antispam /antispam
COPY --from=build --chown=65532:65532 /out/runtime /runtime

USER 65532:65532
WORKDIR /runtime
STOPSIGNAL SIGTERM
ENTRYPOINT ["/antispam"]
CMD ["-config", "/runtime/config.json"]
