// GitHub-compatible git forge entrypoint.
//
// Spawns the vendored `forge` binary (a statically linked Go program that
// lives next to this file) and forwards its stdio. The binary owns the whole
// lifecycle: it binds an ephemeral port, registers with the environment,
// serves git smart HTTP, REST v3, GraphQL v4 and MCP from an in-memory
// store, polls for the end of the trial, and exits 0 when told to.
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const dir = dirname(fileURLToPath(import.meta.url));
const binary = join(dir, "forge");
const child = spawn(binary, [], { stdio: "inherit" });

child.on("error", (err) => {
  console.error(`failed to start forge: ${err.message}`);
  process.exit(1);
});

child.on("exit", (code, signal) => {
  if (signal) {
    process.exit(1);
  }
  process.exit(code ?? 1);
});
