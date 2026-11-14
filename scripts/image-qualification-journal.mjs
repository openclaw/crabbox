// Local effect custody for the existing qualification owner, never registry truth.
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { randomBytes } from "node:crypto";

export function readPrivateJSON(file) {
  if (fs.realpathSync(path.dirname(file)) !== path.resolve(path.dirname(file)))
    throw new Error("qualification journal: symlink ancestry");
  const fd = fs.openSync(
    file,
    fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK,
  );
  try {
    const stat = fs.fstatSync(fd);
    if (
      !stat.isFile() ||
      stat.uid !== process.getuid() ||
      stat.mode & 0o077 ||
      stat.nlink !== 1 ||
      stat.size > 1024 * 1024
    )
      throw new Error("qualification journal: invalid private file");
    return JSON.parse(fs.readFileSync(fd, "utf8"));
  } finally {
    fs.closeSync(fd);
  }
}

export async function withQualificationJournal(
  directory,
  { stateName, lockName, create = false, initial },
  run,
) {
  if (![stateName, lockName].every((name) => /^[a-z-]+\.(json|lock)$/.test(name)))
    throw new Error("qualification journal: invalid file names");
  if (create) fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  if (
    fs.realpathSync(directory) !== path.resolve(directory) ||
    fs.statSync(directory).uid !== process.getuid() ||
    fs.statSync(directory).mode & 0o077
  )
    throw new Error("qualification journal: private retained directory required without symlinks");
  const lock = path.join(directory, lockName);
  const fd = fs.openSync(lock, "wx", 0o600);
  try {
    fs.writeFileSync(fd, JSON.stringify({ host: os.hostname(), pid: process.pid }));
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }
  const file = path.join(directory, stateName);
  const save = async (value) => {
    const temporary = `${file}.${randomBytes(8).toString("hex")}.candidate`;
    const out = fs.openSync(temporary, "wx", 0o600);
    try {
      try {
        fs.writeFileSync(out, `${JSON.stringify(value, null, 2)}\n`);
        fs.fsyncSync(out);
      } finally {
        fs.closeSync(out);
      }
      fs.renameSync(temporary, file);
      const dir = fs.openSync(directory, "r");
      try {
        fs.fsyncSync(dir);
      } finally {
        fs.closeSync(dir);
      }
    } finally {
      if (fs.existsSync(temporary)) fs.unlinkSync(temporary);
    }
  };
  try {
    let state;
    try {
      state = readPrivateJSON(file);
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
      if (!create)
        throw new Error("qualification journal: recovery requires its existing durable journal");
      state = initial();
      await save(state);
    }
    return await run(state, save);
  } finally {
    fs.unlinkSync(lock);
  }
}
