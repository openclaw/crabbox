// Versioned native source helper. Routing remains gated in Crabbox until qualification.
mod conditions;
mod index_owner;
mod live;
mod protocol;
use std::any::Any;
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, OnceLock};

use jj_cli::cli_util::{CliRunner, CommandHelper, RevisionArg};
use jj_cli::command_error::{CommandError, user_error};
use jj_cli::ui::Ui;
use jj_lib::backend::{BackendLoadError, MergedTreeValueExt, TreeValue};
use jj_lib::checkout_budget::CheckoutOutputBudget;
use jj_lib::default_backend_factories::default_backend_factories;
use jj_lib::default_index::DefaultIndexStore;
use jj_lib::file_input_budget::FileInputBudget;
use jj_lib::git_backend::GitBackend;
use jj_lib::local_working_copy::{
    EolConversionMode, ExecChangeSetting, LocalWorkingCopy, LockedLocalWorkingCopy,
};
use jj_lib::object_id::ObjectId as _;
use jj_lib::op_store::OperationId;
use jj_lib::repo::Repo as _;
use jj_lib::working_copy::WorkingCopy as _;
use protocol::{
    Context as SourceContext, Identity, MaterializationPolicy, RecordedEntry, TreeTerm,
};

#[derive(clap::Parser, Clone, Debug)]
enum PrototypeCommand {
    SourceVersion,
    SourceContext,
    SourceLiveInventory,
    SourceRecordedInventory(RevisionArgs),
    SourceRecordedExport(ExportArgs),
}

#[derive(clap::Args, Clone, Debug)]
struct SessionBudgetArgs {
    #[arg(long, global = true)]
    config_conditions_stdin: bool,
    #[arg(long, global = true)]
    max_object_allocation_bytes: Option<usize>,
    #[arg(long, global = true)]
    max_input_bytes: Option<u64>,
    #[arg(long, global = true)]
    max_conflict_scratch_bytes: Option<u64>,
}

struct SessionLimits {
    object_ceiling: usize,
    input_budget: Arc<FileInputBudget>,
    scratch_budget: Arc<CheckoutOutputBudget>,
}

#[derive(clap::Args, Clone, Debug)]
struct RevisionArgs {
    #[arg(long, default_value = "@")]
    revision: RevisionArg,
}

#[derive(clap::Args, Clone, Debug)]
struct ExportArgs {
    #[arg(long)]
    selected: PathBuf,
    #[arg(long)]
    output: PathBuf,
    #[arg(long)]
    state: PathBuf,
    #[arg(long)]
    max_entry_bytes: u64,
    #[arg(long)]
    max_output_bytes: u64,
}

fn utf8_path(path: &Path) -> Result<String, CommandError> {
    // Keep ordinary Windows roots compatible with consumers without rewriting
    // paths that cannot be represented safely in the ordinary namespace.
    dunce::simplified(path)
        .to_str()
        .map(str::to_owned)
        .ok_or_else(|| user_error("native source path is not UTF-8"))
}

fn policy(
    workspace: &jj_cli::cli_util::WorkspaceCommandHelper,
) -> Result<MaterializationPolicy, CommandError> {
    use jj_lib::conflicts::ConflictMarkerStyle;
    use jj_lib::files::FileMergeHunkLevel;
    use jj_lib::merge::SameChange;
    let settings = workspace.settings();
    let merge = workspace.repo().store().merge_options();
    Ok(MaterializationPolicy {
        exec_policy: match settings.get::<ExecChangeSetting>("working-copy.exec-bit-change")? {
            ExecChangeSetting::Ignore => "ignore",
            ExecChangeSetting::Respect => "respect",
            ExecChangeSetting::Auto => "auto",
        }
        .to_owned(),
        eol_conversion: match EolConversionMode::try_from_settings(settings)? {
            EolConversionMode::None => "none",
            EolConversionMode::Input => "input",
            EolConversionMode::InputOutput => "input-output",
        }
        .to_owned(),
        conflict_marker_style: match settings
            .get::<ConflictMarkerStyle>("ui.conflict-marker-style")?
        {
            ConflictMarkerStyle::Diff => "diff",
            ConflictMarkerStyle::DiffExperimental => "diff-experimental",
            ConflictMarkerStyle::Snapshot => "snapshot",
            ConflictMarkerStyle::Git => "git",
        }
        .to_owned(),
        merge_hunk_level: match merge.hunk_level {
            FileMergeHunkLevel::Line => "line",
            FileMergeHunkLevel::Word => "word",
        }
        .to_owned(),
        same_change: match merge.same_change {
            SameChange::Accept => "accept",
            SameChange::Keep => "keep",
        }
        .to_owned(),
        materialization_host: std::env::consts::OS.to_owned(),
    })
}

