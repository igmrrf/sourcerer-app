# Build context is the repository root so the extractor jar can be baked in.
# Pinned tags so image contents cannot drift between builds.
FROM golang:1.25-alpine3.21 AS builder

WORKDIR /src

# Copy go.mod and go.sum files
COPY backend/go.mod backend/go.sum ./
RUN go mod download

# Copy the source code
COPY backend/ .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o sourcerer-backend .

# Final stage
FROM alpine:3.21

# openjdk17-jre runs the Kotlin extractor; git clones the repositories it reads.
RUN apk add --no-cache openjdk17-jre git ca-certificates

# The extractor executes against untrusted repository contents, so the runtime
# user is unprivileged and owns only its own working directory.
RUN addgroup -S sourcerer && adduser -S -G sourcerer -h /app sourcerer

WORKDIR /app

COPY --from=builder --chown=sourcerer:sourcerer /src/sourcerer-backend .

# The extractor jar is copied in, not bind-mounted: Docker silently creates a
# directory when a single-file bind-mount source is missing, which the backend
# then cannot distinguish from a real jar at first glance. Run ./build_cli.sh
# before building this image.
COPY --chown=sourcerer:sourcerer cli/build/libs/sourcerer-app.jar ./sourcerer-app.jar

# The extractor writes its config and classifier cache to a "data" directory
# beside the jar, so that path must be writable by the runtime user.
RUN mkdir -p /app/data && chown -R sourcerer:sourcerer /app

USER sourcerer

EXPOSE 8080

CMD ["./sourcerer-backend"]
