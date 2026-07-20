// Test harness: builds and runs a real ago-server against throwaway state.
//
// Two safety properties matter here and are enforced rather than assumed:
//
//   1. Nothing the suite runs may touch the canonical repository. The fixture
//      repository, database, and artifact root are all created under the OS
//      temporary directory, and startFixtureRepository refuses to hand back a
//      path inside the repository this suite lives in.
//   2. Every server is spawned into its own process group and killed as a
//      group on teardown, including when a test fails. A Go binary that spawns
//      children would otherwise leak them past the run.

import { spawn, execFileSync } from "node:child_process";
import { mkdtemp, rm, writeFile, mkdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
export const repositoryRoot = path.resolve(here, "..", "..");

let builtBinary = null;

/** Builds ago-server once per process, into a temporary directory. */
export async function ensureServerBinary() {
  if (builtBinary) return builtBinary;
  const dir = await mkdtemp(path.join(tmpdir(), "ago-e2e-bin-"));
  const target = path.join(dir, "ago-server");
  execFileSync("go", ["build", "-o", target, "./cmd/ago-server"], {
    cwd: repositoryRoot,
    stdio: "pipe",
  });
  builtBinary = target;
  return target;
}

/**
 * Creates a throwaway fixture repository. It is deliberately outside the
 * canonical checkout: a real executor writing here must never be able to reach
 * the repository under test.
 */
export async function makeFixtureRepository() {
  const root = await mkdtemp(path.join(tmpdir(), "ago-e2e-repo-"));
  const resolved = path.resolve(root);
  if (resolved.startsWith(repositoryRoot + path.sep) || resolved === repositoryRoot) {
    throw new Error(`refusing to use a fixture repository inside the canonical checkout: ${resolved}`);
  }
  await mkdir(path.join(resolved, "docs"), { recursive: true });
  await writeFile(path.join(resolved, "README.md"), "# fixture\n", "utf8");
  return resolved;
}

/**
 * Starts an ago-server on an ephemeral loopback port.
 *
 * @param {{scenario?: string, env?: Record<string,string>}} options
 * @returns {Promise<{baseURL: string, stop: () => Promise<void>, stderr: () => string, stdout: () => string, repositoryRoot: string, stateDir: string}>}
 */
export async function startServer(options = {}) {
  const binary = await ensureServerBinary();
  const stateDir = await mkdtemp(path.join(tmpdir(), "ago-e2e-state-"));
  const fixture = await makeFixtureRepository();

  const child = spawn(
    binary,
    [
      "--db", path.join(stateDir, "ago.db"),
      "--listen", "127.0.0.1:0",
      "--scenario", options.scenario || "success",
    ],
    {
      cwd: stateDir,
      // Its own process group, so teardown can signal the whole tree.
      detached: true,
      stdio: ["ignore", "pipe", "pipe"],
      env: { ...process.env, ...(options.env || {}) },
    },
  );

  let out = "";
  let err = "";
  child.stdout.on("data", (chunk) => { out += chunk.toString(); });
  child.stderr.on("data", (chunk) => { err += chunk.toString(); });

  const baseURL = await new Promise((resolve, reject) => {
    const deadline = setTimeout(() => reject(new Error(`ago-server did not report a listen address.\nstdout: ${out}\nstderr: ${err}`)), 60_000);
    const check = setInterval(() => {
      const match = out.match(/UI:\s+(http:\/\/127\.0\.0\.1:\d+)/);
      if (match) {
        clearInterval(check);
        clearTimeout(deadline);
        resolve(match[1]);
      }
    }, 50);
    child.on("exit", (code) => {
      clearInterval(check);
      clearTimeout(deadline);
      reject(new Error(`ago-server exited early with code ${code}.\nstdout: ${out}\nstderr: ${err}`));
    });
  });

  let stopped = false;
  const stop = async () => {
    if (stopped) return;
    stopped = true;
    try {
      // Negative pid signals the whole process group.
      process.kill(-child.pid, "SIGTERM");
    } catch (_) {
      // Already gone.
    }
    await new Promise((resolve) => {
      const timer = setTimeout(() => {
        try { process.kill(-child.pid, "SIGKILL"); } catch (_) { /* already gone */ }
        resolve();
      }, 5_000);
      child.on("exit", () => { clearTimeout(timer); resolve(); });
      if (child.exitCode !== null) { clearTimeout(timer); resolve(); }
    });
    await rm(stateDir, { recursive: true, force: true });
    await rm(fixture, { recursive: true, force: true });
  };

  return {
    baseURL,
    repositoryRoot: fixture,
    stateDir,
    stop,
    stdout: () => out,
    stderr: () => err,
  };
}

/**
 * withServer runs a body with a server, guaranteeing teardown on any outcome.
 * Playwright fixtures already do this, but a raw helper keeps the guarantee
 * explicit for anything outside the fixture lifecycle.
 */
export async function withServer(options, body) {
  const server = await startServer(options);
  try {
    return await body(server);
  } finally {
    await server.stop();
  }
}
