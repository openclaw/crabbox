import { build } from "esbuild";

await build({
  entryPoints: ["node/server.ts"],
  outfile: "dist-node/server.mjs",
  bundle: true,
  packages: "external",
  platform: "node",
  format: "esm",
  target: "node22",
  sourcemap: true,
});
