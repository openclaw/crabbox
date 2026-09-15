use jj_cli::command_error::{CommandError, user_error};
use jj_cli::ui::Ui;
use serde::{Deserialize, Serialize};

pub const PROTOCOL: &str = "crabbox-jj-source";
pub const SCHEMA_VERSION: u32 = 1;

#[derive(Serialize)]
struct Header {
    protocol: &'static str,
    schema_version: u32,
    kind: &'static str,
    jj_version: &'static str,
}

#[derive(Serialize)]
struct Response<T> {
    #[serde(flatten)]
    header: Header,
    #[serde(flatten)]
    body: T,
}

pub fn emit(ui: &Ui, kind: &'static str, body: impl Serialize) -> Result<(), CommandError> {
    write(kind, body, ui.stdout())
}

pub fn write(
    kind: &'static str,
    body: impl Serialize,
    mut output: impl std::io::Write,
) -> Result<(), CommandError> {
    let response = Response {
        header: Header {
            protocol: PROTOCOL,
            schema_version: SCHEMA_VERSION,
            kind,
            jj_version: env!("CARGO_PKG_VERSION"),
        },
        body,
    };
    serde_json::to_writer(&mut output, &response).map_err(user_error)?;
    output.write_all(b"\n")?;
    Ok(())
}

#[derive(Serialize)]
pub struct Version {
    pub capabilities: Vec<&'static str>,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct MaterializationPolicy {
    pub exec_policy: String,
    pub eol_conversion: String,
    pub conflict_marker_style: String,
    pub merge_hunk_level: String,
    pub same_change: String,
    pub materialization_host: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Identity {
    pub workspace_root: String,
    pub repository_path: String,
    pub workspace: String,
    pub operation: String,
    pub commit: String,
    pub change: String,
    pub parents: Vec<String>,
    /// Positive and negative terms alternate, starting and ending positive.
    pub tree_ids: Vec<String>,
    pub tree_labels: Vec<String>,
    pub policy: MaterializationPolicy,
}

#[derive(Serialize)]
pub struct Context {
    pub workspace_root: String,
    pub repository_path: String,
    pub workspace: String,
    pub working_copy_operation: String,
    pub operation_heads: Vec<String>,
    pub input_used_bytes: u64,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum TreeTerm {
    File {
        id: String,
        executable: bool,
        copy_id: String,
    },
    Symlink {
        id: String,
    },
    Submodule {
        id: String,
    },
    Tree {
        id: String,
    },
}

#[derive(Serialize)]
pub struct RecordedEntry {
    pub path: String,
    pub kind: String,
    /// Ordered positive/negative native terms; absence and repetition are preserved.
    pub terms: Vec<Option<TreeTerm>>,
    pub input_blob_ids: Vec<String>,
    pub size_bytes: Option<u64>,
}

#[derive(Serialize)]
pub struct RecordedInventory {
    pub identity: Identity,
    pub entries: Vec<RecordedEntry>,
    pub input_used_bytes: u64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExportRequest {
    pub protocol: String,
    pub schema_version: u32,
    pub identity: Identity,
    pub paths: Vec<String>,
}

impl ExportRequest {
    pub fn validate_protocol(&self) -> Result<(), CommandError> {
        if self.protocol != PROTOCOL || self.schema_version != SCHEMA_VERSION {
            return Err(user_error(
                "unsupported native source export request protocol",
            ));
        }
        Ok(())
    }
}

#[derive(Serialize)]
pub struct ExportEntry {
    pub path: String,
    pub kind: String,
    pub bytes: u64,
    pub unix_mode: Option<u32>,
}

#[derive(Serialize)]
pub struct CheckoutCounts {
    pub added_files: u64,
    pub updated_files: u64,
    pub removed_files: u64,
    pub skipped_files: u64,
}

#[derive(Serialize)]
pub struct CheckoutCapabilities {
    pub executable_bits: bool,
    pub symlinks: bool,
}

#[derive(Serialize)]
pub struct RecordedExport {
    pub identity: Identity,
    pub entries: Vec<ExportEntry>,
    pub checkout_stats: CheckoutCounts,
    pub capabilities: CheckoutCapabilities,
    pub output: String,
    pub owned_state: String,
    pub input_used_bytes: u64,
    pub output_used_bytes: u64,
    pub scratch_used_bytes: u64,
}

#[derive(Serialize)]
pub struct PendingEntry {
    pub path: String,
    pub kind: &'static str,
    pub covers_descendants: bool,
    pub tracked_regular: bool,
    pub in_sparse_scope: bool,
    pub cached_materialization: &'static str,
}

#[derive(Serialize)]
pub struct TrackedEntry {
    pub path: String,
    pub kind: &'static str,
    pub materialized_conflict: bool,
}

#[derive(Serialize)]
pub struct AdmittedEntry {
    pub path: String,
    pub tracked_in_working_copy: bool,
    pub kind: &'static str,
    pub executable: Option<bool>,
    pub observed_size: u64,
    pub observed_mtime_ms: i64,
}

#[derive(Serialize)]
pub struct PathObservation {
    pub path: String,
    pub subtree: bool,
    pub kind: &'static str,
}

#[derive(Serialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum UntrackedReason {
    NotAutoTracked,
    TooLarge { size: u64, max_size: u64 },
}

#[derive(Serialize)]
pub struct UntrackedEntry {
    pub path: String,
    pub reason: UntrackedReason,
}

#[derive(Serialize)]
pub struct LiveInventory {
    pub identity: Identity,
    pub working_copy_operation: String,
    pub working_copy_freshness: &'static str,
    pub working_copy_tree_ids: Vec<String>,
    pub working_copy_tree_labels: Vec<String>,
    pub pending_entries: Vec<PendingEntry>,
    pub sparse_prefixes: Vec<String>,
    pub tracked_entries: Vec<TrackedEntry>,
    pub auto_track: String,
    pub max_new_file_size: u64,
    pub admitted_entries: Vec<AdmittedEntry>,
    pub observations: Vec<PathObservation>,
    pub untracked: Vec<UntrackedEntry>,
    pub invalid_utf8_path_count: usize,
    pub input_used_bytes: u64,
}

#[derive(Serialize)]
pub struct EnvironmentCondition {
    pub predicate: String,
}
