# Invoked as bash -c with a bounded, fully materialized helper. User command
# and input bytes never enter argv or the helper's environment.
set -u
mode=$1 directory=$2 nonce=$3 command_size=$4 payload_size=$5 idle_ms=$6 grace_ms=$7
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
@GUARDED_GROUP_FUNCTIONS@
remove_evidence() {
    [ "$(cat "$directory/.nonce" 2>/dev/null)" = "$nonce" ] || return 1
    rm -rf -- "$directory"
}

@FUNCTIONAL_PRELUDE@case $mode in
run)
    # setsid and ignored HUP preserve the supervisor after Windows loses its
    # launcher. It directly parents both members of the guarded pipeline.
    exec setsid --wait bash -c "$CBX_HELPER" sh supervise "$directory" "$nonce" "$command_size" "$payload_size" "$idle_ms" "$grace_ms" "$caller_mask"
    ;;
@GUARDED_MEMBERS@
watch)
    IFS= read -r -N 1 -u 3 ignored || :
    : >"$directory/.lost"
    exit 0
    ;;
cleanup)
    [ -e "$directory" ] || exit 0
    [ -d "$directory" ] && [ ! -L "$directory" ] &&
        [ "$(cat "$directory/.nonce" 2>/dev/null)" = "$nonce" ] || exit 74
@FUNCTIONAL_CLEANUP@    read -r supervisor supervisor_identity <"$directory/.supervisor" || exit 74
    if [ "$(identity "$supervisor")" = "$supervisor_identity" ]; then
        : >"$directory/.cancel"
        for ((i=0; i<100; i++)); do
@FUNCTIONAL_CLEANUP_POLL@            [ -e "$directory" ] || exit 0
            sleep .1
        done
        exit 74
    fi
    # Only the original supervisor holds the publication-time guard identity
    # and owns its child reaping. A pathname record cannot replace that authority.
    exit 74
    ;;
supervise) ;;
*) exit 74;;
esac

mkdir -m 700 -- "$directory" || exit 74
printf '%s' "$nonce" >"$directory/.nonce"
@FUNCTIONAL_SCRATCH@printf '%s %s\n' "$$" "$(identity "$$")" >"$directory/.supervisor"
exec 3<&0
exec 0</dev/null
head -c "$((command_size + payload_size))" <&3 >"$directory/frame" 3<&- &
receiver=$!
previous=-1
ticks=0
limit=$(((idle_ms + 99) / 100))
failed=0
while kill -0 "$receiver" 2>/dev/null; do
    count=$(wc -c <"$directory/frame")
    if [ "$count" -gt "$previous" ]; then ticks=0; else ticks=$((ticks + 1)); fi
    previous=$count
    if [ "$ticks" -ge "$limit" ] || [ -e "$directory/.cancel" ]; then failed=1; kill "$receiver" 2>/dev/null || :; break; fi
    sleep .1
done
wait "$receiver" || failed=1
[ "$(wc -c <"$directory/frame")" = "$((command_size + payload_size))" ] || failed=1
if [ "$failed" = 1 ]; then remove_evidence || :; exit 74; fi
head -c "$command_size" "$directory/frame" >"$directory/command" || exit 74
dd if="$directory/frame" of="$directory/input" bs=65536 skip="$command_size" count="$payload_size" iflag=skip_bytes,count_bytes status=none || exit 74
rm "$directory/frame"
bash -c "$CBX_HELPER" sh watch "$directory" "$nonce" 0 0 0 0 "$caller_mask" </dev/null &
watcher=$!
exec 3<&-
@GUARDED_WORKLOAD@
