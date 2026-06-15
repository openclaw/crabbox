export const defaultCoordinatorURL = 'https://crabbox.sh';

type NormalizeCoordinatorOptions = {
  allowLocalHTTP?: boolean;
};

export function normalizeCoordinatorURL(
  value: string,
  options: NormalizeCoordinatorOptions = {},
): string | null {
  const trimmed = value.trim();
  if (!trimmed) {
    return null;
  }

  const withProtocol = /^[a-z][a-z0-9+.-]*:\/\//i.test(trimmed)
    ? trimmed
    : `https://${trimmed}`;

  try {
    const url = new URL(withProtocol);
    if (url.protocol !== 'https:' && !isAllowedLocalHTTP(url, options)) {
      return null;
    }

    url.hash = '';
    url.search = '';
    url.pathname = url.pathname === '/' ? '' : url.pathname.replace(/\/+$/, '');

    return url.toString().replace(/\/$/, '');
  } catch {
    return null;
  }
}

export function webViewOriginWhitelist(coordinatorURL: string): string[] {
  const origins = ['https://*', 'about:*'];

  try {
    const url = new URL(coordinatorURL);
    if (url.protocol === 'http:' && isLoopbackHost(url.hostname)) {
      origins.push(url.origin);
    }
  } catch {
    // Keep the conservative HTTPS-only default.
  }

  return origins;
}

function isAllowedLocalHTTP(url: URL, options: NormalizeCoordinatorOptions): boolean {
  return options.allowLocalHTTP === true && url.protocol === 'http:' && isLoopbackHost(url.hostname);
}

function isLoopbackHost(hostname: string): boolean {
  const host = hostname.toLowerCase().replace(/^\[|\]$/g, '');

  if (host === 'localhost' || host === '::1') {
    return true;
  }

  return /^127(?:\.\d{1,3}){0,3}$/.test(host);
}
