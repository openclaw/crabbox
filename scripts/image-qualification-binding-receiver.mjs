import { WorkerEntrypoint } from "cloudflare:workers";

// No state, credentials, or authority binding. This entrypoint only echoes the
// public synthetic configuration supplied by the protected fixture uploader.
export class BindingProbe extends WorkerEntrypoint {
  describe() {
    return { protocol: "qualification-binding-probe-v1", props: this.ctx.props };
  }
}

export default {
  fetch() {
    return new Response(null, { status: 403 });
  },
};
