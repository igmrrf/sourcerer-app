#!/bin/bash

# Exit on any error
set -e

# Directory where Nginx expects the SSL certificates
SSL_DIR="./nginx/ssl"

echo "Generating local self-signed SSL certificates..."

# Create directory if it doesn't exist
mkdir -p "$SSL_DIR"

# Generate a self-signed certificate using modern standards
# - Elliptic Curve (prime256v1) instead of legacy RSA
# - Subject Alternative Name (SAN) so Chrome/Safari trust the CN
openssl req -x509 -nodes -days 365 \
  -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -sha256 \
  -keyout "$SSL_DIR/privkey.pem" \
  -out "$SSL_DIR/fullchain.pem" \
  -subj "/C=US/ST=Local/L=Local/O=Development/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1,IP:::1"

echo "Success! SSL certificates generated in $SSL_DIR/"
echo "  - $SSL_DIR/fullchain.pem"
echo "  - $SSL_DIR/privkey.pem"
echo ""
echo "You can now run: docker-compose up"
