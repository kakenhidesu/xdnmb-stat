#!/bin/sh
set -eu
if [ "${RENEWED_LINEAGE:-}" = "/etc/letsencrypt/live/status.kusotech.club" ]; then
    /usr/sbin/nginx -t
    /bin/systemctl reload nginx
fi
