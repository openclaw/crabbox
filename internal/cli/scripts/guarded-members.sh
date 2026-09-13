guard)
    trap '' TERM
    read -r group started < <(identity "$$") || exit 74
    printf '%s %s %s\n' "$$" "$started" "$group" >"$directory/.crabbox-owned.tmp"
    mv "$directory/.crabbox-owned.tmp" "$directory/.crabbox-owned" || exit 74
    # A private FIFO supplies a blocking builtin, without a sleep child that
    # could become an unreapable in-group zombie when the guard is killed.
    while :; do IFS= read -r -t 1 -u 6 ignored || :; done
    ;;
workload)
    trap ':' TERM
    while [ ! -e "$directory/.armed" ]; do sleep .1; done
    (umask "$caller_mask"; exec bash "$directory/command" <"$directory/input") &
    child=$!
    while :; do
        code=0
        wait "$child" || code=$?
        kill -0 "$child" 2>/dev/null || break
    done
    # The supervisor must never observe a result before its status is complete.
    printf '%s\n' "$code" >"$directory/.result.tmp" &&
        mv -- "$directory/.result.tmp" "$directory/.result" || exit 74
    exit "$code"
    ;;
