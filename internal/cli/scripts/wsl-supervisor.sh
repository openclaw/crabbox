# Invoked as bash -c with a bounded, fully materialized helper. User command
# and input bytes never enter argv or the helper's environment.
set -u
mode=$1 directory=$2 nonce=$3 command_size=$4 input_size=$5 idle_ms=$6 grace_ms=$7
caller_mask=${8:-$(umask)}
umask "$caller_mask" 2>/dev/null || exit 74
umask 077
trap '' HUP

identity() {
    local raw
    IFS= read -r raw <"/proc/$1/stat" 2>/dev/null || return 1
    raw=${raw##*) }
    set -- $raw
    [ "$1" != Z ] && [ "$1" != X ] || return 1
    printf '%s %s\n' "$3" "${20}"
}
read_guard() {
    read -r guard started group extra 2>/dev/null <"$directory/.owned" || return 1
    case $guard:$started:$group in *[!0-9:]*|:*|*::*|*:) return 1;; esac
    [ -z "${extra:-}" ]
}
valid_guard() {
    local current
    current=$(cat "$directory/.owned" 2>/dev/null) || return 1
    [ "$current" = "$guard $started $group" ] &&
        valid_process_guard
}
valid_process_guard() {
    [ "$(identity "$guard")" = "$group $started" ]
}
group_exists() {
    local diagnostic
    diagnostic=$(LC_ALL=C kill -0 -- "-$group" 2>&1) && return 0
    # Only ESRCH proves absence. Permission failure must retain evidence.
    [[ "$diagnostic" != *": (-$group) - No such process" ]]
}
cleanup_group() {
    # The in-memory tuple was proven before arming and remains authoritative
    # for stopping the group even if the on-disk evidence was later tampered.
    valid_process_guard || return 1
    kill -TERM -- "-$group" 2>/dev/null || return 1
    local ticks=$(((grace_ms + 99) / 100))
    while [ "$ticks" -gt 0 ]; do sleep .1; ticks=$((ticks - 1)); done
    valid_process_guard || return 1
    kill -KILL -- "-$group" 2>/dev/null || return 1
    wait "$leader" 2>/dev/null || :
    wait "$guard" 2>/dev/null || :
    ticks=$(((grace_ms + 99) / 100))
    while group_exists && [ "$ticks" -gt 0 ]; do sleep .1; ticks=$((ticks - 1)); done
    ! group_exists || return 1
}
remove_evidence() {
    [ "$(cat "$directory/.nonce" 2>/dev/null)" = "$nonce" ] || return 1
    if [ -n "${guard:-}" ]; then
        [ "$(cat "$directory/.owned" 2>/dev/null)" = "$guard $started $group" ] || return 1
    fi
    rm -rf -- "$directory"
}
preownership_failure() {
    remove_evidence || :
    exit 74
}

case $mode in
run)
    # setsid preserves the supervisor after Windows loses its launcher.
    exec setsid --wait bash -c "$CBX_HELPER" sh supervise "$directory" "$nonce" "$command_size" "$input_size" "$idle_ms" "$grace_ms" "$caller_mask"
    ;;
guard)
    trap '' TERM
    read -r group started < <(identity "$$") || exit 74
    printf '%s %s %s\n' "$$" "$started" "$group" >"$directory/.owned.tmp"
    mv "$directory/.owned.tmp" "$directory/.owned" || exit 74
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
    printf '%s\n' "$code" >"$directory/.result.tmp" &&
        mv "$directory/.result.tmp" "$directory/.result"
    exit "$code"
    ;;
watch)
    IFS= read -r -N 1 -u 3 ignored || :
    : >"$directory/.lost"
    exit 0
    ;;
