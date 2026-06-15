import { normalizeCoordinatorURL, webViewOriginWhitelist } from '../src/coordinatorURL';

function expectEqual(actual: unknown, expected: unknown, label: string) {
  if (actual !== expected) {
    throw new Error(`${label}: expected ${String(expected)}, got ${String(actual)}`);
  }
}

function expectArrayEqual(actual: string[], expected: string[], label: string) {
  const actualValue = JSON.stringify(actual);
  const expectedValue = JSON.stringify(expected);
  if (actualValue !== expectedValue) {
    throw new Error(`${label}: expected ${expectedValue}, got ${actualValue}`);
  }
}

expectEqual(
  normalizeCoordinatorURL('crabbox.sh/team?token=redacted#section'),
  'https://crabbox.sh/team',
  'normalizes bare HTTPS coordinators',
);

expectEqual(
  normalizeCoordinatorURL('https://broker.example.com////'),
  'https://broker.example.com',
  'trims trailing slash-only paths',
);

expectEqual(
  normalizeCoordinatorURL('http://broker.example.com'),
  null,
  'rejects production HTTP coordinators',
);

expectEqual(
  normalizeCoordinatorURL('http://localhost:8787'),
  null,
  'rejects localhost HTTP outside development',
);

expectEqual(
  normalizeCoordinatorURL('http://localhost:8787', { allowLocalHTTP: true }),
  'http://localhost:8787',
  'allows localhost HTTP in development',
);

expectEqual(
  normalizeCoordinatorURL('http://127.0.0.1:8787', { allowLocalHTTP: true }),
  'http://127.0.0.1:8787',
  'allows IPv4 loopback HTTP in development',
);

expectEqual(
  normalizeCoordinatorURL('http://[::1]:8787', { allowLocalHTTP: true }),
  'http://[::1]:8787',
  'allows IPv6 loopback HTTP in development',
);

expectEqual(
  normalizeCoordinatorURL('http://192.168.1.50:8787', { allowLocalHTTP: true }),
  null,
  'rejects LAN HTTP even in development',
);

expectArrayEqual(
  webViewOriginWhitelist('https://crabbox.sh'),
  ['https://*', 'about:*'],
  'does not whitelist arbitrary HTTP origins',
);

expectArrayEqual(
  webViewOriginWhitelist('http://localhost:8787'),
  ['https://*', 'about:*', 'http://localhost:8787'],
  'whitelists only the active local HTTP origin',
);
