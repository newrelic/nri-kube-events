FROM alpine:3.13

# Set by docker automatically
# If building with `docker build`, make sure to set GOOS/GOARCH explicitly when calling make:
# `make compile GOOS=something GOARCH=something`
# Otherwise the makefile will not append them to the binary name and docker build wil fail.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

RUN apk add --no-cache --upgrade \
    tini ca-certificates \
    && addgroup -g 2000 nri-kube-events \
    && adduser -D -H -u 1000 -G nri-kube-events nri-kube-events
EXPOSE 8080

ADD --chmod=755 bin/nri-kube-events-${TARGETOS}-${TARGETARCH} ./
RUN mv nri-kube-events-${TARGETOS}-${TARGETARCH} nri-kube-events

USER nri-kube-events

# Enable decorating events from NRI_ env vars, such as `foo: bar` from NRI_FOO=bar.
# https://github.com/newrelic/infra-integrations-sdk/blob/master/docs/toolset/args.md#bundled-arguments
ENV METADATA=true

ENTRYPOINT ["/sbin/tini", "--", "./nri-kube-events"]
CMD ["--config", "config.yaml", "-promaddr", "0.0.0.0:8080"]
