import { AwsClient } from "aws4fetch";

import {
  currentAWSTransportObserver,
  type AWSTransportObservation,
} from "./aws-provisioning-diagnostics";
import {
  providerFetch,
  providerResponse,
  providerRequestSignal,
  providerRequestTimeoutMs,
  providerSleep,
  waitForProviderSignal,
} from "./provider-deadline";
import type { AWSCredentials, AWSCredentialProvider } from "./types";

export const awsRequestTimeoutMs = providerRequestTimeoutMs;

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
  #signing: Promise<Request> | undefined;

  constructor(
    options: ConstructorParameters<typeof AwsClient>[0],
    observation: AWSTransportObservation,
  ) {
    super(options);
    this.#observation = observation;
  }

  override sign(...args: Parameters<AwsClient["sign"]>): ReturnType<AwsClient["sign"]> {
    this.#signing = this.signObserved(...args);
    return this.#signing;
  }

  async settleSigning(): Promise<void> {
    await this.#signing?.catch(() => undefined);
  }

  private async signObserved(
    ...args: Parameters<AwsClient["sign"]>
  ): ReturnType<AwsClient["sign"]> {
    const startedAt = Date.now();
    this.#observation.signInvocations += 1;
    try {
      const signal = args[1]?.signal;
      const request = signal
        ? await waitForProviderSignal(signal, () => super.sign(...args))
        : await super.sign(...args);
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
    private readonly timeoutMs = awsRequestTimeoutMs,
  ) {}

  async fetch(
    input: string,
    init?: RequestInit,
    stopRetrying?: StopAWSResponseRetry,
  ): Promise<Response> {
    init?.signal?.throwIfAborted();
    const callerSignal = init?.signal;
    const signal = providerRequestSignal(this.timeoutMs, callerSignal, "aws", this.service);
    const boundedInit = { ...init, signal };
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
    let observedClient: ObservedAwsClient | undefined;
    try {
      const credentials = resolvedAWSCredentials(
        await waitForProviderSignal(signal, () => this.credentials()),
      );
      signal.throwIfAborted();
      if (credentials.expirationMs !== undefined && credentials.expirationMs <= Date.now()) {
        throw new Error("AWS credential snapshot expired");
      }
      const options: ConstructorParameters<typeof AwsClient>[0] = {
        accessKeyId: credentials.accessKeyId,
        secretAccessKey: credentials.secretAccessKey,
        service: this.service,
        region: this.region,
      };
      const session = credentials.sessionToken?.trim();
      if (session) options.sessionToken = session;
      // aws4fetch 1.0.20 calls public sign() once per retry-loop invocation.
      if (observe) observedClient = new ObservedAwsClient(options, observation);
      const client = observedClient ?? new AwsClient(options);
      requestStartedAt = Date.now();
      observation.credentialsMs = Math.max(0, requestStartedAt - startedAt);
      const sign = () =>
        observe
          ? client.sign(input, boundedInit)
          : waitForProviderSignal(signal, () => client.sign(input, boundedInit));
      if (callerSignal) {
        // Optional quotes own a single attempt, including signing and the body read.
        const request = await sign();
        return await providerFetch(request, undefined, signal);
      }
      // Keep SDK dispatch/signing, but own its retry budget so backoff can be aborted.
      // The response-policy path still hands definitive rejections to the operation owner.
      const retries = client.retries;
      client.retries = 0;
      /* oxlint-disable eslint/no-await-in-loop -- Each signed attempt and its backoff must settle before retry or handoff. */
      for (let attempt = 0; ; attempt += 1) {
        const response = await waitForProviderSignal(signal, () =>
          client.fetch(input, boundedInit),
        );
        if (
          attempt === retries ||
          (response.status < 500 && response.status !== 429) ||
          (stopRetrying && (await waitForProviderSignal(signal, () => stopRetrying(response))))
        ) {
          return providerResponse(response, signal);
        }
        await providerSleep(Math.random() * client.initRetryMs * 2 ** attempt, signal);
      }
      /* oxlint-enable eslint/no-await-in-loop */
    } catch (error) {
      if (requestStartedAt === undefined) observation.credentialFailures += 1;
      else observation.requestFailures += 1;
      throw error;
    } finally {
      // A timeout can reject SDK fetch before its bounded signer unwinds.
      await observedClient?.settleSigning();
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
    timeoutMs = awsRequestTimeoutMs,
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
      timeoutMs,
    );
  }
}
