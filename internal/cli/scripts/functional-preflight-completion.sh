remove_evidence() {
    [ "$(cat "$directory/.nonce" 2>/dev/null)" = "$nonce" ] || return 1
    # The supervisor calls this only after reaping its worker group, or before
    # that group was started. Retire the private control stage after collection.
    rm -rf -- "$directory/scratch" || return 1
    [ ! -e "$directory/scratch" ] && [ ! -L "$directory/scratch" ] || return 1
    local state=worker-failed
    if [ -e "$directory/.timed-out" ]; then
        state=timed-out
    elif [ -e "$directory/.cancel" ] || [ -e "$directory/.lost" ]; then
        state=canceled
    else
        case ${code:-74} in
            0) state=ready;;
            20) state=missing-python3;;
            21) state=venv-unavailable;;
            22) state=pip-unavailable;;
        esac
    fi
    printf 'CBX-PREFLIGHT-1\n%s\n%s\nworker-quiesced\nscratch-removed\ncomplete\n' "$nonce" "$state" >"$directory/.completion.tmp" &&
        mv -- "$directory/.completion.tmp" "$directory/.completion"
}
