#!/bin/sh
echo 0 >/proc/self/oom_score_adj 2>/dev/null
exec "$@"
