use anyhow::{Context, Result, bail, ensure};
use std::collections::BTreeMap;
use std::path::Path;

fn main() {
    match execute() {
        Ok(false) => (),
        Ok(true) => std::process::exit(3),
        Err(error) => {
            eprintln!("meter: {error:#}");
            std::process::exit(2);
        }
    }
}

fn execute() -> Result<bool> {
    let mut args = std::env::args().skip(1);
    let mut values = BTreeMap::new();
    while let Some(key) = args.next() {
        ensure!(
            [
                "--module",
                "--module-sha256",
                "--output",
                "--workload",
                "--provider",
                "--budget"
            ]
            .contains(&key.as_str()),
            "unknown argument"
        );
        let value = args.next().context("argument needs a value")?;
        ensure!(values.insert(key, value).is_none(), "duplicate argument");
    }
    let get = |key: &str| values.get(key).with_context(|| format!("missing {key}"));
    let budget = get("--budget")?;
    if !budget.bytes().all(|c| c.is_ascii_digit()) {
        bail!("budget must be an unsigned integer");
    }
    crabbox_wasmtime_meter::run(
        Path::new(get("--module")?),
        get("--module-sha256")?,
        Path::new(get("--output")?),
        get("--workload")?,
        get("--provider")?,
        budget.parse()?,
    )
}
