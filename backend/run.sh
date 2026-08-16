#!/bin/bash
set -e

# Run the Go server (schema migration runs automatically on Go startup)
echo "Starting backend server (development mode)..."
export ENV="development"
go run .

