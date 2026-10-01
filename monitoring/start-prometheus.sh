#!/bin/sh
set -eu
umask 077
case ${MONITORING_METRICS_TOKEN:?MONITORING_METRICS_TOKEN is required} in
  *[!a-zA-Z0-9_-]*) printf 'Invalid metrics token.\n' >&2; exit 1 ;;
esac
[ ${#MONITORING_METRICS_TOKEN} -ge 32 ] || { printf 'Metrics token is too short.\n' >&2; exit 1; }
printf '%s' "$MONITORING_METRICS_TOKEN" > /tmp/metrics-token
unset MONITORING_METRICS_TOKEN
exec /bin/prometheus --config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/prometheus --storage.tsdb.retention.time=7d --storage.tsdb.retention.size=256MB --web.listen-address=0.0.0.0:9090
