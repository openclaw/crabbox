// This protocol transports one finalized archive, separately from workload logs.
export const artifactProtocol = "CBX-CF-ARTIFACT-1";
export const artifactMaxBytes = 10 * 1024 * 1024;
const collectionTimeoutMs = 35_000;

export class ArtifactCollectionFailure extends Error {}

export async function collectContainerArtifacts(
  container: Container,
  cwd: string,
  script: string,
  callerSignal: AbortSignal,
  stopProcess: (process: ExecProcess) => Promise<void>,
): Promise<Uint8Array<ArrayBuffer>> {
  const directory = `/tmp/crabbox-artifacts-${crypto.randomUUID()}`;
  const controller = new AbortController();
  let running: ExecProcess | undefined;
  let stopping: Promise<void> | undefined;
  const stop = () => {
    controller.abort();
    if (running) {
      const process = running;
      running = undefined;
      stopping = stopProcess(process).catch(() => undefined);
    }
  };
  const timer = setTimeout(stop, collectionTimeoutMs);
  callerSignal.addEventListener("abort", stop, { once: true });
  if (callerSignal.aborted) stop();
  try {
    controller.signal.throwIfAborted();
    running = await container.exec(
      [
        "/bin/sh",
        "-c",
        'test "$(pwd -P)" = "$2" && umask 077 && mkdir -- "$1" && exec timeout --kill-after=5s 30s /bin/bash -s -- "$1/archive.tgz"',
        "crabbox-artifacts",
        directory,
        cwd,
      ],
      {
        cwd,
        stdin: new Blob([script]).stream(),
        stdout: "ignore",
        stderr: "ignore",
        signal: controller.signal,
      },
    );
    controller.signal.throwIfAborted();
    const code = await running.exitCode;
    running = undefined;
    controller.signal.throwIfAborted();
    if (code !== 0) {
      throw new ArtifactCollectionFailure(
        code === 8 ? "missing required artifact" : `artifact collection exited ${code}`,
      );
    }
    running = await container.exec(
      [
        "/bin/sh",
        "-c",
        `test -f "$1" && test ! -L "$1" && exec head -c ${artifactMaxBytes + 1} -- "$1"`,
        "crabbox-artifact-read",
        `${directory}/archive.tgz`,
      ],
      { stdout: "pipe", stderr: "ignore", signal: controller.signal },
    );
    controller.signal.throwIfAborted();
    const [bytes, exitCode] = await Promise.all([
      readArtifactBytes(running.stdout, controller.signal),
      running.exitCode,
    ]);
    running = undefined;
    controller.signal.throwIfAborted();
    if (exitCode !== 0) throw new Error("artifact archive read failed");
    return bytes;
  } finally {
    stop();
    clearTimeout(timer);
    callerSignal.removeEventListener("abort", stop);
    await stopping;
    // Remove only this request's random transfer directory, including when a
    // reused lease or failed workload is retained.
    try {
      const cleanup = await container.exec(["rm", "-rf", "--", directory], {
        stdout: "ignore",
        stderr: "ignore",
        signal: AbortSignal.timeout(5_000),
      });
      await cleanup.exitCode;
    } catch {
      // Container destruction also removes these transient transfer files.
    }
  }
}

async function readArtifactBytes(
  stream: ReadableStream<Uint8Array> | null,
  signal: AbortSignal,
): Promise<Uint8Array<ArrayBuffer>> {
  if (!stream) throw new Error("missing artifact body");
  const reader = stream.getReader();
  const abort = () => void reader.cancel().catch(() => undefined);
  signal.addEventListener("abort", abort, { once: true });
  const output = new Uint8Array(artifactMaxBytes);
  let size = 0;
  try {
    for (;;) {
      signal.throwIfAborted();
      // oxlint-disable-next-line eslint/no-await-in-loop -- bounded sequential stream consumption.
      const { done, value } = await reader.read();
      signal.throwIfAborted();
      if (done) break;
      if (value.byteLength > artifactMaxBytes - size) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- cancel the rejected bounded stream.
        await reader.cancel();
        throw new Error("artifact archive exceeds byte limit");
      }
      output.set(value, size);
      size += value.byteLength;
    }
    if (size === 0) throw new Error("empty artifact archive");
    return output.slice(0, size);
  } finally {
    signal.removeEventListener("abort", abort);
    reader.releaseLock();
  }
}
