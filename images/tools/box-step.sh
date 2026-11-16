#!/bin/sh
# box-step runs one converge or save step. Its caller holds stdin open while it waits, so stdin
# closing means the caller gave up or died, and the step ends with everything it started.
PATH=/opt/outpost/bin:$PATH
export PATH
exec 3<&0
setsid "$@" </dev/null 3<&- &
step=$!
{
  while read -r _; do :; done
  kill -TERM -- "-$step" 2>/dev/null
  sleep 5
  kill -KILL -- "-$step" 2>/dev/null
} <&3 >/dev/null 2>&1 &
watch=$!
exec 3<&-
wait "$step"
code=$?
kill "$watch" 2>/dev/null
exit "$code"
