export class AWSQueryError extends Error {
  retryAfter?: string | undefined;
  constructor(
    readonly action: string,
    readonly status: number,
    readonly code: string,
    detail: string,
  ) {
    super(`aws ${action}: http ${status}: ${detail}`);
  }
}

export function isAWSGuestCreateRejectedNoEffect(error: unknown): boolean {
  return (
    error instanceof AWSQueryError &&
    error.action === "CreateSecurityGroup" &&
    error.status === 403 &&
    error.code === "UnauthorizedOperation"
  );
}
