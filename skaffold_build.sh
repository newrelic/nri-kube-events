#!/bin/bash -e

MAKE_TARGET="docker"
if [ "$PUSH_IMAGE" = true ]; then
  MAKE_TARGET="docker-push"
fi

pushd "$BUILD_CONTEXT" > /dev/null
DOCKER_IMAGE_NAME="$IMAGE"  make "$MAKE_TARGET"
