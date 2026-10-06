import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer } from 'node:net';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { loadUsers, renderUsers } from '../frontend/users.mjs';

test('actual backend and frontend satisfy the shared contract', { timeout: 80000 }, async () => {
  const temp = mkdtempSync(join(tmpdir(), 'team-users-'));
  let child;
  try {
    const binary = join(temp, process.platform === 'win32' ? 'server.exe' : 'server');
    execFileSync('go', ['build', '-o', binary, '.'], {
      cwd: fileURLToPath(new URL('../backend', import.meta.url)), timeout: 60000, stdio: 'pipe'
    });
    const reservation = createServer();
    reservation.listen(0, '127.0.0.1');
    await once(reservation, 'listening');
    const port = reservation.address().port;
    await new Promise((resolve, reject) => reservation.close(error => error ? reject(error) : resolve()));
    const base = `http://127.0.0.1:${port}`;
    child = spawn(binary, [], { cwd: fileURLToPath(new URL('../backend', import.meta.url)), env: { ...process.env, TEAM_DEMO_ADDR: `127.0.0.1:${port}` }, stdio: 'ignore' });
    let ready = false;
    for (let n = 0; n < 50; n++) {
      if (child.exitCode !== null) throw new Error('backend exited before readiness');
      try { if ((await fetch(`${base}/health`)).ok) { ready = true; break; } } catch {}
      await delay(100);
    }
    assert.ok(ready, 'backend must become ready');
    const page = await fetch(`${base}/`);
    assert.equal(page.status, 200);
    assert.match(await page.text(), /users.mjs/);
    const response = await fetch(`${base}/api/users`);
    assert.equal(response.status, 200);
    assert.match(response.headers.get('content-type') ?? '', /application\/json/);
    const expected = { items: [{ id: 1, name: 'Ada' }, { id: 2, name: 'Lin' }], total: 2 };
    assert.deepEqual(await response.json(), expected);
    const result = await loadUsers(base);
    assert.deepEqual(result, expected);
    const html = renderUsers(result.items);
    assert.match(html, /Ada/);
    assert.match(html, /Lin/);
  } finally {
    if (child && child.exitCode === null) {
      const closed = once(child, 'exit');
      child.kill();
      await closed;
    }
    rmSync(temp, { recursive: true, force: true });
  }
});