fn empty_owned_directory(path: &Path) -> Result<PathBuf, CommandError> {
    let metadata = std::fs::symlink_metadata(path)?;
    if !metadata.is_dir()
        || metadata.file_type().is_symlink()
        || std::fs::read_dir(path)?.next().is_some()
    {
        return Err(user_error(
            "export requires an existing empty real owned directory",
        ));
    }
    Ok(path.canonicalize()?)
}

async fn run(
    ui: &mut Ui,
    command: &CommandHelper,
    args: PrototypeCommand,
    input_budget: &FileInputBudget,
    scratch_budget: &Arc<CheckoutOutputBudget>,
    index_owner: &index_owner::IndexOwner,
) -> Result<(), CommandError> {
    let loader = command.workspace_loader()?;
    let source_root = loader.workspace_root().canonicalize()?;
    let repository_root = loader.repo_path().canonicalize()?;
    index_owner.prepare(&source_root, &repository_root)?;
    let workspace = command.load_workspace()?;
    if matches!(args, PrototypeCommand::SourceContext) {
        let heads = workspace
            .repo_loader()
            .op_heads_store()
            .get_op_heads()
            .await?;
        let mut operation_heads = heads.iter().map(|id| id.hex()).collect::<Vec<_>>();
        operation_heads.sort();
        return protocol::emit(
            ui,
            "context",
            SourceContext {
                workspace: workspace.workspace_name().as_str().to_owned(),
                workspace_root: utf8_path(&loader.workspace_root().canonicalize()?)?,
                repository_path: utf8_path(&loader.repo_path().canonicalize()?)?,
                working_copy_operation: workspace.working_copy().operation_id().hex(),
                operation_heads,
                input_used_bytes: input_budget.used(),
            },
        );
    }
    let request = if let PrototypeCommand::SourceRecordedExport(args) = &args {
        let request: protocol::ExportRequest =
            serde_json::from_slice(&std::fs::read(&args.selected)?).map_err(user_error)?;
        request.validate_protocol()?;
        Some(request)
    } else {
        None
    };
    let revision = match &args {
        PrototypeCommand::SourceRecordedInventory(args) => args.revision.clone(),
        PrototypeCommand::SourceLiveInventory => RevisionArg::from("@".to_owned()),
        PrototypeCommand::SourceRecordedExport(_) => {
            RevisionArg::from(request.as_ref().unwrap().identity.commit.clone())
        }
        PrototypeCommand::SourceContext | PrototypeCommand::SourceVersion => unreachable!(),
    };
    let at = command
        .global_args()
        .at_operation
        .as_deref()
        .ok_or_else(|| user_error("native source requires a full captured operation ID"))?;
    if at.len() != workspace.working_copy().operation_id().hex().len()
        || !at.bytes().all(|c| c.is_ascii_hexdigit())
    {
        return Err(user_error(
            "native source requires a full captured operation ID",
        ));
    }
    let operation_id =
        OperationId::try_from_hex(at).ok_or_else(|| user_error("invalid full operation ID"))?;
    let operation = workspace
        .repo_loader()
        .load_operation(&operation_id)
        .await?;
    let workspace = command
        .workspace_helper_at_operation(ui, workspace, &operation)
        .await?;
    let commit = workspace.resolve_single_rev(ui, &revision).await?;
    let tree = commit.tree();
    let identity = Identity {
        workspace_root: utf8_path(&source_root)?,
        repository_path: utf8_path(&repository_root)?,
        workspace: workspace.workspace_name().as_str().to_owned(),
        operation: operation_id.hex(),
        commit: commit.id().hex(),
        change: commit.change_id().hex(),
        parents: commit.parent_ids().iter().map(|id| id.hex()).collect(),
        tree_ids: tree.tree_ids().iter().map(|id| id.hex()).collect(),
        tree_labels: tree.tree_ids_and_labels().1.as_slice().to_vec(),
        policy: policy(&workspace)?,
    };
    if matches!(args, PrototypeCommand::SourceLiveInventory) {
        return live::emit_inventory(ui, &workspace, &commit, identity, input_budget).await;
    }
    let mut leaves = BTreeMap::new();
    let mut entries = Vec::new();
    for (path, value) in tree.entries() {
        let value = value?;
        let kind = match value.as_normal() {
            Some(TreeValue::File { .. }) => "file",
            Some(TreeValue::Symlink(_)) => "symlink",
            Some(TreeValue::GitSubmodule(_)) => "submodule",
            Some(TreeValue::Tree(_)) => {
                return Err(user_error("unexpected resolved directory leaf"));
            }
            None if value.to_file_merge().is_some() => "file_conflict",
            None => "other_conflict",
        };
        let name = path.as_internal_file_string().to_owned();
        let input_blob_ids: Vec<String> = match value.as_normal() {
            Some(TreeValue::File { id, .. }) => vec![id.hex()],
            Some(TreeValue::Symlink(id)) => vec![id.hex()],
            _ => value
                .to_file_merge()
                .map(|ids| {
                    let (_, simplified) = tree.tree_ids_and_labels().1.simplify_with(&ids);
                    simplified.iter().flatten().map(|id| id.hex()).collect()
                })
                .unwrap_or_default(),
        };
        let terms = value
            .iter()
            .map(|term| {
                term.as_ref().map(|value| match value {
                    TreeValue::File {
                        id,
                        executable,
                        copy_id,
                    } => TreeTerm::File {
                        id: id.hex(),
                        executable: *executable,
                        copy_id: copy_id.hex(),
                    },
                    TreeValue::Symlink(id) => TreeTerm::Symlink { id: id.hex() },
                    TreeValue::GitSubmodule(id) => TreeTerm::Submodule { id: id.hex() },
                    TreeValue::Tree(id) => TreeTerm::Tree { id: id.hex() },
                })
            })
            .collect();
        entries.push(RecordedEntry {
            path: name.clone(),
            kind: kind.to_owned(),
            terms,
            input_blob_ids,
            size_bytes: None,
        });
        leaves.insert(name, (path, kind));
    }
    let PrototypeCommand::SourceRecordedExport(args) = args else {
        return protocol::emit(
            ui,
            "recorded_inventory",
            protocol::RecordedInventory {
                identity,
                entries,
                input_used_bytes: input_budget.used(),
            },
        );
    };
    let request = request.unwrap();
    if request.identity != identity {
        return Err(user_error(
            "recorded source identity or materialization policy changed; capture a new inventory",
        ));
    }
    let selected = request.paths;
    let mut sparse = Vec::new();
    for name in &selected {
        let (path, kind) = leaves
            .get(name)
            .ok_or_else(|| user_error("selected path is not a terminal native tree entry"))?;
        if *kind == "submodule" {
            return Err(user_error(
                "selected submodule has no ordinary file payload",
            ));
        }
        if sparse.contains(path) {
            return Err(user_error("selected paths must be unique"));
        }
        sparse.push(path.clone());
    }
    let output = empty_owned_directory(&args.output)?;
    let state = empty_owned_directory(&args.state)?;
    if output == state
        || output.parent() != state.parent()
        || [&output, &state]
            .iter()
            .any(|path| path.starts_with(&source_root) || path.starts_with(&repository_root))
    {
        return Err(user_error(
            "owned payload and state must be distinct siblings outside source and repository administration",
        ));
    }
    let owned = LocalWorkingCopy::init(
        commit.store().clone(),
        output.clone(),
        state.clone(),
        operation_id.clone(),
        workspace.workspace_name().to_owned(),
        workspace.settings(),
    )?;
    let mut locked = owned.start_mutation().await?;
    let output_budget = Arc::new(CheckoutOutputBudget::new(
        args.max_entry_bytes,
        args.max_output_bytes,
    ));
    let local_locked = (&mut *locked as &mut dyn Any)
        .downcast_mut::<LockedLocalWorkingCopy>()
        .ok_or_else(|| user_error("owned checkout is not a local working copy"))?;
    local_locked.set_checkout_output_budget(output_budget.clone())?;
    local_locked.set_conflict_scratch_budget(scratch_budget.clone())?;
    let capabilities = local_locked.checkout_capabilities()?;
    locked
        .set_sparse_patterns(sparse)
        .await
        .map_err(user_error)?;
    let stats = locked.check_out(&commit).await.map_err(user_error)?;
    if stats.skipped_files != 0 {
        return Err(user_error(
            "native checkout skipped required selected paths",
        ));
    }
    let owned = locked.finish(operation_id).await?;
    let mut exported = Vec::new();
    for name in &selected {
        let metadata = std::fs::symlink_metadata(output.join(name))?;
        if !metadata.is_file() && !metadata.file_type().is_symlink() {
            return Err(user_error(
                "selected native entry did not materialize as file-like output",
            ));
        }
        #[cfg(unix)]
        let unix_mode = {
            use std::os::unix::fs::PermissionsExt as _;
            Some(metadata.permissions().mode() & 0o777)
        };
        #[cfg(not(unix))]
        let unix_mode = None;
        exported.push(protocol::ExportEntry {
            path: name.clone(),
            kind: if metadata.file_type().is_symlink() {
                "symlink"
            } else {
                "file"
            }
            .to_owned(),
            bytes: metadata.len(),
            unix_mode,
        });
    }
    drop(owned);
    protocol::emit(
        ui,
        "recorded_export",
        protocol::RecordedExport {
            identity,
            entries: exported,
            checkout_stats: protocol::CheckoutCounts {
                added_files: stats.added_files as u64,
                updated_files: stats.updated_files as u64,
                removed_files: stats.removed_files as u64,
                skipped_files: stats.skipped_files as u64,
            },
            capabilities: protocol::CheckoutCapabilities {
                executable_bits: capabilities.executable_bits,
                symlinks: capabilities.symlinks,
            },
            output: utf8_path(&output)?,
            owned_state: utf8_path(&state)?,
            output_used_bytes: output_budget.used(),
            input_used_bytes: input_budget.used(),
            scratch_used_bytes: scratch_budget.used(),
        },
    )
}

