use std::path::{Path, PathBuf};
use std::sync::{Mutex, OnceLock};

use jj_cli::command_error::{CommandError, user_error};
use jj_lib::default_index::DefaultIndexStore;

#[derive(Default)]
pub struct IndexOwner {
    base: OnceLock<PathBuf>,
    directories: Mutex<Vec<tempfile::TempDir>>,
}

#[cfg(test)]
mod tests {
    use super::IndexOwner;

    #[test]
    fn retains_each_index_until_owner_close() {
        let fixture = tempfile::tempdir().unwrap();
        let owner = IndexOwner::default();
        owner.base.set(fixture.path().to_owned()).unwrap();
        let first = owner.create().unwrap();
        let second = owner.create().unwrap();
        let paths: Vec<_> = owner
            .directories
            .lock()
            .unwrap()
            .iter()
            .map(|directory| directory.path().to_owned())
            .collect();
        assert_eq!(paths.len(), 2);
        assert_ne!(paths[0], paths[1]);
        assert!(paths.iter().all(|path| path.is_dir()));
        drop((first, second));
        assert!(owner.close().is_empty());
        assert!(paths.iter().all(|path| !path.exists()));
        assert!(owner.close().is_empty());
    }
}

impl IndexOwner {
    pub fn prepare(&self, source: &Path, repository: &Path) -> Result<(), CommandError> {
        let base = std::env::temp_dir().canonicalize()?;
        if base.starts_with(source) || base.starts_with(repository) {
            return Err(user_error(
                "owned index temporary base must be outside source and repository administration",
            ));
        }
        self.base
            .set(base)
            .map_err(|_| user_error("owned index base already set"))
    }

    pub fn create(&self) -> std::io::Result<DefaultIndexStore> {
        let base = self
            .base
            .get()
            .ok_or_else(|| std::io::Error::other("owned index base was not validated"))?;
        let directory = tempfile::Builder::new()
            .prefix("crabbox-jj-owned-index-")
            .tempdir_in(base)?;
        let path = directory.path().to_owned();
        // Retain the owner before initialization so failures use the same cleanup path.
        self.directories.lock().unwrap().push(directory);
        DefaultIndexStore::init(&path).map_err(std::io::Error::other)
    }

    pub fn close(&self) -> Vec<std::io::Error> {
        std::mem::take(&mut *self.directories.lock().unwrap())
            .into_iter()
            .filter_map(|directory| directory.close().err())
            .collect()
    }
}
