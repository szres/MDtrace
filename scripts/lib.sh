# Shared by the scripts: where the runtime lives and how an event argument is resolved.
# Sourced, not run.

config_dir="$HOME/.config/mdtrace"

# resolve_event NAME|PATH prints the event directory, as `mdtrace -event` does: a bare name is
# looked up under ~/.config/mdtrace/events, anything with a slash is a path.
resolve_event() {
  local dir
  case "$1" in
    */*) dir="$1" ;;
    *) dir="$config_dir/events/$1" ;;
  esac
  [ -f "$dir/event.json" ] || { echo "no event.json in $dir" >&2; return 1; }
  (cd "$dir" && pwd)
}

# event_field DIR FIELD prints a string field of DIR/event.json, e.g. event_field "$event" .timezone
event_field() {
  command -v jq >/dev/null || { echo "jq is required (macOS 15+ ships it; or brew install jq)" >&2; return 1; }
  jq -er "$2" "$1/event.json"
}
