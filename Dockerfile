FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build

# Set by docker automatically
ARG TARGETOS TARGETARCH

# Need to be set manually
ARG TAG=dev
ARG COMMIT=unknown
ARG DATE="Sun Jan  1 00:00:00 UTC 2023"

ARG GOOS=$TARGETOS
ARG GOARCH=$TARGETARCH

WORKDIR /src

# We don't expect the go.mod/go.sum to change frequently.
# So splitting out the mod download helps create another layer
# that should cache well.
COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .
RUN go build \
    -ldflags="-X 'main.integrationVersion=${TAG}' -X 'main.gitCommit=${COMMIT}' -X 'main.buildDate=${DATE}'" \
    -o bin/nri-kube-events ./cmd/nri-kube-events

FROM alpine:3.19.1
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
