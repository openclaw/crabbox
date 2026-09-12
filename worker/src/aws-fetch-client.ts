import { AwsClient } from "aws4fetch";

import {
  currentAWSTransportObserver,
  type AWSTransportObservation,
} from "./aws-provisioning-diagnostics";
import type { AWSCredentials, AWSCredentialProvider } from "./types";

export interface ResolvedAWSCredentials {
  readonly accessKeyId: string;
  readonly secretAccessKey: string;
  readonly sessionToken?: string;
  readonly expirationMs?: number;
}

export function resolvedAWSCredentials(credentials: AWSCredentials): ResolvedAWSCredentials {
  const accessKeyId = credentials.accessKeyId?.trim();
  const secretAccessKey = credentials.secretAccessKey?.trim();
  if (!accessKeyId || !secretAccessKey) {
    throw new Error("AWS credential provider returned incomplete credentials");
  }
  const sessionToken = credentials.sessionToken?.trim();
  const expirationMs = credentials.expiration?.getTime();
  if (expirationMs !== undefined && !Number.isFinite(expirationMs)) {
    throw new Error("AWS credential provider returned an invalid expiration");
  }
  return {
    accessKeyId,
    secretAccessKey,
    ...(sessionToken ? { sessionToken } : {}),
    ...(expirationMs === undefined ? {} : { expirationMs }),
  };
}

type StopAWSResponseRetry = (response: Response) => Promise<boolean>;

export interface AWSFetchClient {
  fetch(input: string, init?: RequestInit, stopRetrying?: StopAWSResponseRetry): Promise<Response>;
}

class ObservedAwsClient extends AwsClient {
  #observation: AWSTransportObservation;

  constructor(
    options: ConstructorParameters<typeof AwsClient>[0],
    observation: AWSTransportObservation,
  ) {
    super(options);
    this.#observation = observation;
  }

  override async sign(...args: Parameters<AwsClient["sign"]>): ReturnType<AwsClient["sign"]> {
    const startedAt = Date.now();
    this.#observation.signInvocations += 1;
    try {
      const request = await super.sign(...args);
      this.#observation.signCompletions += 1;
      return request;
    } catch (error) {
      this.#observation.signFailures += 1;
      throw error;
    } finally {
      this.#observation.signMs += Math.max(0, Date.now() - startedAt);
    }
  }
}

export class RefreshingAWSFetchClient implements AWSFetchClient {
  constructor(
    private readonly credentials: AWSCredentialProvider,
    private readonly service: string,
    private readonly region: string,
    private readonly signal?: AbortSignal,
  ) {}

  async fetch(
    input: string,
    init?: RequestInit,
    stopRetrying?: StopAWSResponseRetry,
  ): Promise<Response> {
    this.signal?.throwIfAborted();
    const observe = currentAWSTransportObserver();
    const observation: AWSTransportObservation = {
      requests: 1,
      credentialsMs: 0,
      credentialFailures: 0,
      signInvocations: 0,
      signCompletions: 0,
      signFailures: 0,
      signMs: 0,
      requestMs: 0,
      requestFailures: 0,
    };
    const startedAt = Date.now();
    let requestStartedAt: number | undefined;
    try {
      const providedCredentials = await this.credentials();
      this.signal?.throwIfAborted();
      const credentials = resolvedAWSCredentials(providedCredentials);
      if (credentials.expirationMs !== undefined && credentials.expirationMs <= Date.now()) {
        throw new Error("AWS credential snapshot expired");
      }
      const options: ConstructorParameters<typeof AwsClient>[0] = {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        service: this.service,
        region: this.region,
        ...(this.signal ? { retries: 0 } : {}),
      };
      const session = credentials.sessionToken?.trim();
      if (session) options.sessionToken = session;
      // aws4fetch 1.0.20 calls public sign() once per retry-loop invocation.
      const client = observe ? new ObservedAwsClient(options, observation) : new AwsClient(options);
      requestStartedAt = Date.now();
      observation.credentialsMs = Math.max(0, requestStartedAt - startedAt);
      if (this.signal) {
        const request = await client.sign(input, { ...init, signal: this.signal });
        this.signal.throwIfAborted();
        // Keep the quote's owning signal through signing and response-body consumption.
        return await fetch(request, { signal: this.signal });
      }
      if (!stopRetrying) return await client.fetch(input, init);
      // aws4fetch has no response-policy hook. Preserve its budget, jitter, signing and
      // thrown errors while allowing the operation owner to handle a definitive rejection.
      /* oxlint-disable eslint/no-await-in-loop -- Each signed attempt and its backoff must settle before retry or handoff. */
      for (let attempt = 0; ; attempt += 1) {
        const response = await fetch(await client.sign(input, init));
        if (
          attempt === client.retries ||
          (response.status < 500 && response.status !== 429) ||
          (await stopRetrying(response))
        ) {
          return response;
        }
        await new Promise((resolve) =>
          setTimeout(resolve, Math.random() * client.initRetryMs * 2 ** attempt),
        );
      }
      /* oxlint-enable eslint/no-await-in-loop */
    } catch (error) {
      if (requestStartedAt === undefined) observation.credentialFailures += 1;
      else observation.requestFailures += 1;
      throw error;
    } finally {
      if (requestStartedAt === undefined)
        observation.credentialsMs = Math.max(0, Date.now() - startedAt);
      else observation.requestMs = Math.max(0, Date.now() - requestStartedAt);
      observe?.(observation);
    }
  }
}

// Regional operations must retain the exact identity verified before their first mutation.
// Reuse the transport owner so fixed credentials preserve diagnostics and response retry policy.
export class FixedAWSFetchClient extends RefreshingAWSFetchClient {
  constructor(
    credentials: ResolvedAWSCredentials,
    service: string,
    region: string,
    signal?: AbortSignal,
  ) {
    super(
      async () => ({
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        ...(credentials.sessionToken ? { sessionToken: credentials.sessionToken } : {}),
        ...(credentials.expirationMs === undefined
          ? {}
          : { expiration: new Date(credentials.expirationMs) }),
      }),
      service,
      region,
      signal,
    );
  }
}
