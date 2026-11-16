#!/bin/sh
for prompt; do :; done
printf '\033]7501;state=working\007'
echo "fake claude was asked: $prompt"
mkdir -p /root/.outpost
printf '{"type":"assistant","message":{"model":"fake","content":[{"type":"text","text":"fake claude finished"}]}}\n' >/root/transcript.jsonl
printf '{"transcript_path":"/root/transcript.jsonl"}' >/root/.outpost/session.json
sleep 0.5
printf '\033]7501;state=idle\007'
while read -r line; do
  echo "fake claude got: $line"
  printf '\033]7501;state=idle\007'
done
