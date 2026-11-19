import { spawn } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

const dir = dirname(fileURLToPath(import.meta.url));
const child = spawn(join(dir, 'forge'), process.argv.slice(2), {
  stdio: 'inherit',
  env: process.env,
});

child.on('exit', (code) => {
  process.exit(code ?? 1);
});

for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.on(signal, () => {
    child.kill(signal);
  });
}

child.on('error', (err) => {
  process.stderr.write(`${err.message}\n`);
  process.exit(1);
});
