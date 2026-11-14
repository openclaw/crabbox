// Real process-death qualification against an on-disk synthetic cloud, no network.
import fs from "node:fs";
import path from "node:path";
import { idleFixture } from "./qualification-idle-controller.mjs";
import { idleController, idleInitialState } from "../image-qualification-idle-controller.mjs";
import { withQualificationJournal } from "../image-qualification-journal.mjs";
const [directory, mode, crash] = process.argv.slice(2);
const f = idleFixture();
const cloudFile = path.join(directory, "synthetic-cloud.json");
if (fs.existsSync(cloudFile))
  Object.assign(f.model, JSON.parse(fs.readFileSync(cloudFile, "utf8")));
const store = () => fs.writeFileSync(cloudFile, JSON.stringify(f.model));
const request = f.cf.request.bind(f.cf);
f.cf.request = async (...args) => {
  const value = await request(...args);
  store();
  const [route, init] = args;
  if (
    (crash === "create" && init.method === "PUT") ||
    (crash === "exposure" && init.method === "POST") ||
    (crash === "delete" && init.method === "DELETE")
  )
    process.exit(91);
  return value;
};
const invoke = f.context.invoke;
f.context.invoke = async (...args) => {
  const value = await invoke(...args);
  store();
  if (
    (crash === "identity" && args[0].endsWith("binding-identity")) ||
    (crash === "discovery" && args[0].endsWith("discover"))
  )
    process.exit(91);
  return value;
};
try {
  await withQualificationJournal(
    directory,
    {
      stateName: "idle-controller-state.json",
      lockName: "idle-controller.lock",
      create: mode === "run",
      initial: () => idleInitialState(f.plan),
    },
    async (state, save) => {
      // Fixture write-ahead assertions observe the exact same live state object.
      Object.assign(f.state, state);
      await idleController(f.cf, f.plan, f.state, {
        ...f.context,
        mode,
        save: async (value) => {
          await save(value);
          await f.save(value);
          if (crash === "before-create" && value.phase === "create-intent") process.exit(91);
        },
      });
    },
  );
  store();
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
