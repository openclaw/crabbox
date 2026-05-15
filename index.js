import { spawn } from "node:child_process";

const PLUGIN_ID = "crabbox";
const DEFAULT_BINARY = "crabbox";
const DEFAULT_MAX_OUTPUT_BYTES = 60_000;
const DEFAULT_TIMEOUT_SECONDS = 30 * 60;

const commandArraySchema = {
  type: "array",
  minItems: 1,
  items: {
    type: "string",
    minLength: 1,
  },
};

const envSchema = {
  type: "object",
  additionalProperties: {
    type: "string",
  },
};

const providerSchema = {
  type: "string",
  enum: [
    "aws",
    "azure",
    "gcp",
    "google",
    "google-cloud",
    "hetzner",
    "proxmox",
    "ssh",
    "static",
    "static-ssh",
    "blacksmith-testbox",
    "blacksmith",
    "namespace-devbox",
    "namespace",
    "namespace-devboxes",
    "semaphore",
    "sem",
    "sprites",
    "daytona",
    "islo",
    "e2b",
    "modal",
    "tensorlake",
    "tl",
    "tensorlake-sbx",
    "cloudflare",
    "cf",
  ],
};

function readConfig(api) {
  const raw = api?.pluginConfig && typeof api.pluginConfig === "object" ? api.pluginConfig : {};
  return {
    binary: readString(raw, "binary") ?? DEFAULT_BINARY,
    maxOutputBytes: readPositiveInteger(raw, "maxOutputBytes", DEFAULT_MAX_OUTPUT_BYTES),
    timeoutSeconds: readPositiveInteger(raw, "timeoutSeconds", DEFAULT_TIMEOUT_SECONDS),
    allowRun: readBoolean(raw, "allowRun", true),
    allowWarmup: readBoolean(raw, "allowWarmup", true),
    allowStop: readBoolean(raw, "allowStop", true),
  };
}

function readString(source, key) {
  const value = source?.[key];
  return typeof value === "string" && value.trim() ? value.trim() : undefined;
}

function readBoolean(source, key, fallback) {
  const value = source?.[key];
  return typeof value === "boolean" ? value : fallback;
}

function readPositiveInteger(source, key, fallback) {
  const value = source?.[key];
  return typeof value === "number" && Number.isFinite(value) && value > 0
    ? Math.floor(value)
    : fallback;
}

function readStringArray(source, key) {
  const value = source?.[key];
  if (!Array.isArray(value) || value.length === 0) {
    throw new Error(`${key} must be a non-empty string array`);
  }
  const next = value.map((item) => {
    if (typeof item !== "string" || !item.trim()) {
      throw new Error(`${key} must contain only non-empty strings`);
    }
    return item;
  });
  return next;
}

function readEnv(source) {
  const value = source?.env;
  if (value === undefined) {
    return {};
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("env must be an object of string values");
  }
  const entries = Object.entries(value).map(([key, entry]) => {
    if (typeof entry !== "string") {
      throw new Error(`env.${key} must be a string`);
    }
    return [key, entry];
  });
  return Object.fromEntries(entries);
}

function maybePush(args, flag, value) {
  if (value !== undefined) {
    args.push(flag, value);
  }
}

function maybePushBool(args, flag, value) {
  if (value === true) {
    args.push(flag);
  }
}

function toolResult(text, details) {
  return {
    content: [{ type: "text", text }],
    details,
  };
}

function commandLine(binary, args) {
  return [binary, ...args].map((part) => (/\s/.test(part) ? JSON.stringify(part) : part)).join(" ");
}

function appendChunk(current, chunk, maxBytes) {
  if (!chunk) {
    return current;
  }
  const next = current + chunk;
  if (Buffer.byteLength(next, "utf8") <= maxBytes) {
    return next;
  }
  return next.slice(0, maxBytes) + "\n[truncated]\n";
}