cleanup)
    [ -e "$directory" ] || exit 0
    [ -d "$directory" ] && [ ! -L "$directory" ] &&
        [ "$(cat "$directory/.nonce" 2>/dev/null)" = "$nonce" ] || exit 74
    read -r supervisor supervisor_identity <"$directory/.supervisor" || exit 74
    [ "$(identity "$supervisor")" = "$supervisor_identity" ] || exit 74
    : >"$directory/.cancel"
    for ((i=0; i<100; i++)); do [ -e "$directory" ] || exit 0; sleep .1; done
    exit 74
    ;;
supervise) ;;
*) exit 74 ;;
esac

mkdir -m 700 -- "$directory" || exit 74
printf '%s' "$nonce" >"$directory/.nonce"
printf '%s %s\n' "$$" "$(identity "$$")" >"$directory/.supervisor" || preownership_failure
exec 3<&0
exec 0</dev/null
head -c "$((command_size + input_size))" <&3 >"$directory/frame" 3<&- &
receiver=$!
previous=-1
ticks=0
limit=$(((idle_ms + 99) / 100))
failed=0
while kill -0 "$receiver" 2>/dev/null; do
    count=$(wc -c <"$directory/frame")
    if [ "$count" -gt "$previous" ]; then ticks=0; else ticks=$((ticks + 1)); fi
    previous=$count
    if [ "$ticks" -ge "$limit" ] || [ -e "$directory/.cancel" ]; then
        failed=1
        kill "$receiver" 2>/dev/null || :
        break
    fi
    sleep .1
done
wait "$receiver" || failed=1
[ "$(wc -c <"$directory/frame")" = "$((command_size + input_size))" ] || failed=1
if [ "$failed" = 1 ]; then remove_evidence || :; exit 74; fi
head -c "$command_size" "$directory/frame" >"$directory/command" || preownership_failure
dd if="$directory/frame" of="$directory/input" bs=65536 skip="$command_size" count="$input_size" iflag=skip_bytes,count_bytes status=none || preownership_failure
rm "$directory/frame" || preownership_failure
bash -c "$CBX_HELPER" sh watch "$directory" "$nonce" 0 0 0 0 "$caller_mask" </dev/null &
watcher=$!
exec 3<&-
mkfifo -m 600 "$directory/guard-wait" || preownership_failure
exec 6<>"$directory/guard-wait"
set -m
bash -c "$CBX_HELPER" sh guard "$directory" "$nonce" 0 0 0 0 "$caller_mask" </dev/null |
    bash -c "$CBX_HELPER" sh workload "$directory" "$nonce" 0 0 0 0 "$caller_mask" <"$directory/input" 6>&- &
leader=$!
owned_guard=$(jobs -p %%)
set +m
exec 6>&-
for ((i=0; i<50; i++)); do
    [ -e "$directory/.owned" ] && break
    kill -0 "$owned_guard" 2>/dev/null || break
    sleep .1
done
if ! read_guard || [ "$guard" != "$owned_guard" ] || ! valid_guard; then
    # Before arming, only exact direct children may be stopped.
    kill -KILL "$owned_guard" "$leader" "$watcher" 2>/dev/null || :
    wait 2>/dev/null || :
    [ -e "$directory/.owned" ] || remove_evidence || :
    exit 74
fi
if [ ! -e "$directory/.lost" ] && [ ! -e "$directory/.cancel" ]; then : >"$directory/.armed"; fi
code=74
while valid_guard; do
    [ -e "$directory/.lost" ] || [ -e "$directory/.cancel" ] && break
    if [ -e "$directory/.result" ]; then read -r code <"$directory/.result"; break; fi
    kill -0 "$leader" 2>/dev/null || break
    sleep .1
done
kill "$watcher" 2>/dev/null || :
wait "$watcher" 2>/dev/null || :
if ! cleanup_group; then
    echo 'WSL2 command cleanup failed: group absence unconfirmed' >&2
    exit 74
fi
if ! remove_evidence; then
    echo 'WSL2 command cleanup failed: evidence ownership unconfirmed' >&2
    exit 74
fi
exit "$code"
