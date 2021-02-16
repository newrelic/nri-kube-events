FROM golang:1.13.5 AS base-env

WORKDIR /src/

COPY go.mod go.sum ./
RUN go mod download

COPY . .

FROM base-env AS build-env
ARG TARGETOS
ARG TARGETARCH
ENV BUILD_TARGET=/src/nri-kube-events
ENV CGO_ENABLED=0
RUN make compile GOOS=${TARGETOS} GOARCH=${TARGETARCH}

FROM alpine:3.13 AS final

WORKDIR /app

RUN apk add --no-cache --upgrade \
    ca-certificates \
    && addgroup -g 2000 nri-kube-events \
    && adduser -D -H -u 1000 -G nri-kube-events nri-kube-events
USER nri-kube-events

COPY --from=build-env /src/nri-kube-events .
EXPOSE 8080

# Enable custom attributes decoration in the infra SDK
ENV METADATA=true

ENTRYPOINT [ "./nri-kube-events" ]
CMD ["--config", "config.yaml", "-promaddr", "0.0.0.0:8080"]
