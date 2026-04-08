#!/bin/bash

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BINARY="$ROOT_DIR/bin/gocache-server-test"

cleanup() {
    rm -f "$BINARY"
    rm -f "$ROOT_DIR/snapshot.dat"
}
trap cleanup EXIT

echo "Building server..."
go build -o "$BINARY" "$ROOT_DIR/cmd/server"

echo "Starting GoCache server..."
"$BINARY" &
SERVER_PID=$!
echo "Server PID: $SERVER_PID"
sleep 2

echo "Connecting and sending SET command..."
echo -e '*3\r\n$3\r\nSET\r\n$4\r\ntest\r\n$5\r\nvalue\r\n' | nc localhost 6379

echo "Sending SIGTERM to trigger graceful shutdown..."
kill -TERM $SERVER_PID

echo "Waiting for server to shut down..."
wait $SERVER_PID

echo "Server shut down cleanly (exit code: $?)"
