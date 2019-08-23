FROM golang:1.13.5 AS base-env

WORKDIR /src/

# TODO: vendor everything after first PR, disabled now to remove PR-file-clutter
COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .

FROM base-env AS build-env
ENV BUILD_TARGET=/src/nr-kube-events
ENV CGO_ENABLED=0
RUN make compile

FROM alpine:3.9 AS final

WORKDIR /app

RUN apk add --no-cache --upgrade \
        ca-certificates \
        musl=1.1.20-r5 \
    && addgroup -g 2000 nr-kube-events \
    && adduser -D -H -u 1000 -G nr-kube-events nr-kube-events
USER nr-kube-events

COPY --from=build-env /src/nr-kube-events .
EXPOSE 8080

# Enable custom attributes decoration in the infra SDK
ENV METADATA=true

ENTRYPOINT [ "./nr-kube-events" ]
CMD ["--config", "config.yaml", "-promaddr", "0.0.0.0:8080"]
