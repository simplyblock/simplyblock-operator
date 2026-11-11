#!/bin/sh

# VM logs
rm -rf /tmp/neon-logs.fifo; mkfifo -m 0666 /tmp/neon-logs.fifo

# Services logs
rm -rf /tmp/service-postgrest.fifo; mkfifo -m 0666 /tmp/service-postgrest.fifo
rm -rf /tmp/service-storage.fifo; mkfifo -m 0666 /tmp/service-storage.fifo
rm -rf /tmp/service-pgmeta.fifo; mkfifo -m 0666 /tmp/service-pgmeta.fifo
rm -rf /tmp/service-pgexporter.fifo; mkfifo -m 0666 /tmp/service-pgexporter.fifo
rm -rf /tmp/service-pgbouncer-exporter.fifo; mkfifo -m 0666 /tmp/service-pgbouncer-exporter.fifo
rm -rf /tmp/service-pgbouncer.fifo; mkfifo -m 0666 /tmp/service-pgbouncer.fifo
rm -rf /tmp/service-postgresql.fifo; mkfifo -m 0666 /tmp/service-postgresql.fifo
rm -rf /tmp/service-storage.fifo; mkfifo -m 0666 /tmp/service-storage.fifo
