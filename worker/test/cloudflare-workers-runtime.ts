export class WorkerEntrypoint<Env = unknown, Props = unknown> {
  env: Env;
  ctx: { props: Props };

  constructor(env: Env, props: Props) {
    this.env = env;
    this.ctx = { props };
  }
}
