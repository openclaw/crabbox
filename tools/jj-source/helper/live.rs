use std::sync::Mutex;

use jj_cli::cli_util::WorkspaceCommandHelper;
use jj_cli::command_error::{CommandError, user_error};
use jj_cli::ui::Ui;
use jj_lib::backend::{MergedTreeValueExt, TreeValue};
use jj_lib::commit::Commit;
use jj_lib::file_input_budget::FileInputBudget;
use jj_lib::local_working_copy::{FileType, LocalWorkingCopy, RawPathObservationKind};
use jj_lib::object_id::ObjectId as _;
use jj_lib::working_copy::{UntrackedReason, WorkingCopyFreshness};

use super::protocol::{self, Identity};

pub async fn emit_inventory(
    ui: &Ui,
    workspace: &WorkspaceCommandHelper,
    commit: &Commit,
    identity: Identity,
    input_budget: &FileInputBudget,
) -> Result<(), CommandError> {
    if workspace
        .repo()
        .view()
        .get_wc_commit_id(workspace.workspace_name())
        != Some(commit.id())
    {
        return Err(user_error(
            "live inventory requires the current workspace revision; recorded export is a separate mode",
        ));
    }
    let wc = workspace.working_copy();
    let local = wc
        .downcast_ref::<LocalWorkingCopy>()
        .ok_or_else(|| user_error("native source requires a local-disk JJ workspace"))?;
    let state = local.read_existing_state()?;
    match WorkingCopyFreshness::check_at_operation(
        wc.operation_id(),
        state.current_tree(),
        commit,
        workspace.repo(),
    )
    .await?
    {
        WorkingCopyFreshness::Fresh => {}
        WorkingCopyFreshness::Updated(_) => {
            return Err(user_error(
                "working copy advanced beyond the selected operation; capture a new source context",
            ));
        }
        WorkingCopyFreshness::WorkingCopyStale => {
            return Err(user_error(
                "working copy is stale at the selected operation; update it with JJ before source capture",
            ));
        }
        WorkingCopyFreshness::SiblingOperation => {
            return Err(user_error(
                "working copy belongs to a sibling operation; resolve the workspace context before source capture",
            ));
        }
    }
    let tracked = state.file_states();
    let auto = workspace.auto_tracking_matcher(ui)?;
    let options = workspace.snapshot_options_with_start_tracking_matcher(auto.as_ref())?;
    let admitted = Mutex::new(Vec::new());
    let (stats, observations) =
        state.capture_raw(&options, &|path, _disk_path, previous, observed| {
            let (kind, executable) = match &observed.file_type {
                FileType::Normal { exec_bit } => ("file", Some(exec_bit.on_disk())),
                FileType::Symlink => ("symlink", None),
                FileType::GitSubmodule => unreachable!("native walker omits submodules"),
            };
            admitted.lock().unwrap().push(protocol::AdmittedEntry {
                path: path.as_internal_file_string().to_owned(),
                tracked_in_working_copy: previous.is_some(),
                kind,
                executable,
                observed_size: observed.size,
                observed_mtime_ms: observed.mtime.0,
            });
            Ok(())
        })?;
    let mut admitted_entries = admitted.into_inner().unwrap();
    admitted_entries.sort_by(|a, b| a.path.cmp(&b.path));
    let mut tracked_entries: Vec<_> = tracked
        .iter()
        .map(|(path, info)| protocol::TrackedEntry {
            path: path.as_internal_file_string().to_owned(),
            kind: match info.file_type {
                FileType::Normal { .. } => "file",
                FileType::Symlink => "symlink",
                FileType::GitSubmodule => "submodule",
            },
            materialized_conflict: info.materialized_conflict_data.is_some(),
        })
        .collect();
    tracked_entries.sort_by(|a, b| a.path.cmp(&b.path));
    let mut pending_entries = Vec::new();
    for (path, value) in state.current_tree().entries() {
        let value = value?;
        let kind = match value.as_normal() {
            Some(TreeValue::File { .. }) => "file",
            Some(TreeValue::Symlink(_)) => "symlink",
            Some(TreeValue::GitSubmodule(_)) => "submodule",
            Some(TreeValue::Tree(_)) => {
                return Err(user_error(
                    "unexpected directory leaf in native pending-tree enumeration",
                ));
            }
            None if value.to_file_merge().is_some() => "file_conflict",
            None => "other_conflict",
        };
        let structural = value
            .iter()
            .any(|term| matches!(term, Some(TreeValue::Tree(_))));
        let cached = tracked.get(&path);
        pending_entries.push(protocol::PendingEntry {
            path: path.as_internal_file_string().to_owned(),
            kind,
            covers_descendants: structural,
            tracked_regular: matches!(kind, "file" | "file_conflict"),
            in_sparse_scope: state.is_in_sparse_scope(&path),
            cached_materialization: match cached {
                None => "unestablished",
                Some(state) if state.needs_materialization_observation() => "needs_observation",
                Some(_) => "cached",
            },
        });
    }
    pending_entries.sort_by(|a, b| a.path.cmp(&b.path));
    let mut prefixes: Vec<_> = state
        .sparse_patterns()
        .iter()
        .map(|p| p.as_internal_file_string().to_owned())
        .collect();
    prefixes.sort();
    let mut observations: Vec<_> = observations
        .iter()
        .map(|item| protocol::PathObservation {
            path: item.path.as_internal_file_string().to_owned(),
            subtree: item.subtree,
            kind: match item.kind {
                RawPathObservationKind::RemovedAbsent => "removed_absent",
                RawPathObservationKind::RemovedKindReplacement => "removed_kind_replacement",
                RawPathObservationKind::OmittedUnsupportedKind => "omitted_unsupported_kind",
                RawPathObservationKind::OmittedSubmodule => "omitted_submodule",
                RawPathObservationKind::OmittedNestedRepository => "omitted_nested_repository",
                RawPathObservationKind::OmittedNotAdmitted => "omitted_not_admitted",
            },
        })
        .collect();
    observations.sort_by(|a, b| (&a.path, a.kind, a.subtree).cmp(&(&b.path, b.kind, b.subtree)));
    let mut untracked: Vec<_> = stats
        .untracked_paths
        .iter()
        .map(|(path, reason)| protocol::UntrackedEntry {
            path: path.as_internal_file_string().to_owned(),
            reason: match reason {
                UntrackedReason::FileNotAutoTracked => protocol::UntrackedReason::NotAutoTracked,
                UntrackedReason::FileTooLarge { size, max_size } => {
                    protocol::UntrackedReason::TooLarge {
                        size: *size,
                        max_size: *max_size,
                    }
                }
            },
        })
        .collect();
    untracked.sort_by(|a, b| a.path.cmp(&b.path));
    protocol::emit(
        ui,
        "live_inventory",
        protocol::LiveInventory {
            identity,
            working_copy_operation: wc.operation_id().hex(),
            working_copy_freshness: "fresh",
            working_copy_tree_ids: state
                .current_tree()
                .tree_ids()
                .iter()
                .map(|id| id.hex())
                .collect(),
            working_copy_tree_labels: state
                .current_tree()
                .tree_ids_and_labels()
                .1
                .as_slice()
                .to_vec(),
            pending_entries,
            sparse_prefixes: prefixes,
            tracked_entries,
            auto_track: workspace.settings().get_string("snapshot.auto-track")?,
            max_new_file_size: options.max_new_file_size,
            admitted_entries,
            observations,
            untracked,
            invalid_utf8_path_count: stats.invalid_utf8_paths.len(),
            input_used_bytes: input_budget.used(),
        },
    )
}