function runCrabbox(config, args, options = {}) {
  const timeoutSeconds = options.timeoutSeconds ?? config.timeoutSeconds;
  const maxOutputBytes = config.maxOutputBytes;
  return new Promise((resolve, reject) => {
    const child = spawn(config.binary, args, {
      env: { ...process.env, ...options.env },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    let didTimeout = false;
    const timer = setTimeout(() => {
      didTimeout = true;
      child.kill("SIGTERM");
    }, timeoutSeconds * 1000);
    const abort = () => child.kill("SIGTERM");
    options.signal?.addEventListener("abort", abort, { once: true });

    child.stdout?.setEncoding("utf8");
    child.stderr?.setEncoding("utf8");
    child.stdout?.on("data", (chunk) => {
      stdout = appendChunk(stdout, chunk, maxOutputBytes);
    });
    child.stderr?.on("data", (chunk) => {
      stderr = appendChunk(stderr, chunk, maxOutputBytes);
    });
    child.on("error", (error) => {
      clearTimeout(timer);
      options.signal?.removeEventListener("abort", abort);
      reject(error);
    });
    child.on("close", (code, signal) => {
      clearTimeout(timer);
      options.signal?.removeEventListener("abort", abort);
      const result = {
        ok: code === 0 && !didTimeout,
        code,
        signal,
        timedOut: didTimeout,
        stdout,
        stderr,
        command: commandLine(config.binary, args),
      };
      resolve(result);
    });
  });
}

function formatResult(result) {
  const parts = [`$ ${result.command}`, `exit=${result.code ?? "signal"}${result.signal ? ` signal=${result.signal}` : ""}`];
  if (result.timedOut) {
    parts.push("timed out");
  }
  if (result.stdout.trim()) {
    parts.push(`stdout:\n${result.stdout.trimEnd()}`);
  }
  if (result.stderr.trim()) {
    parts.push(`stderr:\n${result.stderr.trimEnd()}`);
  }
  return parts.join("\n\n");
}

async function execute(config, args, params, signal) {
  const timeoutSeconds = readPositiveInteger(params, "timeoutSeconds", config.timeoutSeconds);
  const result = await runCrabbox(config, args, {
    env: readEnv(params),
    signal,
    timeoutSeconds,
  });
  return toolResult(formatResult(result), result);
}

function registerRun(api, config) {
  api.registerTool({
    name: "crabbox_run",
    description: "Run a command on an existing Crabbox lease after syncing the current repository.",
    parameters: {
      type: "object",
      additionalProperties: false,
      required: ["id", "command"],
      properties: {
        id: {
          type: "string",
          description: "Crabbox lease ID or friendly slug.",
        },
        command: commandArraySchema,
        provider: providerSchema,
        env: envSchema,
        noSync: {
          type: "boolean",
          description: "Pass --no-sync.",
        },
        syncOnly: {
          type: "boolean",
          description: "Pass --sync-only.",
        },
        forceSyncLarge: {
          type: "boolean",
          description: "Pass --force-sync-large.",
        },
        checksum: {
          type: "boolean",
          description: "Pass --checksum.",
        },
        debug: {
          type: "boolean",
          description: "Pass --debug.",
        },
        reclaim: {
          type: "boolean",
          description: "Pass --reclaim.",
        },
        junit: {
          type: "string",
          description: "Comma-separated remote JUnit XML paths.",
        },
        timeoutSeconds: {
          type: "number",
          description: "Local wrapper timeout for this Crabbox CLI invocation.",
        },
      },
    },
    async execute(_toolCallId, params, signal) {
      if (!config.allowRun) {
        throw new Error("crabbox_run is disabled by plugin config");
      }
      const args = ["run", "--id", readString(params, "id")];
      maybePush(args, "--provider", readString(params, "provider"));
      maybePushBool(args, "--no-sync", params?.noSync);
      maybePushBool(args, "--sync-only", params?.syncOnly);
      maybePushBool(args, "--force-sync-large", params?.forceSyncLarge);
      maybePushBool(args, "--checksum", params?.checksum);
      maybePushBool(args, "--debug", params?.debug);
      maybePushBool(args, "--reclaim", params?.reclaim);
      maybePush(args, "--junit", readString(params, "junit"));
      args.push("--", ...readStringArray(params, "command"));
      return execute(config, args, params, signal);
    },
  });
}

function registerWarmup(api, config) {
  api.registerTool({
    name: "crabbox_warmup",
    description: "Provision or reuse a Crabbox lease and wait until it is ready.",
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        provider: providerSchema,
        profile: { type: "string" },
        class: { type: "string" },
        type: {
          type: "string",
          description: "Provider server or instance type.",
        },
        ttl: {
          type: "string",
          description: "Maximum lease lifetime, for example 90m.",
        },
        idleTimeout: {
          type: "string",
          description: "Idle timeout, for example 30m.",
        },
        keep: {
          type: "boolean",
          description: "Pass --keep.",
        },
        actionsRunner: {
          type: "boolean",
          description: "Pass --actions-runner.",
        },
        reclaim: {
          type: "boolean",
          description: "Pass --reclaim.",
        },
        timeoutSeconds: {
          type: "number",
          description: "Local wrapper timeout for this Crabbox CLI invocation.",
        },
      },
    },
    async execute(_toolCallId, params, signal) {
      if (!config.allowWarmup) {
        throw new Error("crabbox_warmup is disabled by plugin config");
      }
      const args = ["warmup"];
      maybePush(args, "--provider", readString(params, "provider"));
      maybePush(args, "--profile", readString(params, "profile"));
      maybePush(args, "--class", readString(params, "class"));
      maybePush(args, "--type", readString(params, "type"));
      maybePush(args, "--ttl", readString(params, "ttl"));
      maybePush(args, "--idle-timeout", readString(params, "idleTimeout"));
      maybePushBool(args, "--keep", params?.keep);
      maybePushBool(args, "--actions-runner", params?.actionsRunner);
      maybePushBool(args, "--reclaim", params?.reclaim);
      return execute(config, args, params, signal);
    },
  });
}

