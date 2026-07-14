import http from 'node:http';
import { pathToFileURL } from 'node:url';

export const DEFAULT_UPSTREAM = Object.freeze({ hostname: '127.0.0.1', port: 8317 });
export const DEFAULT_LISTEN = Object.freeze({ hostname: '127.0.0.1', port: 8318 });
export const DEFAULT_COMPACTION_MODEL = 'gpt-5.6-luna';
export const DEFAULT_MAX_BUFFER_BYTES = 256 * 1024 * 1024;

export const COMPACTION_MARKERS = Object.freeze([
  "Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions.",
  'Your task is to create a detailed summary of the RECENT portion of the conversation',
]);

const hiddenFromPicker = new Set([
  'claude-opus-4-8',
  'claude-sonnet-5',
]);

function contentText(content) {
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) return '';
  return content
    .filter(block => block && block.type === 'text' && typeof block.text === 'string')
    .map(block => block.text)
    .join('\n');
}

/**
 * Claude Code 2.1.208 appends its private summary request as the last user
 * message. Match only that position and the exact native prompt prefix so a
 * quoted marker elsewhere in the transcript cannot change routing.
 */
export function isNativeCompactionRequest(body) {
  if (!body || typeof body !== 'object' || !Array.isArray(body.messages)) return false;
  const last = body.messages.at(-1);
  if (!last || last.role !== 'user') return false;
  const text = contentText(last.content).trimStart();
  return COMPACTION_MARKERS.some(marker => text.startsWith(marker));
}

export function rewriteMessagesBody(body, {
  compactionModel = DEFAULT_COMPACTION_MODEL,
  sourceModels = ['gpt-5.6-sol'],
} = {}) {
  const fromModel = typeof body?.model === 'string' ? body.model : '';
  if (!sourceModels.includes(fromModel) || !isNativeCompactionRequest(body)) {
    return { body, routed: false, fromModel, toModel: fromModel };
  }

  return {
    body: { ...body, model: compactionModel },
    routed: true,
    fromModel,
    toModel: compactionModel,
  };
}

function responseHandler(req, res, { routed = false, fromModel = '', toModel = '' } = {}) {
  return proxyRes => {
    if (req.method === 'GET' && req.url?.split('?')[0] === '/v1/models') {
      const chunks = [];
      proxyRes.on('data', chunk => chunks.push(chunk));
      proxyRes.on('end', () => {
        try {
          const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
          if (Array.isArray(body.data)) {
            body.data = body.data.filter(model => !hiddenFromPicker.has(model.id));
          }
          const output = Buffer.from(JSON.stringify(body));
          const headers = { ...proxyRes.headers, 'content-length': String(output.length) };
          delete headers['content-encoding'];
          delete headers['transfer-encoding'];
          res.writeHead(proxyRes.statusCode ?? 200, headers);
          res.end(output);
        } catch (error) {
          res.writeHead(502, { 'content-type': 'application/json' });
          res.end(JSON.stringify({ error: { message: `model filter failed: ${error.message}` } }));
        }
      });
      return;
    }

    const headers = { ...proxyRes.headers };
    if (routed) {
      headers['x-claudex-route-role'] = 'compaction';
      headers['x-claudex-original-model'] = fromModel;
      headers['x-claudex-routed-model'] = toModel;
    }
    res.writeHead(proxyRes.statusCode ?? 502, headers);
    proxyRes.pipe(res);
  };
}

function proxyError(res, error) {
  if (!res.headersSent) res.writeHead(502, { 'content-type': 'application/json' });
  res.end(JSON.stringify({ error: { message: `CLIProxyAPI unavailable: ${error.message}` } }));
}

function upstreamRequest(req, res, upstream, headers, route = {}) {
  const proxyReq = http.request({
    hostname: upstream.hostname,
    port: upstream.port,
    method: req.method,
    path: req.url,
    headers: { ...headers, host: `${upstream.hostname}:${upstream.port}` },
  }, responseHandler(req, res, route));
  proxyReq.on('error', error => proxyError(res, error));
  return proxyReq;
}

