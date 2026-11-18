import { spawn } from "node:child_process";
import { chmodSync, copyFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// The vendored forge binary sits beside this file. Its path is resolved
// from this file's own directory so the entry works from any cwd. A fresh
// checkout stores it without the exec bit (git mode 100644), so it must be
// made executable before it can be spawned.
const forgePath = join(dirname(fileURLToPath(import.meta.url)), "forge");

// Fast path: make the vendored binary itself executable. On the server the
// code is mounted read-only (/bot:ro under --read-only), so this throws
// there; fall back to running a copy from the writable /tmp tmpfs instead.
let target = forgePath;
try {
  chmodSync(forgePath, 0o755);
} catch {
  try {
    target = join(tmpdir(), `forge-${process.pid}`);
    copyFileSync(forgePath, target);
    chmodSync(target, 0o755);
  } catch {
    // Last resort: try the original as-is and let the spawn decide.
    target = forgePath;
  }
}

const child = spawn(target, [], { stdio: "inherit" });

child.on("error", (err) => {
  console.error(`failed to start forge: ${err.message}`);
  process.exit(1);
});

child.on("exit", (code, signal) => {
  if (signal) process.exit(1);
  process.exit(code ?? 1);
});
