use anyhow::{Context, Result, ensure};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::fs::{self, OpenOptions};
use std::io::{Read, Write};
use std::path::Path;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;
use time::{OffsetDateTime, format_description::well_known::Rfc3339};
use wasmtime::{Config, Engine, Linker, Module, OptLevel, Store, Strategy};
use wasmtime_wasi::p2::pipe::MemoryOutputPipe;
use wasmtime_wasi::{Deterministic, HostMonotonicClock, HostWallClock, I32Exit, WasiCtxBuilder};

pub const INITIAL_FUEL: u64 = 9_007_199_254_740_991;
pub const ENGINE_VERSION: &str = "50.0.1";
pub const ENGINE_REVISION: &str = "c307ec81de02852d815e9849f7c89d955c03c30d";
pub const CONFIGURATION: &str = "crabbox-wasmtime-fuel/v2";
pub const WASI_CONFIGURATION: &str = "crabbox-deterministic-wasi/v1";
pub const TOOLCHAIN: &str = "go1.27.3";
pub const ARGV: [&str; 4] = [
    "workflow-action-pins.test",
    "-test.run=^(TestCheckWorkflowNodeAcceptsImmutableUsesSyntaxes|TestCheckWorkflowNodeIgnoresUnrelatedUsesKeys)$",
    "-test.count=1",
    "-test.shuffle=off",
];
const MAX_MODULE: u64 = 16 * 1024 * 1024;
const MAX_STDIO: usize = 256 * 1024;

pub fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

pub fn comparison_key(workload: &str) -> Result<String> {
    ensure!(
        !workload.is_empty() && workload.len() <= 128,
        "invalid workload name"
    );
    ensure!(
        workload
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || b"-._/".contains(&c)),
        "workload name must be an ASCII identifier"
    );
    // Every key/value in this fixed projection is ASCII and contains no numbers.
    // serde_json's sorted map encoding is therefore RFC 8785 canonical JSON.
    let projection = json!({
        "argv": ARGV, "configuration": CONFIGURATION, "engine": "wasmtime",
        "engineRevision": ENGINE_REVISION, "engineVersion": ENGINE_VERSION,
        "metric": "fuel", "target": "wasip1/wasm", "toolchain": TOOLCHAIN,
        "unit": "fuel", "wasiConfiguration": WASI_CONFIGURATION, "workload": workload
    });
    Ok(format!(
        "sha256:{}",
        digest(&serde_json::to_vec(&projection)?)
    ))
}

struct FixedWall;
impl HostWallClock for FixedWall {
    fn resolution(&self) -> Duration {
        Duration::from_nanos(1)
    }
    fn now(&self) -> Duration {
        Duration::from_secs(1_700_000_000)
    }
}
struct ScriptedMonotonic(AtomicU64);
impl HostMonotonicClock for ScriptedMonotonic {
    fn resolution(&self) -> u64 {
        1
    }
    fn now(&self) -> u64 {
        self.0.fetch_add(1_000, Ordering::Relaxed)
    }
}

pub struct Measurement {
    pub remaining: u64,
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
}

