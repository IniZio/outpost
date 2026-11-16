#!/bin/sh
sleep 0.5
printf '\033]7501;state=idle\007'
while read -r word; do
  printf '\033]7501;state=%s\007' "$word"
  [ "$word" = quit ] && exit 0
done
