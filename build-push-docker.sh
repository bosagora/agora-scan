#!/bin/bash
set -e

TAG_NAME="agora_v2.0.2-$(git rev-parse --short=12 HEAD)"
echo "TAG_NAME=$TAG_NAME"

IMAGE="bosagora/agora-scan"

docker buildx build --platform=linux/amd64,linux/arm64 \
  -t "${IMAGE}:${TAG_NAME}" \
  -t "${IMAGE}:latest" \
  -f Dockerfile \
  --push \
  .

echo "Pushed ${IMAGE}:${TAG_NAME} and ${IMAGE}:latest"
