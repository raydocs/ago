import assert from 'node:assert/strict';
import http from 'node:http';
import test from 'node:test';
import {
  COMPACTION_MARKERS,
  createProxyServer,
  isNativeCompactionRequest,
  rewriteMessagesBody,
} from './model-filter-proxy.mjs';

function compactBody(content = COMPACTION_MARKERS[0]) {
  return {
    model: 'gpt-5.6-sol',
    max_tokens: 32000,
    thinking: { type: 'enabled', budget_tokens: 128 },
    messages: [
      { role: 'user', content: 'normal work' },
      { role: 'assistant', content: 'result' },
      { role: 'user', content },
    ],
  };
}

test('detects full and recent native compact prompts only in the last user message', () => {
  assert.equal(isNativeCompactionRequest(compactBody()), true);
  assert.equal(isNativeCompactionRequest(compactBody([
    { type: 'text', text: `${COMPACTION_MARKERS[1]} — preserve recent work.` },
  ])), true);

  const ordinary = compactBody('continue implementing the UI');
  ordinary.messages[0].content = COMPACTION_MARKERS[0];
  assert.equal(isNativeCompactionRequest(ordinary), false);

  const assistantLast = compactBody();
  assistantLast.messages.at(-1).role = 'assistant';
  assert.equal(isNativeCompactionRequest(assistantLast), false);
});

test('rewrites only Sol compact requests and preserves effort/thinking fields', () => {
  const input = compactBody();
  const routed = rewriteMessagesBody(input);
  assert.equal(routed.routed, true);
  assert.equal(routed.fromModel, 'gpt-5.6-sol');
  assert.equal(routed.toModel, 'gpt-5.6-luna');
  assert.equal(routed.body.model, 'gpt-5.6-luna');
  assert.deepEqual(routed.body.thinking, input.thinking);
  assert.equal(input.model, 'gpt-5.6-sol');

  const ordinary = rewriteMessagesBody(compactBody('normal request'));
  assert.equal(ordinary.routed, false);
  assert.equal(ordinary.body.model, 'gpt-5.6-sol');

  const grok = compactBody();
  grok.model = 'grok-4.5';
  const otherModel = rewriteMessagesBody(grok);
  assert.equal(otherModel.routed, false);
});

async function listen(server) {
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  return server.address().port;
}

async function close(server) {
  await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
}

test('proxy routes compact to Luna, annotates response, and leaves ordinary calls unchanged', async t => {
  const received = [];
  const upstream = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', chunk => chunks.push(chunk));
    req.on('end', () => {
      const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
      received.push(body);
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ model: body.model }));
    });
  });
  const upstreamPort = await listen(upstream);
  const events = [];
  const proxy = createProxyServer({
    upstream: { hostname: '127.0.0.1', port: upstreamPort },
    logger: { log: value => events.push(value), warn: value => events.push(value) },
  });
  const proxyPort = await listen(proxy);
  t.after(async () => {
    await close(proxy);
    await close(upstream);
  });

  const compactResponse = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(compactBody()),
  });
  assert.equal(compactResponse.status, 200);
  assert.equal(compactResponse.headers.get('x-claudex-route-role'), 'compaction');
  assert.equal(compactResponse.headers.get('x-claudex-original-model'), 'gpt-5.6-sol');
  assert.equal(compactResponse.headers.get('x-claudex-routed-model'), 'gpt-5.6-luna');
  assert.equal((await compactResponse.json()).model, 'gpt-5.6-luna');

  const ordinaryResponse = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(compactBody('continue')),
  });
  assert.equal(ordinaryResponse.headers.get('x-claudex-route-role'), null);
  assert.equal((await ordinaryResponse.json()).model, 'gpt-5.6-sol');
  assert.deepEqual(received.map(body => body.model), ['gpt-5.6-luna', 'gpt-5.6-sol']);
  assert.equal(events.length, 1);
  assert.doesNotMatch(events[0], /normal work|explicit requests/);
});

test('model picker filter remains active', async t => {
  const upstream = http.createServer((req, res) => {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ data: [
      { id: 'claude-opus-4-8' },
      { id: 'gpt-5.6-luna' },
      { id: 'claude-sonnet-5' },
    ] }));
  });
  const upstreamPort = await listen(upstream);
  const proxy = createProxyServer({ upstream: { hostname: '127.0.0.1', port: upstreamPort } });
  const proxyPort = await listen(proxy);
  t.after(async () => {
    await close(proxy);
    await close(upstream);
  });

  const response = await fetch(`http://127.0.0.1:${proxyPort}/v1/models`);
  const body = await response.json();
  assert.deepEqual(body.data, [{ id: 'gpt-5.6-luna' }]);
});
