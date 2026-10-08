#!/bin/sh
# Everything here runs in a network-disabled container. Only loopback exists.
set -eu
role=$1
compression=$2
agent=$3
case_timeout=$4
peer=
cleanup() {
    if [ -n "$peer" ]; then
        kill "$peer" 2>/dev/null || true
    fi
    for metric in memory.events memory.peak; do
        if [ -r "/sys/fs/cgroup/$metric" ]; then
            cat "/sys/fs/cgroup/$metric" > "/reports/$metric"
        fi
    done
}
trap cleanup EXIT INT TERM
python /harness/inventory.py /config/spec.json /reports/inventory.json
wstest -a > /reports/suite-version.txt 2>&1
python --version > /reports/python-version.txt 2>&1
if [ "$role" = inventory ]; then
    exit 0
fi
if [ "$role" = server ]; then
    /testee -mode server -compression="$compression" -case-timeout="$case_timeout" > /reports/testee.log 2>&1 &
    peer=$!
    /testee -mode wait
    wstest -m fuzzingclient -s /config/spec.json > /reports/autobahn.log 2>&1
else
    wstest -m fuzzingserver -s /config/spec.json -u 0 > /reports/autobahn.log 2>&1 &
    peer=$!
    /testee -mode client -compression="$compression" -agent="$agent" -case-timeout="$case_timeout" > /reports/testee.log 2>&1
fi
# A successful foreground command must not hide a crashed background server.
# Both roles require their background listener until the suite/client finishes.
if ! kill -0 "$peer" 2>/dev/null; then
    peer_status=0
    wait "$peer" || peer_status=$?
    echo "background peer exited unexpectedly (status $peer_status)" >&2
    exit 1
fi
