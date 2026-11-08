#!/bin/bash
# Run by the io.github.szres.mdtrace.stop agent: end collection, send the final summary with the
# Mission Day replay, keep the data, then remove itself.
set -uo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

uid="$(id -u)"
dir="$HOME/.config/mdtrace"
agents="$HOME/Library/LaunchAgents"
stamp() { date '+%Y/%m/%d %H:%M:%S'; }

echo "$(stamp) scheduled stop: collection ended; data kept in $dir"
launchctl bootout "gui/$uid/io.github.szres.mdtrace" 2>/dev/null || true
# A channel paused for the regular updates still gets the final summary; with the collector
# unloaded, nothing else can post there.
env_file="$dir/telegram.env"
if grep -qE '^# Paused [^:]*: TELEGRAM_CHANNEL_ID=' "$env_file"; then
  sed -i '' -E 's/^# Paused [^:]*: (TELEGRAM_CHANNEL_ID=)/\1/' "$env_file"
  echo "$(stamp) channel re-enabled for the final summary"
fi
echo "$(stamp) sending final summary"
if "$dir/bin/mdtrace" final -dir "$dir" -tz Asia/Shanghai -charts "$dir/charts.R"; then
  echo "$(stamp) final summary sent; charts saved in $dir/final"
else
  echo "$(stamp) final summary failed; rerun: $dir/bin/mdtrace final -dir $dir -tz Asia/Shanghai -charts $dir/charts.R"
fi
/bin/rm -f "$agents/io.github.szres.mdtrace.plist" "$agents/io.github.szres.mdtrace.stop.plist"
# Last: unloading this agent ends this script.
launchctl bootout "gui/$uid/io.github.szres.mdtrace.stop" 2>/dev/null || true
