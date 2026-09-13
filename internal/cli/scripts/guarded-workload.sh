mkfifo -m 600 "$directory/guard-wait" || exit 74
exec 6<>"$directory/guard-wait"
set -m
bash -c "$CBX_HELPER" sh guard "$directory" "$nonce" 0 0 0 0 "$caller_mask" </dev/null |
    bash -c "$CBX_HELPER" sh workload "$directory" "$nonce" 0 0 0 0 "$caller_mask" <"$directory/input" 6>&- &
leader=$!
owned_guard=$(jobs -p %%)
set +m
exec 6>&-
for ((i=0; i<50; i++)); do
    [ -e "$directory/.crabbox-owned" ] && break
    kill -0 "$owned_guard" 2>/dev/null || break
    sleep .1
done
if ! read_guard || [ "$guard" != "$owned_guard" ] || ! valid_guard; then
    # Publication failed before arming: only exact direct children can be
    # stopped here, and the unarmed diagnostic state remains.
    kill -KILL "$owned_guard" "$leader" "$watcher" 2>/dev/null || :
    wait 2>/dev/null || :
    exit 74
fi
@WORKLOAD_INIT@if [ ! -e "$directory/.lost" ] && [ ! -e "$directory/.cancel" ]; then : >"$directory/.armed"; fi
code=74
while valid_guard; do
@WORKLOAD_TICK@    [ -e "$directory/.lost" ] || [ -e "$directory/.cancel" ] && break
    if [ -e "$directory/.result" ]; then
        if {
            IFS= read -r result && ! IFS= read -r extra && [ -z "$extra" ]
        } <"$directory/.result"; then
            case $result in
                ''|*[!0-9]*) ;;
                *) [ "${#result}" -le 3 ] && [ "$result" -le 255 ] && code=$result;;
            esac
        fi
        break
    fi
    kill -0 "$leader" 2>/dev/null || break
    sleep .1
done
kill "$watcher" 2>/dev/null || :
wait "$watcher" 2>/dev/null || :
cleanup_group && remove_evidence || { echo 'WSL2 command cleanup failed: group absence unconfirmed' >&2; exit 74; }
exit "$code"
