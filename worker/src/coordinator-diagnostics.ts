import { errorMessage, redactDiagnosticSecrets } from "./http";
import { coordinatorProviderRegistry, type Env } from "./types";

export function coordinatorErrorMessage(env: Env, error: unknown): string {
  return errorMessage(error, coordinatorDiagnosticSecrets(env));
}

export function coordinatorDiagnosticText(env: Env, value: string): string {
  return redactDiagnosticSecrets(value, coordinatorDiagnosticSecrets(env));
}

function coordinatorDiagnosticSecrets(env: Env): Array<string | undefined> {
  return [
    ...coordinatorProviderRegistry.flatMap((provider) =>
      provider.requiredSecrets.map((name) => env[name]),
    ),
    env.AWS_SESSION_TOKEN,
    env.CRABBOX_RUNTIME_ADAPTER_TOKEN,
    env.CRABBOX_SHARED_TOKEN,
    env.CRABBOX_ADMIN_TOKEN,
    env.CRABBOX_SESSION_SECRET,
    env.CRABBOX_GITHUB_CLIENT_SECRET,
    env.CRABBOX_WORKSPACE_SSH_PRIVATE_KEY,
    env.CRABBOX_TRUSTED_PROXY_SECRET,
    env.CRABBOX_TAILSCALE_CLIENT_SECRET,
    env.CRABBOX_ARTIFACTS_ACCESS_KEY_ID,
    env.CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY,
    env.CRABBOX_ARTIFACTS_SESSION_TOKEN,
  ];
}
