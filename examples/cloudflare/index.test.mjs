import test from 'node:test';
import assert from 'node:assert/strict';
import {webcrypto} from 'node:crypto';
import worker from './index.mjs';
globalThis.crypto ??= webcrypto;
const secret = 'a'.repeat(64);
const url = `https://pins.example/hooks/certificates/${secret}`;
const payload = {version: 1, certificates: [{algorithm: 'sha-256', value: 'A'.repeat(43)+'=', notBefore: '2026-11-03T00:00:00Z', notAfter: '2026-11-16T00:00:00Z'}]};
test('private updates are stored before acknowledgement and exposed without the secret', async () => {
  let stored;
  const env = {WEBHOOK_SECRET: secret, PINS: {put: async (_, value) => {stored = value;}, get: async () => stored}};
  const request = () => new Request(url, {method: 'PUT', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)});
  assert.equal((await worker.fetch(request(), env)).status, 204);
  assert.equal((await worker.fetch(request(), env)).status, 204);
  const get = await worker.fetch(new Request('https://pins.example/certificates'), env);
  assert.deepEqual(await get.json(), payload);
  assert.ok(!stored.includes(secret));
  assert.equal(get.headers.get('Cache-Control'), 'no-store');
  assert.equal((await worker.fetch(new Request(url+'wrong', {method: 'PUT'}), env)).status, 404);
  assert.equal((await worker.fetch(new Request(url, {method: 'POST'}), env)).status, 404);
});
test('invalid payloads are rejected and storage failures never produce success', async () => {
  const env = {WEBHOOK_SECRET: secret, PINS: {put: async () => {throw new Error('storage failed');}}};
  const req = body => new Request(url, {method: 'PUT', headers: {'Content-Type': 'application/json'}, body});
  assert.equal((await worker.fetch(req('{}'), env)).status, 400);
  assert.equal((await worker.fetch(req('x'.repeat(8193)), env)).status, 413);
  await assert.rejects(worker.fetch(req(JSON.stringify(payload)), env), /storage failed/);
});
