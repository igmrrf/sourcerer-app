FROM golang:alpine AS builder

WORKDIR /app

# Copy go.mod and go.sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o sourcerer-backend .

# Final stage
FROM alpine:latest

# Install Java and Git
RUN apk add --no-cache openjdk17-jre git

WORKDIR /root/

# Copy the pre-built binary file from the previous stage
COPY --from=builder /app/sourcerer-backend .
# Also copy the schema for initialization
COPY --from=builder /app/schema.sql .

EXPOSE 8080

CMD ["./sourcerer-backend"]
