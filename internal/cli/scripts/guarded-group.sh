read_guard() {
    read -r guard started group extra 2>/dev/null <"$directory/.crabbox-owned" || return 1
    case $guard:$started:$group in *[!0-9:]*|:*|*::*|*:) return 1;; esac
    [ -z "${extra:-}" ]
}
valid_guard() {
    local current
    current=$(cat "$directory/.crabbox-owned" 2>/dev/null) || return 1
    [ "$current" = "$guard $started $group" ] &&
        [ "$(identity "$guard")" = "$group $started" ]
}
group_exists() {
    local diagnostic
    diagnostic=$(LC_ALL=C kill -0 -- "-$group" 2>&1) && return 0
    # Only ESRCH proves absence. Permission failure must retain evidence too.
    [[ "$diagnostic" != *": (-$group) - No such process" ]]
}
cleanup_group() {
    # Keep the witness alive through TERM. Revalidate both its immutable
    # process identity and the published record before the final KILL.
    valid_guard || return 1
    kill -TERM -- "-$group" 2>/dev/null || return 1
    local ticks=$(((grace_ms + 99) / 100))
    while [ "$ticks" -gt 0 ]; do sleep .1; ticks=$((ticks - 1)); done
    valid_guard || return 1
    kill -KILL -- "-$group" 2>/dev/null || return 1
    wait "$leader" 2>/dev/null || :
    wait "$guard" 2>/dev/null || :
    ticks=$(((grace_ms + 99) / 100))
    while group_exists && [ "$ticks" -gt 0 ]; do sleep .1; ticks=$((ticks - 1)); done
    ! group_exists || return 1
    [ "$(cat "$directory/.crabbox-owned" 2>/dev/null)" = "$guard $started $group" ]
}
