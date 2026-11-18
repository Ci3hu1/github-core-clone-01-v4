// Entry: spawn the static Go forge binary, register its port with the env,
// then poll until the env says done and exit 0.
//
// Lifecycle (the harness's, not the contest's):
//   1. MAKEFUN_ENV_URL names the env's origin. Without it, exit 1 at once.
//   2. Spawn ./forge (fallback ladder below). The forge binds an ephemeral
//      port — never 8080 — and prints "PORT <n>" on stdout.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. 200 is normal; 409 means
//      a first register already won. 400/405 are contract errors.
//   4. GET $MAKEFUN_ENV_URL/done until {"done": true}, polling with
//      exponential backoff; never exit before the env says done.
//   5. Stop the forge and exit 0.
//
// Spawn fallback ladder (the binary may not be executable where it sits):
//   direct exec -> copy to tmp and exec -> memfd via python3.

import { spawn, type ChildProcess } from 'node:child_process';
import { copyFileSync, chmodSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;
const PORT_TIMEOUT_MS = 10_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

const HERE = dirname(fileURLToPath(import.meta.url));
const FORGE_PATH = join(HERE, 'forge');

/** Wait for the child to print "PORT <n>" on stdout; resolve the port. */
function waitForPort(child: ChildProcess): Promise<number> {
  return new Promise((resolve, reject) => {
    let buf = '';
    const timer = setTimeout(() => {
      cleanup();
      reject(new Error('forge did not print its port in time'));
    }, PORT_TIMEOUT_MS);
    const onData = (chunk: Buffer): void => {
      buf += chunk.toString('utf8');
      const m = buf.match(/PORT (\d+)/);
      if (m) {
        cleanup();
        resolve(Number(m[1]));
      }
    };
    const onExit = (code: number | null): void => {
      cleanup();
      reject(new Error(`forge exited early with code ${code}`));
    };
    const cleanup = (): void => {
      clearTimeout(timer);
      child.stdout?.off('data', onData);
      child.off('exit', onExit);
    };
    child.stdout?.on('data', onData);
    child.once('exit', onExit);
  });
}

async function spawnForge(): Promise<{ child: ChildProcess; port: number }> {
  const attempts: Array<{ label: string; run: () => ChildProcess }> = [
    {
      label: 'direct exec',
      run: () => spawn(FORGE_PATH, [], { stdio: ['ignore', 'pipe', 'inherit'] }),
    },
    {
      label: 'copy to tmp',
      run: () => {
        const tmp = join(tmpdir(), `forge-${process.pid}`);
        copyFileSync(FORGE_PATH, tmp);
        chmodSync(tmp, 0o755);
        return spawn(tmp, [], { stdio: ['ignore', 'pipe', 'inherit'] });
      },
    },
    {
      label: 'memfd via python3',
      run: () =>
        spawn(
          'python3',
          [
            '-c',
            'import os,sys;fd=os.memfd_create("forge",0);src=open(sys.argv[1],"rb");\nwhile True:\n b=src.read(1<<20)\n if not b: break\n os.write(fd,b)\nos.fexecve(fd,["forge"],os.environ)',
            FORGE_PATH,
          ],
          { stdio: ['ignore', 'pipe', 'inherit'] },
        ),
    },
  ];
  let lastErr: unknown = null;
  for (const attempt of attempts) {
    let child: ChildProcess;
    try {
      child = attempt.run();
    } catch (err) {
      lastErr = err;
      continue;
    }
    try {
      const port = await waitForPort(child);
      process.stdout.write(`[entry] forge up via ${attempt.label} on port ${port}\n`);
      return { child, port };
    } catch (err) {
      lastErr = err;
      try {
        child.kill('SIGKILL');
      } catch {
        // already gone
      }
    }
  }
  throw new Error(`could not start the forge binary: ${lastErr instanceof Error ? lastErr.message : String(lastErr)}`);
}

async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: not done.
    }
    await sleep(retryDelay(attempt));
  }
}

async function main(): Promise<void> {
  if (!existsSync(FORGE_PATH)) throw new Error(`forge binary not found at ${FORGE_PATH}`);
  const { child, port } = await spawnForge();
  try {
    await register(port);
    process.stdout.write('[entry] registered; waiting for the env to say done\n');
    await waitForDone();
  } finally {
    child.kill('SIGTERM');
    await Promise.race([new Promise((r) => child.once('exit', r)), sleep(2000)]);
    try {
      child.kill('SIGKILL');
    } catch {
      // already gone
    }
  }
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