fn measure(bytes: &[u8], initial: u64) -> Result<Measurement> {
    ensure!(
        bytes.starts_with(b"\0asm\x01\0\0\0"),
        "expected a binary WebAssembly module"
    );
    let mut config = Config::new();
    config.consume_fuel(true).epoch_interruption(false);
    config.strategy(Strategy::Cranelift);
    config.cranelift_opt_level(OptLevel::None);
    // Avoid Linux memfd-backed initialization under the fixture's file/syscall
    // limits. Copy initial memory bytes instead; instantiation still counts.
    config.memory_init_cow(false);
    // Threads are not compiled in. Use synchronous linking/calls below; Wasmtime
    // 50's deprecated async_support switch no longer changes execution behavior.
    // No cache configuration, async yielding, epoch deadline, or concurrent guest.
    #[cfg(feature = "fixture")]
    let _ = std::io::stderr().write_all(b"meter: engine-create\n");
    let engine = Engine::new(&config)?;
    #[cfg(feature = "fixture")]
    let _ = std::io::stderr().write_all(b"meter: module-compile\n");
    let module = Module::new(&engine, bytes)?;
    #[cfg(feature = "fixture")]
    let _ = std::io::stderr().write_all(b"meter: module-ready\n");
    for import in module.imports() {
        ensure!(
            import.module() == "wasi_snapshot_preview1",
            "unexpected import module"
        );
        ensure!(
            !import.name().starts_with("sock_"),
            "socket imports are outside this workload"
        );
    }
    let stdout = MemoryOutputPipe::new(MAX_STDIO);
    let stderr = MemoryOutputPipe::new(MAX_STDIO);
    let mut wasi = WasiCtxBuilder::new();
    wasi.args(&ARGV)
        .stdout(stdout.clone())
        .stderr(stderr.clone())
        .secure_random(Deterministic::new((0u8..=255).collect()))
        .insecure_random(Deterministic::new((0u8..=255).collect()))
        .insecure_random_seed(0)
        .wall_clock(FixedWall)
        .monotonic_clock(ScriptedMonotonic(AtomicU64::new(0)))
        .allow_tcp(false)
        .allow_udp(false)
        .allow_ip_name_lookup(false);
    // Builder defaults give closed stdin, empty environment and no preopens.
    let mut store = Store::new(&engine, wasi.build_p1());
    let mut linker = Linker::new(&engine);
    wasmtime_wasi::p1::add_to_linker_sync(&mut linker, |ctx| ctx)?;
    store.set_fuel(initial)?;
    // Fuel starts before instantiation: start functions and Go initialization count.
    #[cfg(feature = "fixture")]
    let _ = std::io::stderr().write_all(b"meter: instantiate\n");
    let instance = linker.instantiate(&mut store, &module)?;
    #[cfg(feature = "fixture")]
    let _ = std::io::stderr().write_all(b"meter: instance-ready\n");
    let start = instance.get_typed_func::<(), ()>(&mut store, "_start")?;
    eprintln!("meter: guest-started");
    if let Err(error) = start.call(&mut store, ()) {
        if !matches!(error.downcast_ref::<I32Exit>(), Some(I32Exit(0))) {
            return Err(error.into());
        }
    }
    let remaining = store.get_fuel()?;
    ensure!(remaining <= initial, "invalid remaining fuel");
    Ok(Measurement {
        remaining,
        stdout: stdout.contents().to_vec(),
        stderr: stderr.contents().to_vec(),
    })
}

pub fn evidence(bytes: &[u8], workload: &str, provider: &str, budget: u64) -> Result<Value> {
    evidence_with_ceiling(bytes, workload, provider, budget, INITIAL_FUEL)
}

fn evidence_with_ceiling(
    bytes: &[u8],
    workload: &str,
    provider: &str,
    budget: u64,
    initial: u64,
) -> Result<Value> {
    ensure!(
        budget <= INITIAL_FUEL && initial <= INITIAL_FUEL,
        "fuel exceeds JSON safe integer range"
    );
    ensure!(
        !provider.is_empty()
            && provider.len() <= 64
            && provider
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"-._".contains(&b)),
        "invalid provider identifier"
    );
    let comparison_key = comparison_key(workload)?;
    let measured = measure(bytes, initial)?;
    let count = initial
        .checked_sub(measured.remaining)
        .context("invalid fuel subtraction")?;
    let exceeded = count > budget;
    Ok(json!({
        "schemaVersion": 1, "generatedAt": OffsetDateTime::now_utc().format(&Rfc3339)?,
        "provider": provider,
        "workload": {"name": workload, "moduleSha256": digest(bytes), "toolchain": TOOLCHAIN, "target": "wasip1/wasm", "argv": ARGV},
        "meter": {"mechanism": "wasmtime-fuel", "engine": "wasmtime", "engineVersion": ENGINE_VERSION,
            "engineRevision": ENGINE_REVISION, "configuration": CONFIGURATION, "wasiConfiguration": WASI_CONFIGURATION,
            "comparisonKey": comparison_key, "deterministic": true, "initialFuel": initial, "remainingFuel": measured.remaining},
        "results": [{"metric": "fuel", "unit": "fuel", "measured": count, "budget": budget, "exceeded": exceeded}],
        "exceeded": exceeded,
        "guestOutput": {"stdoutSha256": digest(&measured.stdout), "stderrSha256": digest(&measured.stderr)}
    }))
}