fn main() -> std::process::ExitCode {
    // The bootstrap option must precede all native arguments: config is loaded
    // before the normal global-argument callback runs.
    let condition_owner = if std::env::args_os().nth(1).as_deref()
        == Some(std::ffi::OsStr::new(conditions::INPUT_FLAG))
    {
        match conditions::Conditions::read() {
            Ok(owner) => Some(Arc::new(owner)),
            Err(error) => {
                eprintln!("Error: {}", error.error);
                return 1u8.into();
            }
        }
    } else {
        None
    };
    let condition_mode = condition_owner.is_some();
    let index_owner = Arc::new(index_owner::IndexOwner::default());
    let index_factory_owner = index_owner.clone();
    let dispatch_index_owner = index_owner.clone();
    let session_limits = Arc::new(OnceLock::<SessionLimits>::new());
    let backend_limits = session_limits.clone();
    let dispatch_limits = session_limits.clone();
    let mut factories = default_backend_factories();
    factories.add_backend(
        GitBackend::name(),
        Box::new(move |settings, path| {
            let limits = backend_limits.get().ok_or_else(|| {
                BackendLoadError(Box::new(std::io::Error::other(
                    "session limits were not set",
                )))
            })?;
            Ok(Box::new(
                GitBackend::load_with_object_allocation_ceiling(
                    settings,
                    path,
                    limits.object_ceiling,
                )?
                .with_readonly_commit_metadata()
                .with_file_input_budget(limits.input_budget.clone()),
            ))
        }),
    );
    factories.add_index_store(
        DefaultIndexStore::name(),
        Box::new(move |_settings, _source_path| {
            let index = index_factory_owner
                .create()
                .map_err(|err| BackendLoadError(Box::new(err)))?;
            Ok(Box::new(index))
        }),
    );
    let mut runner = CliRunner::init();
    if let Some(owner) = &condition_owner {
        runner = runner.with_config_environment_matcher(owner.clone());
    }
    let result = runner
        .add_global_args(move |_ui, args: SessionBudgetArgs| {
            if args.config_conditions_stdin != condition_mode {
                return Err(user_error(
                    "--config-conditions-stdin must be the first argument",
                ));
            }
            if args.max_object_allocation_bytes.is_none()
                && args.max_input_bytes.is_none()
                && args.max_conflict_scratch_bytes.is_none()
            {
                return Ok(());
            }
            let ceiling = args.max_object_allocation_bytes.ok_or_else(|| {
                user_error("--max-object-allocation-bytes is required for this prototype")
            })?;
            let input_limit = args
                .max_input_bytes
                .ok_or_else(|| user_error("--max-input-bytes is required for this prototype"))?;
            let scratch_limit = args.max_conflict_scratch_bytes.ok_or_else(|| {
                user_error("--max-conflict-scratch-bytes is required for this prototype")
            })?;
            session_limits
                .set(SessionLimits {
                    object_ceiling: ceiling,
                    input_budget: Arc::new(FileInputBudget::new(input_limit)),
                    scratch_budget: Arc::new(CheckoutOutputBudget::for_conflict_scratch(
                        scratch_limit,
                    )),
                })
                .map_err(|_| user_error("session limits already set"))?;
            Ok(())
        })
        .with_readonly_secure_config()
        .with_store_factories(factories)
        .add_subcommand(async move |ui, command, args| {
            if matches!(args, PrototypeCommand::SourceVersion) {
                return protocol::emit(
                    ui,
                    "version",
                    protocol::Version {
                        capabilities: vec![
                            "context",
                            "live_inventory",
                            "recorded_inventory",
                            "recorded_export",
                            "environment_conditions",
                        ],
                    },
                );
            }
            let limits = dispatch_limits
                .get()
                .ok_or_else(|| user_error("session limits were not set"))?;
            run(
                ui,
                command,
                args,
                &limits.input_budget,
                &limits.scratch_budget,
                &dispatch_index_owner,
            )
            .await
        })
        .add_dispatch_hook(async |ui, command, inner| {
            if !matches!(
                command.matches().subcommand_name(),
                Some(
                    "source-version"
                        | "source-context"
                        | "source-live-inventory"
                        | "source-recorded-inventory"
                        | "source-recorded-export"
                )
            ) {
                return Err(user_error(
                    "this helper only supports native source commands",
                ));
            }
            inner.call(ui, command).await
        })
        .run();
    let cleanup_errors = index_owner.close();
    for error in &cleanup_errors {
        eprintln!("Error: remove owned source index: {error}");
    }
    if result != 0 && cleanup_errors.is_empty() {
        if let Some(predicate) = condition_owner
            .as_ref()
            .and_then(|owner| owner.take_pending())
        {
            if let Err(error) = protocol::write(
                "environment_condition",
                protocol::EnvironmentCondition { predicate },
                std::io::stdout().lock(),
            ) {
                eprintln!("Error: {}", error.error);
                return 1u8.into();
            }
            return conditions::REQUEST_EXIT.into();
        }
    }
    if result == 0 && !cleanup_errors.is_empty() {
        1u8.into()
    } else {
        result.into()
    }
}

