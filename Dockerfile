FROM --platform=$BUILDPLATFORM golang:1.19-alpine AS build
WORKDIR /src
COPY . .

# Set by docker automatically
ARG TARGETOS TARGETARCH

# Need to be set manually
ARG TAG=dev
ARG COMMIT=unknown
ARG DATE="Sun Jan  1 00:00:00 UTC 2023"

ARG GOOS=$TARGETOS
ARG GOARCH=$TARGETARCH

RUN go build \
    -ldflags="-X 'main.integrationVersion=${TAG}' -X 'main.gitCommit=${COMMIT}' -X 'main.buildDate=${DATE}'" \
    -o bin/nri-kube-events ./cmd/nri-kube-events

FROM alpine:3.17.3
WORKDIR /app

RUN apk add --no-cache --upgrade \
    tini ca-certificates \
    && addgroup -g 2000 nri-kube-events \
    && adduser -D -H -u 1000 -G nri-kube-events nri-kube-events
EXPOSE 8080

USER nri-kube-events

COPY --chown=nri-kube-events:nri-kube-events --from=build /src/bin/nri-kube-events ./

# Enable custom attributes decoration in the infra SDK
ENV METADATA=true

ENTRYPOINT ["/sbin/tini", "--", "./nri-kube-events"]
CMD ["--config", "config.yaml", "-promaddr", "0.0.0.0:8080"]
