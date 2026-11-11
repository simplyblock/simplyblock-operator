#!/bin/sh
set -e

PITR=
if [ -n "$RECOVERY_TARGET_TIME" ]; then
    DONE=$(cat "$PGDATA/.recovery_done" 2>/dev/null || true)
    if [ "$DONE" != "$RECOVERY_TARGET_TIME" ]; then
        echo "$RECOVERY_TARGET_TIME" > "$PGDATA/.recovery_done"
        PITR="-c recovery_target_time='$RECOVERY_TARGET_TIME' -c snapshot_pitr_target_time='$RECOVERY_TARGET_TIME'"
    fi
fi
printf '%s' "$PITR" > "$RUNTIME_DIRECTORY/pitr-args"
