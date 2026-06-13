export class WorkerEntrypoint<Env = unknown, Props = unknown> {
  env: Env;
  ctx: { props: Props };

  constructor(env: Env, props: Props) {
    this.env = env;
    this.ctx = { props };
  }
}

export class DurableObject<Env = unknown> {
  ctx: { storage: unknown };
  env: Env;

  constructor(ctx: { storage: unknown }, env: Env) {
    this.ctx = ctx;
    this.env = env;
  }
}
