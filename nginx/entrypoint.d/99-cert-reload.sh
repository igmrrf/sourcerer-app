#!/bin/sh
# Reload nginx every 6h so a certificate renewed by the certbot sidecar takes
# effect without restarting the container: `certbot renew` rewrites the files
# on disk but a running nginx keeps serving the certificate it loaded at start.
#
# This lives in /docker-entrypoint.d rather than as a `command:` override,
# because the nginx image only runs its entrypoint scripts — envsubst on
# /etc/nginx/templates included — when the command is literally `nginx`.
set -e

RELOAD_INTERVAL="${NGINX_RELOAD_INTERVAL:-6h}"

(
    while true; do
        sleep "$RELOAD_INTERVAL"
        nginx -s reload 2>&1 || echo "cert-reload: nginx reload failed, will retry"
    done
) &

echo "cert-reload: nginx will reload every $RELOAD_INTERVAL"
