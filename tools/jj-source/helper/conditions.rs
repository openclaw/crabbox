use std::collections::HashMap;
use std::io::Read as _;
use std::sync::Mutex;

use jj_cli::command_error::{CommandError, user_error};
use jj_lib::config::{ConfigEnvironmentMatcher, ConfigGetError};
use serde::Deserialize;

use super::protocol;

pub const INPUT_FLAG: &str = "--config-conditions-stdin";
pub const REQUEST_EXIT: u8 = 75;
const MAX_INPUT_BYTES: u64 = 1 << 20;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Input {
    protocol: String,
    schema_version: u32,
    answers: Vec<Answer>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Answer {
    predicate: String,
    matched: bool,
}

#[derive(Debug)]
pub struct Conditions {
    answers: HashMap<String, bool>,
    pending: Mutex<Option<String>>,
}

impl Conditions {
    pub fn read() -> Result<Self, CommandError> {
        let mut data = Vec::new();
        std::io::stdin()
            .lock()
            .take(MAX_INPUT_BYTES + 1)
            .read_to_end(&mut data)?;
        if data.len() as u64 > MAX_INPUT_BYTES {
            return Err(user_error(
                "native environment condition input exceeds limit",
            ));
        }
        let input: Input = serde_json::from_slice(&data).map_err(user_error)?;
        if input.protocol != protocol::PROTOCOL || input.schema_version != protocol::SCHEMA_VERSION
        {
            return Err(user_error("unsupported native environment condition input"));
        }
        let mut answers = HashMap::new();
        for answer in input.answers {
            if answers.insert(answer.predicate, answer.matched).is_some() {
                return Err(user_error("duplicate native environment condition answer"));
            }
        }
        Ok(Self {
            answers,
            pending: Mutex::new(None),
        })
    }

    pub fn take_pending(&self) -> Option<String> {
        self.pending.lock().unwrap().take()
    }
}

impl ConfigEnvironmentMatcher for Conditions {
    fn matches(&self, predicate: &str) -> Result<bool, ConfigGetError> {
        if let Some(answer) = self.answers.get(predicate) {
            return Ok(*answer);
        }
        self.pending
            .lock()
            .unwrap()
            .get_or_insert_with(|| predicate.to_owned());
        Err(ConfigGetError::Type {
            name: "--when.environments".to_owned(),
            error: "native environment condition requires a parent decision".into(),
            source_path: None,
        })
    }
}