pub fn run(
    module: &Path,
    expected: &str,
    output: &Path,
    workload: &str,
    provider: &str,
    budget: u64,
) -> Result<bool> {
    run_with_ceiling(
        module,
        expected,
        output,
        workload,
        provider,
        budget,
        INITIAL_FUEL,
    )
}

fn run_with_ceiling(
    module: &Path,
    expected: &str,
    output: &Path,
    workload: &str,
    provider: &str,
    budget: u64,
    initial: u64,
) -> Result<bool> {
    use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
    ensure!(
        expected.len() == 64
            && expected
                .bytes()
                .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c)),
        "invalid expected module digest"
    );
    let parent = output.parent().context("output needs a parent directory")?;
    let metadata = fs::symlink_metadata(parent)?;
    ensure!(
        metadata.is_dir()
            && metadata.uid() == unsafe { libc::geteuid() }
            && metadata.permissions().mode() & 0o077 == 0,
        "output parent must be a private owned directory"
    );
    ensure!(
        fs::symlink_metadata(output).is_err_and(|e| e.kind() == std::io::ErrorKind::NotFound),
        "output must be absent before invocation"
    );
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(module)?;
    ensure!(file.metadata()?.is_file(), "module must be a regular file");
    let mut bytes = Vec::new();
    file.take(MAX_MODULE + 1).read_to_end(&mut bytes)?;
    ensure!(
        bytes.len() as u64 <= MAX_MODULE,
        "module size limit exceeded"
    );
    ensure!(digest(&bytes) == expected, "module provenance mismatch");
    let value = evidence_with_ceiling(&bytes, workload, provider, budget, initial)?;
    let encoded = serde_json::to_vec(&value)?;
    ensure!(encoded.len() <= 65_536, "evidence size limit exceeded");
    let temporary = parent.join(format!(".fuel-{}.tmp", std::process::id()));
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&temporary)?;
    let mut published = false;
    let publication = (|| -> Result<()> {
        file.write_all(&encoded)?;
        file.write_all(b"\n")?;
        file.sync_all()?;
        // Publish complete bytes without replacing a destination created by a
        // concurrent invocation after the initial absence check.
        fs::hard_link(&temporary, output)?;
        published = true;
        fs::remove_file(&temporary)?;
        fs::File::open(parent)?.sync_all()?;
        Ok(())
    })();
    if publication.is_err() {
        let _ = fs::remove_file(&temporary);
        if published {
            let _ = fs::remove_file(output);
        }
    }
    publication?;
    Ok(value["exceeded"]
        .as_bool()
        .context("missing budget predicate")?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;
    fn module(body: &str) -> Vec<u8> {
        wat::parse_str(format!("(module (func (export \"_start\") {body}))")).unwrap()
    }
    #[test]
    fn arithmetic_and_budget_equality() {
        let a = evidence(
            &module("nop"),
            "example/workflow/v1",
            "fixture",
            INITIAL_FUEL,
        )
        .unwrap();
        let count = a["results"][0]["measured"].as_u64().unwrap();
        assert!(count > 0);
        let equal = evidence(&module("nop"), "example/workflow/v1", "fixture", count).unwrap();
        assert_eq!(equal["exceeded"], false);
        let lower = evidence(&module("nop"), "example/workflow/v1", "fixture", count - 1).unwrap();
        assert_eq!(lower["exceeded"], true);
        assert_eq!(
            count,
            INITIAL_FUEL - equal["meter"]["remainingFuel"].as_u64().unwrap()
        );
        assert_eq!(a["meter"]["comparisonKey"], lower["meter"]["comparisonKey"]);
    }
    #[test]
    fn v2_configuration_has_a_distinct_comparison_key() {
        let value = evidence(
            &module("nop"),
            "example-org/workflow-action-pin-tests/v2",
            "fixture",
            INITIAL_FUEL,
        )
        .unwrap();
        assert_eq!(value["meter"]["configuration"], "crabbox-wasmtime-fuel/v2");
        assert_eq!(
            value["meter"]["wasiConfiguration"],
            "crabbox-deterministic-wasi/v1"
        );
        assert_eq!(
            value["meter"]["comparisonKey"],
            "sha256:158ee0686954ed1ad1e62deeb2d8b14b9ca8e133adb30d7c080ca82c1f174bb4"
        );
        assert_ne!(
            value["meter"]["comparisonKey"],
            "sha256:38b199dc30e8b9603f9a4d5ddfa74929ed781459a346bd41593a1451ec79e065"
        );
    }
    #[test]
    fn bounded_loop_fuel_is_repeatable_and_detects_more_work() {
        let loop_module = |iterations| {
            module(&format!(
                "(local $left i32) i32.const {iterations} local.set $left
                 (loop $again local.get $left i32.const 1 i32.sub
                  local.tee $left br_if $again)"
            ))
        };
        let baseline = loop_module(10);
        let mutation = loop_module(20);
        let ceiling = 1_000;
        let consumed = |bytes: &[u8]| ceiling - measure(bytes, ceiling).unwrap().remaining;
        let count = consumed(&baseline);
        assert!(count > 0 && count < ceiling);
        assert_eq!(consumed(&baseline), count);
        assert!(consumed(&mutation) > count);
    }
    #[test]
    fn wasi_exit_zero_completes_and_nonzero_is_a_guest_error() {
        let exit_module = |code| {
            wat::parse_str(format!(
                "(module (import \"wasi_snapshot_preview1\" \"proc_exit\" (func $exit (param i32)))
                 (memory (export \"memory\") 1)
                 (func (export \"_start\") i32.const {code} call $exit))"
            ))
            .unwrap()
        };
        if let Err(error) = measure(&exit_module(0), 100) {
            panic!("proc_exit(0) failed: {error:#}");
        }
        let error = measure(&exit_module(7), 100)
            .err()
            .expect("proc_exit(7) unexpectedly completed");
        assert!(
            matches!(error.downcast_ref::<I32Exit>(), Some(I32Exit(7))),
            "expected I32Exit(7), got: {error:#}"
        );
    }
    #[test]
    fn test_only_small_ceiling_uses_real_out_of_fuel_and_never_publishes() {
        let bytes = module("(loop br 0)");
        let error = measure(&bytes, 16).err().unwrap();
        assert_eq!(
            error.downcast_ref::<wasmtime::Trap>(),
            Some(&wasmtime::Trap::OutOfFuel)
        );
        let dir = std::env::temp_dir().join(format!("fuel-exhaustion-{}", std::process::id()));
        fs::create_dir(&dir).unwrap();
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).unwrap();
        let source = dir.join("in.wasm");
        let output = dir.join("evidence.json");
        fs::write(&source, &bytes).unwrap();
        assert!(
            run_with_ceiling(
                &source,
                &digest(&bytes),
                &output,
                "example/workflow/v1",
                "fixture",
                1,
                16
            )
            .is_err()
        );
        assert!(!output.exists());
        assert_eq!(fs::read_dir(&dir).unwrap().count(), 1);
        fs::remove_dir_all(&dir).unwrap();
    }
    #[test]
    fn traps_and_unknown_imports_are_measurement_failures() {
        assert!(evidence(&module("unreachable"), "example/workflow/v1", "fixture", 1).is_err());
        let foreign = wat::parse_str(
            "(module (import \"foreign\" \"call\" (func)) (func (export \"_start\")))",
        )
        .unwrap();
        assert!(evidence(&foreign, "example/workflow/v1", "fixture", 1).is_err());
        assert!(comparison_key("workload\nforged").is_err());
    }

    #[test]
    fn bad_provenance_and_stale_output_never_replace_evidence() {
        let dir = std::env::temp_dir().join(format!("fuel-publication-{}", std::process::id()));
        fs::create_dir(&dir).unwrap();
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).unwrap();
        let source = dir.join("in.wasm");
        let output = dir.join("evidence.json");
        let bytes = module("nop");
        fs::write(&source, &bytes).unwrap();
        assert!(
            run(
                &source,
                &"0".repeat(64),
                &output,
                "example/workflow/v1",
                "fixture",
                1
            )
            .is_err()
        );
        assert!(!output.exists());
        fs::write(&output, b"prior evidence").unwrap();
        assert!(
            run(
                &source,
                &digest(&bytes),
                &output,
                "example/workflow/v1",
                "fixture",
                1
            )
            .is_err()
        );
        assert_eq!(fs::read(&output).unwrap(), b"prior evidence");
        fs::remove_dir_all(&dir).unwrap();
    }
}