function streamUnchanged(req, res, upstream) {
  const proxyReq = upstreamRequest(req, res, upstream, req.headers);
  req.pipe(proxyReq);
}

function forwardBuffered(req, res, upstream, body, route) {
  const headers = { ...req.headers, 'content-length': String(body.length) };
  delete headers['transfer-encoding'];
  const proxyReq = upstreamRequest(req, res, upstream, headers, route);
  proxyReq.end(body);
}

function handleMessages(req, res, options) {
  const declaredLength = Number(req.headers['content-length']);
  if (Number.isFinite(declaredLength) && declaredLength > options.maxBufferBytes) {
    options.logger.warn(JSON.stringify({
      event: 'claudex_compaction_route_skipped',
      reason: 'body_above_buffer_limit',
      bytes: declaredLength,
    }));
    streamUnchanged(req, res, options.upstream);
    return;
  }

  const chunks = [];
  let bytes = 0;
  let rejected = false;

  req.on('data', chunk => {
    if (rejected) return;
    bytes += chunk.length;
    if (bytes > options.maxBufferBytes) {
      rejected = true;
      res.writeHead(413, { 'content-type': 'application/json', connection: 'close' });
      res.end(JSON.stringify({ error: { message: 'Claude X gateway request body exceeded the local routing buffer' } }));
      req.destroy();
      return;
    }
    chunks.push(chunk);
  });

  req.on('end', () => {
    if (rejected) return;
    const original = Buffer.concat(chunks);
    let route = { routed: false, fromModel: '', toModel: '' };
    let output = original;
    try {
      const parsed = JSON.parse(original.toString('utf8'));
      route = rewriteMessagesBody(parsed, options);
      if (route.routed) output = Buffer.from(JSON.stringify(route.body));
    } catch {
      // Preserve malformed or non-JSON bodies exactly; upstream owns validation.
    }

    if (route.routed) {
      options.logger.log(JSON.stringify({
        event: 'claudex_compaction_routed',
        from_model: route.fromModel,
        to_model: route.toModel,
        request_bytes: original.length,
        timestamp: new Date().toISOString(),
      }));
    }
    forwardBuffered(req, res, options.upstream, output, route);
  });
}

export function createProxyServer({
  upstream = DEFAULT_UPSTREAM,
  compactionModel = DEFAULT_COMPACTION_MODEL,
  sourceModels = ['gpt-5.6-sol'],
  maxBufferBytes = DEFAULT_MAX_BUFFER_BYTES,
  logger = console,
} = {}) {
  const options = { upstream, compactionModel, sourceModels, maxBufferBytes, logger };
  return http.createServer((req, res) => {
    const path = req.url?.split('?')[0];
    if (req.method === 'POST' && path === '/v1/messages') {
      handleMessages(req, res, options);
      return;
    }
    streamUnchanged(req, res, upstream);
  });
}

function main() {
  const upstream = {
    hostname: process.env.CLAUDEX_UPSTREAM_HOST ?? DEFAULT_UPSTREAM.hostname,
    port: Number(process.env.CLAUDEX_UPSTREAM_PORT ?? DEFAULT_UPSTREAM.port),
  };
  const listen = {
    hostname: process.env.CLAUDEX_LISTEN_HOST ?? DEFAULT_LISTEN.hostname,
    port: Number(process.env.CLAUDEX_LISTEN_PORT ?? DEFAULT_LISTEN.port),
  };
  const compactionModel = process.env.CLAUDEX_COMPACTION_MODEL ?? DEFAULT_COMPACTION_MODEL;
  const server = createProxyServer({ upstream, compactionModel });
  server.listen(listen.port, listen.hostname, () => {
    console.log(`Claude X gateway listening on ${listen.hostname}:${listen.port}; compact -> ${compactionModel}`);
  });
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
