#!/usr/bin/env bash

aws_guard_account_for_profile() {
  local profile_name="$1"
  local -a account_cmd=(aws)
  if [[ -n "$profile_name" ]]; then
    account_cmd+=(--profile "$profile_name")
  fi
  "${account_cmd[@]}" sts get-caller-identity --query Account --output text
}

aws_guard_select_profile_for_account() {
  local coordinator_account="$1"
  local refusal_action="$2"
  local selected_profile=""
  local selected_default=0
  local checked_profiles=0
  local candidate_account=""
  local candidate_profile=""
  local profiles=""

  if candidate_account="$(aws_guard_account_for_profile "" 2>/dev/null)" && [[ -n "$candidate_account" ]]; then
    checked_profiles=$((checked_profiles + 1))
    printf 'checked_profile=default-credentials account=%s\n' "$candidate_account" >&2
    if [[ "$candidate_account" == "$coordinator_account" ]]; then
      selected_default=1
    fi
  else
    checked_profiles=$((checked_profiles + 1))
    printf 'checked_profile=default-credentials status=unusable\n' >&2
  fi

  if [[ "$selected_default" != "1" ]]; then
    if ! profiles="$(aws configure list-profiles 2>/dev/null)"; then
      printf 'refusing to %s: failed to enumerate local AWS profiles\n' "$refusal_action" >&2
      return 1
    fi
    while IFS= read -r candidate_profile; do
      [[ -n "$candidate_profile" ]] || continue
      checked_profiles=$((checked_profiles + 1))
      if candidate_account="$(aws_guard_account_for_profile "$candidate_profile" 2>/dev/null)" && [[ -n "$candidate_account" ]]; then
        printf 'checked_profile=%s account=%s\n' "$candidate_profile" "$candidate_account" >&2
        if [[ "$candidate_account" == "$coordinator_account" ]]; then
          selected_profile="$candidate_profile"
          break
        fi
      else
        printf 'checked_profile=%s status=unusable\n' "$candidate_profile" >&2
      fi
    done < <(printf '%s\n' "$profiles")
  fi

  if [[ -z "$selected_profile" && "$selected_default" != "1" ]]; then
    printf 'refusing to %s: no local AWS profile matches coordinator account %s after checking %s profile(s)\n' "$refusal_action" "$coordinator_account" "$checked_profiles" >&2
    return 1
  fi
  if [[ "$selected_default" == "1" ]]; then
    printf '\n'
  else
    printf '%s\n' "$selected_profile"
  fi
}

aws_guard_account_for_selected_profile() {
  local profile_name="$1"
  local coordinator_account="$2"
  local refusal_action="$3"
  local local_account

  if ! local_account="$(aws_guard_account_for_profile "$profile_name")"; then
    printf 'refusing to %s: failed to read local AWS caller identity\n' "$refusal_action" >&2
    return 1
  fi
  if [[ -z "$local_account" ]]; then
    printf 'refusing to %s: local AWS caller identity returned an empty account\n' "$refusal_action" >&2
    return 1
  fi
  if [[ -n "$coordinator_account" && "$local_account" != "$coordinator_account" ]]; then
    printf 'refusing to %s: local AWS account %s does not match coordinator account %s\n' "$refusal_action" "$local_account" "$coordinator_account" >&2
    return 1
  fi
  printf '%s\n' "$local_account"
}
