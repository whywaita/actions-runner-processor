#!/bin/sh
set -e

# Stop and disable service if running
if systemctl is-active --quiet actions-runner-processor.service 2>/dev/null; then
    systemctl stop actions-runner-processor.service
fi

if systemctl is-enabled --quiet actions-runner-processor.service 2>/dev/null; then
    systemctl disable actions-runner-processor.service
fi
