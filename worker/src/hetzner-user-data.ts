import { bytesToBase64 } from "./encoding";

export async function hetznerUserData(cloudConfig: string): Promise<string> {
  const limit = 32768;
  if (new TextEncoder().encode(cloudConfig).length <= limit) return cloudConfig;
  const stream = new Blob([cloudConfig]).stream().pipeThrough(new CompressionStream("gzip"));
  const compressed = new Uint8Array(await new Response(stream).arrayBuffer());
  const encoded = bytesToBase64(compressed);
  // Cloud-init decodes MIME transfer encoding, then detects the gzipped cloud-config.
  const envelope =
    "MIME-Version: 1.0\nContent-Type: application/gzip\nContent-Transfer-Encoding: base64\n\n" +
    encoded.match(/.{1,76}/g)!.join("\n") +
    "\n";
  if (envelope.length > limit) {
    throw new Error(
      `Hetzner user_data limit is ${limit} bytes; compressed cloud-init MIME is ${envelope.length} bytes`,
    );
  }
  return envelope;
}
