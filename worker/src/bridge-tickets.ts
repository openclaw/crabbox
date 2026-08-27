import type { AuthContext, GitHubUserGrant } from "./auth";
import type { CoordinatorStorageView } from "./coordinator-runtime";
import type { LeaseRecord } from "./types";

export interface CachedAdminGrant {
  auth?: AuthContext["auth"];
  login?: string;
  adminTokenHash?: string;
  adminGrantVersion?: string;
}

export interface CachedBridgeGrant extends CachedAdminGrant {
  sharedTokenHash?: string;
  portalSessionHash?: string;
  githubGrant?: GitHubUserGrant;
}

export interface LeaseBridgeTicketRecord extends CachedBridgeGrant {
  ticket: string;
  leaseID: string;
  owner: string;
  org: string;
  admin?: boolean;
  createdAt: string;
  expiresAt: string;
}

export type EgressRole = "host" | "client";

interface EgressTicketRecord<R extends EgressRole> extends LeaseBridgeTicketRecord {
  role: R;
  sessionID: string;
  profile?: string;
  allow?: string[];
}

interface BridgeTicketRecords {
  "webvnc-agent": LeaseBridgeTicketRecord;
  "code-agent": LeaseBridgeTicketRecord;
  "egress-host": EgressTicketRecord<"host">;
  "egress-client": EgressTicketRecord<"client">;
}

export type BridgeTicketKind = keyof BridgeTicketRecords;
export type BridgeTicketRecord<K extends BridgeTicketKind> = BridgeTicketRecords[K];
type BridgeTicketInput<K extends BridgeTicketKind> = Omit<
  BridgeTicketRecord<K>,
  "ticket" | "createdAt" | "expiresAt"
>;

export type LeaseBridgeTicketConsumption<T> =
  | { status: "invalid" }
  | { status: "not_found" }
  | { status: "accepted"; ticket: T; lease: LeaseRecord };

interface TicketPolicy {
  namespace: string;
  tokenPrefix: string;
  format: RegExp;
  ttlSeconds: number;
  role?: EgressRole;
}

// Kinds are selected by the endpoint, not added to persisted records. Egress keeps its
// shared namespace and on-record role so tickets survive coordinator upgrades/restarts.
const ticketPolicies: Record<BridgeTicketKind, TicketPolicy> = {
  "webvnc-agent": {
    namespace: "webvnc-ticket:",
    tokenPrefix: "wvnc_",
    format: /^wvnc_[a-f0-9]{32}$/,
    ttlSeconds: 120,
  },
  "code-agent": {
    namespace: "code-ticket:",
    tokenPrefix: "code_",
    format: /^code_[a-f0-9]{32}$/,
    ttlSeconds: 120,
  },
  "egress-host": {
    namespace: "egress-ticket:",
    tokenPrefix: "egress_",
    format: /^egress_[a-f0-9]{32}$/,
    ttlSeconds: 120,
    role: "host",
  },
  "egress-client": {
    namespace: "egress-ticket:",
    tokenPrefix: "egress_",
    format: /^egress_[a-f0-9]{32}$/,
    ttlSeconds: 120,
    role: "client",
  },
};

export function validBridgeTicket(kind: BridgeTicketKind, value: string): boolean {
  return ticketPolicies[kind].format.test(value);
}

interface BridgeTicketContext {
  withLock<T>(operation: () => Promise<T>): Promise<T>;
  getLease(id: string): Promise<LeaseRecord | undefined>;
  identifierMatchesLease(identifier: string, lease: LeaseRecord): boolean;
  currentTicket<T extends LeaseBridgeTicketRecord>(
    ticket: T,
    lease: LeaseRecord,
  ): Promise<T | undefined>;
}

export class BridgeTickets {
  constructor(
    private readonly storage: CoordinatorStorageView,
    private readonly context: BridgeTicketContext,
  ) {}

  async create<K extends BridgeTicketKind>(
    kind: K,
    prepare: (now: Date) => Promise<BridgeTicketInput<NoInfer<K>> | Response>,
  ): Promise<BridgeTicketRecord<K> | Response> {
    const policy = ticketPolicies[kind];
    await this.cleanupExpired(policy.namespace);
    const now = new Date();
    // Egress binds its session after cleanup and before persistence; it may reject
    // a replaced session without minting a ticket or activating anything.
    const input = await prepare(now);
    if (input instanceof Response) return input;
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    const ticket = {
      ...input,
      ticket: `${policy.tokenPrefix}${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`,
      createdAt: now.toISOString(),
      expiresAt: new Date(now.getTime() + policy.ttlSeconds * 1000).toISOString(),
    } as BridgeTicketRecord<K>;
    await this.storage.put(`${policy.namespace}${ticket.ticket}`, ticket);
    return ticket;
  }

  consume<K extends BridgeTicketKind>(
    kind: K,
    value: string,
    identifier: string,
  ): Promise<LeaseBridgeTicketConsumption<BridgeTicketRecord<K>>> {
    return this.consumeAndUse(kind, value, identifier, async (result) => result);
  }

  consumeAndUse<K extends BridgeTicketKind, T>(
    kind: K,
    value: string,
    identifier: string,
    use: (result: LeaseBridgeTicketConsumption<BridgeTicketRecord<K>>) => Promise<T>,
  ): Promise<T> {
    // Keep the coordinator's existing lock through socket registration, not just
    // storage deletion. Grant checks can await I/O in both runtimes.
    return this.context.withLock(async () =>
      use(await this.consumeUnderLock(kind, value, identifier)),
    );
  }

  private async consumeUnderLock<K extends BridgeTicketKind>(
    kind: K,
    value: string,
    identifier: string,
  ): Promise<LeaseBridgeTicketConsumption<BridgeTicketRecord<K>>> {
    const policy = ticketPolicies[kind];
    if (!validBridgeTicket(kind, value)) return { status: "invalid" };
    const key = `${policy.namespace}${value}`;
    const ticket = await this.storage.get<BridgeTicketRecord<K>>(key);
    if (!ticket || ticket.ticket !== value) return { status: "invalid" };
    if (Date.parse(ticket.expiresAt) <= Date.now()) {
      await this.storage.delete(key);
      return { status: "invalid" };
    }
    // Binding mismatches intentionally preserve tickets; expired or revoked
    // grants and successful consumption delete them.
    if (policy.role && (!("role" in ticket) || ticket.role !== policy.role)) {
      return { status: "invalid" };
    }
    const lease = await this.context.getLease(ticket.leaseID);
    if (!lease || !this.context.identifierMatchesLease(identifier, lease)) {
      return { status: "not_found" };
    }
    const currentTicket = await this.context.currentTicket(ticket, lease);
    await this.storage.delete(key);
    if (!currentTicket) return { status: "invalid" };
    return { status: "accepted", ticket: currentTicket, lease };
  }

  private async cleanupExpired(namespace: string): Promise<void> {
    const tickets = await this.storage.list<LeaseBridgeTicketRecord>({ prefix: namespace });
    const now = Date.now();
    await Promise.all(
      [...tickets.entries()]
        .filter(([, ticket]) => Date.parse(ticket.expiresAt) <= now)
        .map(([key]) => this.storage.delete(key)),
    );
  }
}
