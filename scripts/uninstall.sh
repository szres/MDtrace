#!/bin/bash
# Stop and remove the launchd agent. Collected data in ~/.config/mdtrace is kept.
set -euo pipefail

label="io.github.szres.mdtrace"
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
/bin/rm -f "$HOME/Library/LaunchAgents/$label.plist"
echo "agent removed; data kept in $HOME/.config/mdtrace"
