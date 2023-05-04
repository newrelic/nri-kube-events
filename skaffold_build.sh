#!/bin/bash -e

export DOCKER_BUILDKIT=1
export DOCKER_IMAGE_NAME="${IMAGE}"

pushd "${BUILD_CONTEXT}" > /dev/null
make docker

if [[ "${PUSH_IMAGE}" = true ]]; then
  docker push "${DOCKER_IMAGE_NAME}"
fi
