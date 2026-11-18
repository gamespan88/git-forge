// The forge's HTTP server: one process serving git smart HTTP (/<owner>/<repo>.git/...), the
// REST v3 API (/api/v3/...), GraphQL v4 (POST /api/graphql) and MCP (POST /mcp), all over the
// single in-memory store in store.ts. app.ts owns the wire frame every surface shares — the
// bearer-token check, the JSON body parse, the X-GitHub-Request-Id header and the JSON 404 —
// and routes to the surface modules; main.ts owns the entry <-> env lifecycle and stays as the
// starter shipped it.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Forge } from './store.ts';
import { handleRest } from './rest.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './githttp.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const MAX_BODY_BYTES = 256 * 1024 * 1024;

/** An error that maps to an HTTP status with a GitHub-shaped {message} body. */
export class HttpError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/** Everything a surface handler needs: the store, the request frame and the answer helpers. */
export interface Ctx {
  readonly forge: Forge;
  readonly req: IncomingMessage;
  readonly res: ServerResponse;
  readonly url: URL;
  readonly body: Buffer;
  /** The entry's own origin as the caller addressed it (for absolute pagination links). */
  readonly origin: string;
  /** The parsed JSON body; a malformed body is a 400, as the wire contract requires. */
  json(): unknown;
  send(status: number, body: unknown): void;
  sendRaw(status: number, body: Buffer): void;
  sendNoContent(): void;
  fail(status: number, message: string): void;
  notFound(): void;
}

let requestCounter = 0;

async function readBody(req: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let total = 0;
  for await (const chunk of req) {
    const buffer = chunk as Buffer;
    total += buffer.length;
    if (total > MAX_BODY_BYTES) throw new HttpError(413, 'Request body too large');
    chunks.push(buffer);
  }
  return Buffer.concat(chunks);
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): Server {
  const forge = new Forge();
  const token = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      requestCounter += 1;
      res.setHeader('X-GitHub-Request-Id', `forge-${requestCounter.toString(36)}-${Date.now().toString(36)}`);
      try {
        const url = new URL(req.url ?? '/', 'http://entry');
        const path = url.pathname;
        const method = req.method ?? 'GET';

        // The env's boot check calls this twice: 200 {"ok":true}. It carries no token.
        if (method === 'GET' && path === '/healthz') {
          res.writeHead(200, { 'content-type': JSON_TYPE });
          res.end('{"ok":true}\n');
          return;
        }

        const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);

        // Every other surface requires the trial's bearer token.
        if (token !== '' && req.headers.authorization !== `Bearer ${token}`) {
          throw new HttpError(401, 'Bad credentials');
        }

        const host = req.headers.host ?? 'entry';
        const ctx: Ctx = {
          forge,
          req,
          res,
          url,
          body,
          origin: `http://${host}`,
          json(): unknown {
            try {
              return JSON.parse(body.toString('utf8'));
            } catch {
              throw new HttpError(400, 'Problems parsing JSON');
            }
          },
          send(status: number, value: unknown): void {
            const text = `${JSON.stringify(value)}\n`;
            if (!res.hasHeader('Content-Type')) res.setHeader('Content-Type', JSON_TYPE);
            res.setHeader('Content-Length', Buffer.byteLength(text));
            res.writeHead(status);
            res.end(text);
          },
          sendRaw(status: number, data: Buffer): void {
            res.setHeader('Content-Length', data.length);
            res.writeHead(status);
            res.end(data);
          },
          sendNoContent(): void {
            res.writeHead(204);
            res.end();
          },
          fail(status: number, message: string): void {
            ctx.send(status, { message });
          },
          notFound(): void {
            ctx.fail(404, 'Not Found');
          },
        };

        if (path === '/api/v3' || path.startsWith('/api/v3/')) {
          res.setHeader('X-OAuth-Scopes', 'repo, read:org');
          const segments = path
            .slice('/api/v3'.length)
            .split('/')
            .filter((segment) => segment !== '');
          handleRest(ctx, segments);
          return;
        }
        if (path === '/api/graphql' && method === 'POST') {
          handleGraphQL(ctx);
          return;
        }
        if (path === '/mcp' && method === 'POST') {
          handleMcp(ctx);
          return;
        }

        const gitMatch = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(path);
        if (gitMatch !== null) {
          const owner = decodeURIComponent(gitMatch[1] as string);
          const repoName = decodeURIComponent(gitMatch[2] as string);
          const endpoint = gitMatch[3] as string;
          if (endpoint === 'info/refs' && method === 'GET') handleInfoRefs(ctx, owner, repoName);
          else if (endpoint === 'git-upload-pack' && method === 'POST') handleUploadPack(ctx, owner, repoName);
          else if (endpoint === 'git-receive-pack' && method === 'POST') handleReceivePack(ctx, owner, repoName);
          else ctx.notFound();
          return;
        }

        ctx.notFound();
      } catch (err) {
        if (res.headersSent) {
          res.end();
          return;
        }
        if (err instanceof HttpError) {
          const text = `${JSON.stringify({ message: err.message })}\n`;
          res.writeHead(err.status, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text) });
          res.end(text);
          return;
        }
        const text = `${JSON.stringify({ message: err instanceof Error ? err.message : 'Internal error' })}\n`;
        res.writeHead(400, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text) });
        res.end(text);
      }
    })();
  });
  server.keepAliveTimeout = 75_000;
  server.headersTimeout = 80_000;
  return server;
}