function registerStatus(api, config) {
  api.registerTool({
    name: "crabbox_status",
    description: "Read the current state for a Crabbox lease.",
    parameters: {
      type: "object",
      additionalProperties: false,
      required: ["id"],
      properties: {
        id: { type: "string" },
        provider: providerSchema,
        wait: { type: "boolean" },
        waitTimeout: {
          type: "string",
          description: "Maximum wait duration, for example 10m.",
        },
        json: { type: "boolean" },
        timeoutSeconds: {
          type: "number",
          description: "Local wrapper timeout for this Crabbox CLI invocation.",
        },
      },
    },
    async execute(_toolCallId, params, signal) {
      const args = ["status", "--id", readString(params, "id")];
      maybePush(args, "--provider", readString(params, "provider"));
      maybePushBool(args, "--wait", params?.wait);
      maybePush(args, "--wait-timeout", readString(params, "waitTimeout"));
      maybePushBool(args, "--json", params?.json);
      return execute(config, args, params, signal);
    },
  });
}

function registerList(api, config) {
  api.registerTool({
    name: "crabbox_list",
    description: "List current Crabbox machines.",
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        provider: providerSchema,
        json: { type: "boolean" },
        refresh: {
          type: "boolean",
          description: "Pass --refresh.",
        },
        timeoutSeconds: {
          type: "number",
          description: "Local wrapper timeout for this Crabbox CLI invocation.",
        },
      },
    },
    async execute(_toolCallId, params, signal) {
      const args = ["list"];
      maybePush(args, "--provider", readString(params, "provider"));
      maybePushBool(args, "--json", params?.json);
      maybePushBool(args, "--refresh", params?.refresh);
      return execute(config, args, params, signal);
    },
  });
}

function registerStop(api, config) {
  api.registerTool({
    name: "crabbox_stop",
    description: "Stop a kept Crabbox lease by ID or friendly slug.",
    parameters: {
      type: "object",
      additionalProperties: false,
      required: ["id"],
      properties: {
        id: { type: "string" },
        provider: providerSchema,
        timeoutSeconds: {
          type: "number",
          description: "Local wrapper timeout for this Crabbox CLI invocation.",
        },
      },
    },
    async execute(_toolCallId, params, signal) {
      if (!config.allowStop) {
        throw new Error("crabbox_stop is disabled by plugin config");
      }
      const args = ["stop"];
      maybePush(args, "--provider", readString(params, "provider"));
      args.push(readString(params, "id"));
      return execute(config, args, params, signal);
    },
  });
}

export default {
  id: PLUGIN_ID,
  name: "Crabbox",
  description: "Run Crabbox remote testbox checks from OpenClaw.",
  register(api) {
    const config = readConfig(api);
    registerRun(api, config);
    registerWarmup(api, config);
    registerStatus(api, config);
    registerList(api, config);
    registerStop(api, config);
    api.logger?.info?.("Crabbox plugin registered");
  },
};