#[cfg(test)]
mod source_tests {
    use std::io::Write as _;

    #[test]
    fn protocol_reports_ordinary_paths_in_compatible_form() {
        let (input, expected) = if cfg!(windows) {
            (r"\\?\C:\fixture\source", r"C:\fixture\source")
        } else {
            ("/fixture/source", "/fixture/source")
        };
        assert_eq!(
            super::utf8_path(std::path::Path::new(input)).unwrap(),
            expected
        );
    }

    #[test]
    fn caller_ceiling_preserves_native_and_reduced_trust_limits() {
        for packed in [false, true] {
            let fixture = tempfile::tempdir().unwrap();
            let created = gix::ThreadSafeRepository::init_opts(
                fixture.path(),
                gix::create::Kind::Bare,
                gix::create::Options::default(),
                gix::open::Options::isolated(),
            )
            .unwrap()
            .to_thread_local();
            let content = vec![b'x'; 4096];
            let id = created.write_blob(&content).unwrap().detach();
            drop(created);
            if packed {
                let pack_prefix = fixture.path().join("objects/pack/fixture");
                let empty_config = fixture.path().join("empty-git-config");
                std::fs::write(&empty_config, []).unwrap();
                let mut command = std::process::Command::new("git")
                    .env_clear()
                    .envs(
                        ["PATH", "SystemRoot", "WINDIR", "TEMP", "TMP"]
                            .into_iter()
                            .filter_map(|name| std::env::var_os(name).map(|value| (name, value))),
                    )
                    .env("GIT_CONFIG_NOSYSTEM", "1")
                    .env("GIT_CONFIG_GLOBAL", &empty_config)
                    .arg("--git-dir")
                    .arg(fixture.path())
                    .arg("pack-objects")
                    .arg(pack_prefix)
                    .stdin(std::process::Stdio::piped())
                    .stdout(std::process::Stdio::piped())
                    .stderr(std::process::Stdio::piped())
                    .spawn()
                    .unwrap();
                writeln!(command.stdin.take().unwrap(), "{id}").unwrap();
                let result = command.wait_with_output().unwrap();
                assert!(result.status.success(), "{result:?}");
                let hex = id.to_string();
                std::fs::remove_file(
                    fixture
                        .path()
                        .join("objects")
                        .join(&hex[..2])
                        .join(&hex[2..]),
                )
                .unwrap();
            }
            for (name, native, fallback, reduced, caller, succeeds) in [
                ("unchanged default", None, None, false, None, true),
                ("caller exact", None, None, false, Some(4096), true),
                ("caller smaller", None, None, false, Some(4095), false),
                (
                    "native stricter",
                    Some(4095),
                    None,
                    false,
                    Some(8192),
                    false,
                ),
                (
                    "caller stricter",
                    Some(8192),
                    None,
                    false,
                    Some(4095),
                    false,
                ),
                ("native exact", Some(4096), None, false, Some(8192), true),
                (
                    "fallback stricter",
                    None,
                    Some(4095),
                    true,
                    Some(8192),
                    false,
                ),
                ("fallback disabled", None, Some(0), true, Some(4095), false),
                ("caller zero", None, None, false, Some(0), false),
            ] {
                let mut config = Vec::new();
                if let Some(value) = native {
                    config.push(format!("gitoxide.objects.allocLimit={value}"));
                }
                if let Some(value) = fallback {
                    config.push(format!("gitoxide.objects.allocLimitIfReducedTrust={value}"));
                }
                let trust = if reduced {
                    gix::sec::Trust::Reduced
                } else {
                    gix::sec::Trust::Full
                };
                let mut options = gix::open::Options::isolated()
                    .with(trust)
                    .config_overrides(config);
                if let Some(value) = caller {
                    options = options.object_allocation_ceiling(value);
                }
                let repo = gix::ThreadSafeRepository::open_opts(fixture.path(), options)
                    .unwrap()
                    .to_thread_local();
                let result = repo.find_object(id);
                assert_eq!(
                    result.is_ok(),
                    succeeds,
                    "packed={packed}, {name}: {result:?}"
                );
                if let Ok(object) = result {
                    assert_eq!(object.data, content, "{name}");
                }
            }
        }
    }
}
