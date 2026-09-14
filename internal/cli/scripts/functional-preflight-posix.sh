set -u
mode=$1 directory=$2 nonce=$3 command_size=$4 payload_size=$5 idle_ms=$6 grace_ms=$7
caller_mask=${8:-$(umask)}
umask "$caller_mask" 2>/dev/null || exit 74
umask 077
trap '' HUP

identity() {
    local raw pgid state weekday month day clock year extra
    if [ -r "/proc/$1/stat" ]; then
        IFS= read -r raw <"/proc/$1/stat" 2>/dev/null || return 1
        raw=${raw##*) }
        set -- $raw
        [ "$1" != Z ] && [ "$1" != X ] || return 1
        printf '%s %s\n' "$3" "${20}"
        return
    fi
    read -r pgid state weekday month day clock year extra < <(LC_ALL=C ps -o pgid= -o state= -o lstart= -p "$1" 2>/dev/null) || return 1
    case $state in Z*|X*) return 1;; esac
    [ -z "${extra:-}" ] || return 1
    case $month in
        Jan) month=01;; Feb) month=02;; Mar) month=03;; Apr) month=04;;
        May) month=05;; Jun) month=06;; Jul) month=07;; Aug) month=08;;
        Sep) month=09;; Oct) month=10;; Nov) month=11;; Dec) month=12;;
        *) return 1;;
    esac
    clock=${clock//:/}
    case $pgid:$day:$clock:$year in *[!0-9:]*|:*|*::*|*:) return 1;; esac
    # The living direct-child witness remains reserved through group cleanup.
    # Preserve ps's full start-time representation, not a PID-only observation.
    printf '%s %s%s%02d%s\n' "$pgid" "$year" "$month" "$((10#$day))" "$clock"
}
@GUARDED_GROUP_FUNCTIONS@
@FUNCTIONAL_PRELUDE@
case $mode in
run)
    # Bash monitor mode supplies the portable group boundary. The supervisor
    # remains the direct parent/reaper of the separately guarded worker group.
    set -m
    bash -c "$CBX_HELPER" sh supervise "$directory" "$nonce" "$command_size" 0 "$idle_ms" "$grace_ms" "$caller_mask" <&0 &
    supervisor=$!
    supervisor_group=$(jobs -p %%)
    set +m
    armed=0
    if [ "$supervisor" = "$supervisor_group" ]; then
        for ((i=0; i<(idle_ms+99)/100; i++)); do
            if [ -f "$directory/.supervisor" ]; then
                observed=$(identity "$supervisor") || break
                if [ "${observed%% *}" = "$supervisor" ] &&
                    [ "$(cat "$directory/.nonce" 2>/dev/null)" = "$nonce" ] &&
                    [ "$(cat "$directory/.supervisor" 2>/dev/null)" = "$supervisor $observed" ]; then
                    : >"$directory/.native-armed" || break
                    armed=1
                    break
                fi
            fi
            kill -0 "$supervisor" 2>/dev/null || break
            sleep .1
        done
    fi
    # An unarmed supervisor has its own bounded startup clock. Never acquire
    # new signal authority from a stale pathname or a PID-only lookup.
    code=0
    wait "$supervisor" || code=$?
    [ "$armed" = 1 ] || exit 74
    exit "$code"
    ;;
@GUARDED_MEMBERS@
watch)
    while :; do IFS= read -r -t 1 -u 8 ignored || :; done
    ;;
supervise) ;;
*) exit 74;;
esac

interrupted=0
trap 'interrupted=1' TERM INT
handoff_deadline=$((SECONDS + (idle_ms+999)/1000))
mkdir -m 700 -- "$directory" || exit 74
printf '%s' "$nonce" >"$directory/.nonce" || exit 74
mkdir -m 700 -- "$directory/scratch" || exit 74
printf '%s %s\n' "$$" "$(identity "$$")" >"$directory/.supervisor" || exit 74
while [ ! -e "$directory/.native-armed" ]; do
    if [ "$interrupted" = 1 ] || [ "$SECONDS" -ge "$handoff_deadline" ]; then
        : >"$directory/.cancel"
        remove_evidence || :
        exit 74
    fi
    sleep .1
done
exec 3<&0
exec 0</dev/null
head -c "$command_size" <&3 >"$directory/command" 3<&- &
receiver=$!
failed=0
while kill -0 "$receiver" 2>/dev/null; do
    if [ "$interrupted" = 1 ] || [ -e "$directory/.cancel" ] || [ "$SECONDS" -ge "$handoff_deadline" ]; then
        failed=1
        kill "$receiver" 2>/dev/null || :
        break
    fi
    sleep .1
done
wait "$receiver" || failed=1
exec 3<&-
[ "$(wc -c <"$directory/command")" -eq "$command_size" ] || failed=1
if [ "$failed" = 1 ]; then remove_evidence || :; exit 74; fi
: >"$directory/input"
mkfifo -m 600 "$directory/scratch/control" || exit 74
exec 8<>"$directory/scratch/control"
bash -c "$CBX_HELPER" sh watch "$directory" "$nonce" 0 0 0 0 "$caller_mask" </dev/null &
watcher=$!
exec 8>&-
@GUARDED_WORKLOAD@
