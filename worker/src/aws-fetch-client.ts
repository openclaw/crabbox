import { AwsClient } from "aws4fetch";

import {
  currentAWSTransportObserver,
  type AWSTransportObservation,
} from "./aws-provisioning-diagnostics";
import type { AWSCredentialProvider } from "./types";

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
  ) {}

  async fetch(
    input: string,
    init?: RequestInit,
    stopRetrying?: StopAWSResponseRetry,
  ): Promise<Response> {
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
      const credentials = await this.credentials();
      const accessKeyId = credentials.accessKeyId?.trim();
      const secretAccessKey = credentials.secretAccessKey?.trim();
      if (!accessKeyId || !secretAccessKey) {
        throw new Error("AWS credential provider returned incomplete credentials");
      }
      const options: ConstructorParameters<typeof AwsClient>[0] = {
        accessKeyId,
        secretAccessKey,
        service: this.service,
        region: this.region,
      };
      const session = credentials.sessionToken?.trim();
      if (session) options.sessionToken = session;
      // aws4fetch 1.0.20 calls public sign() once per retry-loop invocation.
      const client = observe ? new ObservedAwsClient(options, observation) : new AwsClient(options);
      requestStartedAt = Date.now();
      observation.credentialsMs = Math.max(0, requestStartedAt - startedAt);
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
