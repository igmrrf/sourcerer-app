#!/usr/bin/env bash
# Builds the Kotlin extractor jar that the Go backend shells out to.
#
# The backend refuses to start without a usable jar, so this must run before
# `docker compose up`. Gradle runs inside a container to pin the toolchain
# (Gradle 4.10.3 / JDK 8) that this build script requires.
set -euo pipefail

cd "$(dirname "$0")"

JAR_PATH="cli/build/libs/sourcerer-app.jar"

if command -v docker >/dev/null 2>&1; then
  docker compose --profile build run --rm cli-build
elif command -v gradle >/dev/null 2>&1; then
  echo "docker not found; falling back to the host gradle (must be 4.x on JDK 8)"
  (cd cli && gradle --no-daemon assemble)
else
  echo "error: need either docker or gradle on PATH to build the CLI jar" >&2
  exit 1
fi

if [ ! -s "$JAR_PATH" ]; then
  echo "error: build finished but $JAR_PATH is missing or empty" >&2
  exit 1
fi

echo "Built $JAR_PATH ($(wc -c <"$JAR_PATH" | tr -d ' ') bytes)"
