import { Mppx, tempo } from "mppx/server";

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

export function paymentGuardFromEnv(env: Env): PaymentGuard | undefined {
  const recipient = env.CRABBOX_MPP_RECIPIENT?.trim();
  if (!recipient || !isAddress(recipient)) {
    return undefined;
  }
  const currency = env.CRABBOX_MPP_CURRENCY?.trim() || PATH_USD_TEMPO;
  if (!isAddress(currency)) {
    return undefined;
  }
  const decimals = parseDecimals(env.CRABBOX_MPP_DECIMALS) ?? 6;
  const testnet = parseBool(env.CRABBOX_MPP_TESTNET);
  const tempoConfig: {
    currency: `0x${string}`;
    recipient: `0x${string}`;
    decimals: number;
    testnet?: boolean;
  } = { currency, recipient, decimals };
  if (testnet) {
    tempoConfig.testnet = true;
  }
  const mppx = Mppx.create({
    methods: [tempo.charge(tempoConfig)],
    secretKey: env.CRABBOX_MPP_SECRET_KEY,
  });
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
