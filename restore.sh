#!/bin/bash

if [[ $# -ne 1 ]]; then
    echo "usage: ./restore.sh <restore-path>" >&2
    exit 1
fi

readonly RESTORE_PATH="$1"

exec 50</proc/$$/ns/net

criu restore -D "$RESTORE_PATH" \
     --shell-job \
     --inherit-fd 'fd[50]:extRootNetNS'

rm -r "$RESTORE_PATH"
