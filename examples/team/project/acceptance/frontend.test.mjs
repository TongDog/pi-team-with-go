import test from 'node:test';
import assert from 'node:assert/strict';
import { loadUsers, renderUsers } from '../frontend/users.mjs';

test('names render safely', () => {
  const html = renderUsers([{ id: 1, name: 'Ada' }, { id: 2, name: '<script>&' }]);
  assert.match(html, /<ul[\s>]/);
  assert.match(html, /Ada/);
  assert.match(html, /&lt;script&gt;&amp;/);
  assert.doesNotMatch(html, /<script>/);
});

test('failed responses reject', async () => {
  const original = globalThis.fetch;
  globalThis.fetch = async () => ({ ok: false, status: 503, json: async () => ({ items: [], total: 0 }) });
  try { await assert.rejects(() => loadUsers('http://example.invalid')); }
  finally { globalThis.fetch = original; }
});
