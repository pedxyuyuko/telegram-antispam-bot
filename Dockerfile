ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.24
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

FROM docker.io/library/alpine:${ALPINE_VERSION}
# ffmpeg decodes Telegram MP4 animations into their first and last frames.
RUN apk add --no-cache ca-certificates ffmpeg
COPY --from=build /out/antispam /antispam
COPY --from=build --chown=65532:65532 /out/runtime /runtime

USER 65532:65532
WORKDIR /runtime
STOPSIGNAL SIGTERM
ENTRYPOINT ["/antispam"]
CMD ["-config", "/runtime/config.json"]
