#!/bin/bash
set -e

# Run the database schema migration
echo "Initializing database schema..."
psql -U postgres -d postgres -f schema.sql

# Run the Go server
echo "Starting backend server..."
go run main.go api.go
