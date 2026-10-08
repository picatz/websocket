#!/bin/sh
# Everything here runs in a network-disabled container. Only loopback exists.
set -eu
role=$1
compression=$2
agent=$3
case_timeout=$4
cleanup() {
    kill "$peer" 2>/dev/null || true
    for metric in memory.events memory.peak; do
        if [ -r "/sys/fs/cgroup/$metric" ]; then
            cat "/sys/fs/cgroup/$metric" > "/reports/$metric"
        fi
    done
}
python /harness/inventory.py /config/spec.json /reports/inventory.json
wstest -a > /reports/suite-version.txt 2>&1
python --version > /reports/python-version.txt 2>&1
if [ "$role" = server ]; then
    /testee -mode server -compression="$compression" -case-timeout="$case_timeout" > /reports/testee.log 2>&1 &
    peer=$!
    trap cleanup EXIT INT TERM
    /testee -mode wait
    wstest -m fuzzingclient -s /config/spec.json > /reports/autobahn.log 2>&1
else
    wstest -m fuzzingserver -s /config/spec.json -u 0 > /reports/autobahn.log 2>&1 &
    peer=$!
    trap cleanup EXIT INT TERM
    /testee -mode client -compression="$compression" -agent="$agent" -case-timeout="$case_timeout" > /reports/testee.log 2>&1
fi
