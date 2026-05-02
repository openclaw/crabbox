import { Mppx, Store, tempo } from "mppx/server";

import type { Env } from "./types";

const PATH_USD_TEMPO = "0x20c0000000000000000000000000000000000000";

export type ChargeResult =
  | { status: 402; challenge: Response }
  | { withReceipt: (response: Response) => Response };

export interface PaymentGuard {
  charge(amount: string): (request: Request) => Promise<ChargeResult>;
}

export function isChallenge(result: ChargeResult): result is { status: 402; challenge: Response } {
  return "challenge" in result;
}

export class MppxConfigError extends Error {}

export function paymentEnabled(env: Env): boolean {
  return Boolean(env.CRABBOX_MPP_RECIPIENT?.trim());
}

export function paymentGuardFromEnv(
  env: Env,
  storage?: DurableObjectStorage,
): PaymentGuard | undefined {
  const recipient = env.CRABBOX_MPP_RECIPIENT?.trim();
  if (!recipient) {
    return undefined;
  }
  if (!isAddress(recipient)) {
    throw new MppxConfigError("CRABBOX_MPP_RECIPIENT must be a 0x… 20-byte address");
  }
  const currency = env.CRABBOX_MPP_CURRENCY?.trim() || PATH_USD_TEMPO;
  if (!isAddress(currency)) {
    throw new MppxConfigError("CRABBOX_MPP_CURRENCY must be a 0x… 20-byte address");
  }
  if (!env.CRABBOX_MPP_SECRET_KEY?.trim()) {
    throw new MppxConfigError("CRABBOX_MPP_SECRET_KEY is required when MPP is enabled");
  }
  if (!env.CRABBOX_SESSION_SECRET?.trim()) {
    throw new MppxConfigError(
      "CRABBOX_SESSION_SECRET is required when MPP is enabled (used to sign lease bearers)",
    );
  }
  const decimals = parseDecimals(env.CRABBOX_MPP_DECIMALS) ?? 6;
  const testnet = parseBool(env.CRABBOX_MPP_TESTNET);
  const tempoConfig: {
    currency: `0x${string}`;
    recipient: `0x${string}`;
    decimals: number;
    testnet?: boolean;
    store?: Store.AtomicStore;
  } = { currency, recipient, decimals };
  if (testnet) {
    tempoConfig.testnet = true;
  }
  if (storage) {
    tempoConfig.store = doStorageStore(storage);
  }
  const secretKey = env.CRABBOX_MPP_SECRET_KEY;
  const realm = env.CRABBOX_MPP_REALM?.trim();
  const mppx = realm
    ? Mppx.create({ methods: [tempo.charge(tempoConfig)], secretKey, realm })
    : Mppx.create({ methods: [tempo.charge(tempoConfig)], secretKey });
  return {
    charge: (amount: string) => async (request: Request) => {
      const response = await mppx.charge({ amount })(request);
      if (response.status === 402) {
        return { status: 402, challenge: response.challenge };
      }
      return { withReceipt: (out: Response) => response.withReceipt(out) };
    },
  };
}

export function paymentConfigured(env: Env): boolean {
  if (!paymentEnabled(env)) {
    return false;
  }
  try {
    return Boolean(paymentGuardFromEnv(env));
  } catch {
    return false;
  }
}

export function formatAmountUSD(amount: number): string {
  if (!Number.isFinite(amount) || amount <= 0) {
    return "0.000001";
  }
  return amount.toFixed(6);
}

function parseDecimals(value: string | undefined): number | undefined {
  if (!value) {
    return undefined;
  }
  const parsed = Number.parseInt(value, 10);
  return Number.isFinite(parsed) && parsed >= 0 && parsed <= 32 ? parsed : undefined;
}

function parseBool(value: string | undefined): boolean {
  return value === "1" || value === "true" || value === "yes";
}

function isAddress(value: string): value is `0x${string}` {
  return /^0x[0-9a-fA-F]{40}$/.test(value);
}

// DO storage is single-threaded per object, so naive read-modify-write inside
// `update` is atomic by definition; no explicit transaction is needed.
function doStorageStore(storage: DurableObjectStorage): Store.AtomicStore {
  const prefix = "mpp:";
  return Store.cloudflare({
    get: async (key: string) => (await storage.get<string>(prefix + key)) ?? null,
    put: async (key: string, value: string) => {
      await storage.put(prefix + key, value);
    },
    delete: async (key: string) => {
      await storage.delete(prefix + key);
    },
    update: async (key, fn) => {
      const k = prefix + key;
      const current = (await storage.get<string>(k)) ?? null;
      const change = fn(current);
      if (change.op === "set") {
        await storage.put(k, change.value);
      } else if (change.op === "delete") {
        await storage.delete(k);
      }
      return change.result;
    },
  });
}
