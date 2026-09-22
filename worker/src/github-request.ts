const githubAPIURL = "https://api.github.com";
const verificationTimeoutMS = 15_000;

export class GitHubTransientError extends Error {}

export class GitHubRequestDeadline {
  private readonly controller = new AbortController();
  private readonly timer: ReturnType<typeof setTimeout>;
  private readonly expired: Promise<never>;

  constructor() {
    let expire!: (error: GitHubTransientError) => void;
    this.expired = new Promise((_, reject) => {
      expire = reject;
    });
    this.timer = setTimeout(() => {
      const error = new GitHubTransientError("GitHub verification timed out.");
      expire(error);
      this.controller.abort(error);
    }, verificationTimeoutMS);
  }

  async wait<T>(operation: () => Promise<T>): Promise<T> {
    this.controller.signal.throwIfAborted();
    return await Promise.race([operation(), this.expired]);
  }

  fetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    return this.wait(() => fetch(input, { ...init, signal: this.controller.signal }));
  }

  api(path: string, accessToken: string): Promise<Response> {
    return this.fetch(`${githubAPIURL}${path}`, {
      headers: {
        accept: "application/vnd.github+json",
        authorization: `Bearer ${accessToken}`,
        "user-agent": "crabbox-coordinator",
        "x-github-api-version": "2022-11-28",
      },
    });
  }

  json<T>(response: Response): Promise<T> {
    return this.wait(() => response.json() as Promise<T>);
  }

  close(): void {
    clearTimeout(this.timer);
    this.controller.abort();
  }
}

export async function withGitHubRequestDeadline<T>(
  operation: (deadline: GitHubRequestDeadline) => Promise<T>,
): Promise<T> {
  const deadline = new GitHubRequestDeadline();
  try {
    return await deadline.wait(() => operation(deadline));
  } finally {
    deadline.close();
  }
}
