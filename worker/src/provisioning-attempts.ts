import type { ProvisioningAttempt } from "./types";

// Ordinary exhaustion carries evidence across region boundaries, never allocation authority.
export class ProvisioningAttemptsError extends Error {
  readonly attempts: readonly Readonly<ProvisioningAttempt>[];

  constructor(message: string, attempts: readonly ProvisioningAttempt[], options?: ErrorOptions) {
    super(message, options);
    this.name = "ProvisioningAttemptsError";
    this.attempts = Object.freeze(attempts.map((attempt) => Object.freeze({ ...attempt })));
  }
}

// Providers still own candidate order, classification, redaction, and permission to retry.
export class ProvisioningAttemptHistory {
  private readonly entries: ProvisioningAttempt[] = [];
  private readonly diagnostics: string[] = [];

  record(attempt: ProvisioningAttempt, diagnostic: string): void {
    this.entries.push({ ...attempt });
    this.diagnostics.push(diagnostic);
  }

  recordFailure(
    error: unknown,
    fallback: ProvisioningAttempt,
    diagnostic: string,
    preceding: readonly ProvisioningAttempt[] = [],
  ): void {
    this.entries.push(...preceding.map((attempt) => ({ ...attempt })));
    if (error instanceof ProvisioningAttemptsError && error.attempts.length > 0) {
      this.entries.push(...error.attempts.map((attempt) => ({ ...attempt })));
      this.diagnostics.push(diagnostic);
    } else {
      this.record(fallback, diagnostic);
    }
  }

  result(additional: readonly ProvisioningAttempt[] = []): { attempts?: ProvisioningAttempt[] } {
    const attempts = [...this.entries, ...additional].map((attempt) => ({ ...attempt }));
    return attempts.length > 0 ? { attempts } : {};
  }

  error(prefix = "", options?: ErrorOptions): ProvisioningAttemptsError {
    return new ProvisioningAttemptsError(
      prefix + this.diagnostics.join("; "),
      this.entries,
      options,
    );
  }
}
