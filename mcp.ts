// The MCP endpoint: JSON-RPC 2.0 over streamable HTTP at POST /mcp. It speaks the modern
// 2026-07-28 revision through server/discover (no handshake, no session) and the legacy
// initialize handshake for older clients. The tools act on the same store as REST: an issue
// created here is the same object GET /api/v3 reads back, with the same ids.

import { Forge, Repo, VIEWER, squashMerge, type IssueRec, type PullRec } from './store.ts';
import { issueJson, pullJson, repoJson } from './rest.ts';
import type { Ctx } from './app.ts';

const SERVER_INFO = Object.freeze({ name: 'forge', version: '1.0.0' });
const SUPPORTED_VERSIONS = Object.freeze(['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05']);

interface ToolDef {
  name: string;
  description: string;
  properties: Record<string, { type: string; description: string }>;
  required: string[];
  run: (forge: Forge, args: Record<string, unknown>) => Record<string, unknown>;
}

class ToolError extends Error {}

function needRepo(forge: Forge, args: Record<string, unknown>): Repo {
  const owner = typeof args.owner === 'string' ? args.owner : '';
  const name = typeof args.repo === 'string' ? args.repo : '';
  const repo = forge.getRepo(owner, name);
  if (repo === undefined) throw new ToolError(`repository ${owner}/${name} not found`);
  return repo;
}

function toolResult(view: unknown): Record<string, unknown> {
  const text = JSON.stringify(view);
  return { content: [{ type: 'text', text }], structuredContent: view, isError: false };
}

const TOOLS: readonly ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository for the authenticated user.',
    properties: {
      name: { type: 'string', description: 'The repository name.' },
      description: { type: 'string', description: 'A short description of the repository.' },
      private: { type: 'boolean', description: 'Whether the repository is private.' },
    },
    required: ['name'],
    run: (forge, args) => {
      const name = typeof args.name === 'string' ? args.name : '';
      if (name === '') throw new ToolError('name is required');
      const created = forge.createRepo(name, typeof args.description === 'string' ? args.description : '', args.private !== false);
      if (created === 'exists') throw new ToolError(`repository ${name} already exists`);
      return toolResult(repoJson(created));
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
    },
    required: ['owner', 'repo'],
    run: (forge, args) => toolResult(repoJson(needRepo(forge, args))),
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The issue title.' },
      body: { type: 'string', description: 'The issue body.' },
    },
    required: ['owner', 'repo', 'title'],
    run: (forge, args) => {
      const repo = needRepo(forge, args);
      const issue: IssueRec = {
        number: repo.nextNumber,
        id: forge.allocIssueId(),
        title: typeof args.title === 'string' ? args.title : '',
        body: typeof args.body === 'string' ? args.body : '',
        state: 'open',
        labels: [],
        comments: [],
        seq: repo.nextSeq(),
      };
      repo.nextNumber += 1;
      repo.issues.set(issue.number, issue);
      return toolResult(issueJson(repo, issue));
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      state: { type: 'string', description: 'Filter by state: open, closed or all.' },
    },
    required: ['owner', 'repo'],
    run: (forge, args) => {
      const repo = needRepo(forge, args);
      const state = typeof args.state === 'string' ? args.state : 'open';
      let list = [...repo.issues.values()];
      if (state === 'open' || state === 'closed') list = list.filter((issue) => issue.state === state);
      list.sort((a, b) => a.seq - b.seq);
      return toolResult(list.map((issue) => issueJson(repo, issue)));
    },
  },
  {
    name: 'get_issue',
    description: 'Get one issue by its number.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      issue_number: { type: 'number', description: 'The issue number.' },
    },
    required: ['owner', 'repo', 'issue_number'],
    run: (forge, args) => {
      const repo = needRepo(forge, args);
      const issue = typeof args.issue_number === 'number' ? repo.issues.get(args.issue_number) : undefined;
      if (issue === undefined) throw new ToolError('issue not found');
      return toolResult(issueJson(repo, issue));
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The pull request title.' },
      body: { type: 'string', description: 'The pull request body.' },
      head: { type: 'string', description: 'The head branch name.' },
      base: { type: 'string', description: 'The base branch name.' },
    },
    required: ['owner', 'repo', 'title', 'head', 'base'],
    run: (forge, args) => {
      const repo = needRepo(forge, args);
      const head = typeof args.head === 'string' ? args.head : '';
      const base = typeof args.base === 'string' ? args.base : '';
      if (repo.tipOf(head) === undefined) throw new ToolError(`head branch ${head} does not exist`);
      if (repo.tipOf(base) === undefined) throw new ToolError(`base branch ${base} does not exist`);
      const pull: PullRec = {
        number: repo.nextNumber,
        id: forge.allocIssueId(),
        title: typeof args.title === 'string' ? args.title : '',
        body: typeof args.body === 'string' ? args.body : '',
        state: 'open',
        merged: false,
        mergedAt: null,
        mergeCommitSha: null,
        headRef: head,
        baseRef: base,
        seq: repo.nextSeq(),
      };
      repo.nextNumber += 1;
      repo.pulls.set(pull.number, pull);
      return toolResult(pullJson(repo, pull));
    },
  },
  {
    name: 'list_pull_requests',
    description: 'List the pull requests of a repository, optionally filtered by state.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      state: { type: 'string', description: 'Filter by state: open, closed or all.' },
    },
    required: ['owner', 'repo'],
    run: (forge, args) => {
      const repo = needRepo(forge, args);
      const state = typeof args.state === 'string' ? args.state : 'open';
      let list = [...repo.pulls.values()];
      if (state === 'open' || state === 'closed') list = list.filter((pull) => pull.state === state);
      list.sort((a, b) => a.seq - b.seq);
      return toolResult(list.map((pull) => pullJson(repo, pull)));
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request into its base branch (squash).',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      pull_number: { type: 'number', description: 'The pull request number.' },
    },
    required: ['owner', 'repo', 'pull_number'],
    run: (forge, args) => {
      const repo = needRepo(forge, args);
      const pull = typeof args.pull_number === 'number' ? repo.pulls.get(args.pull_number) : undefined;
      if (pull === undefined) throw new ToolError('pull request not found');
      if (pull.state !== 'open') throw new ToolError('pull request is not open');
      const sha = squashMerge(repo, pull);
      if (sha === null) throw new ToolError('pull request is not mergeable: merge conflict');
      pull.state = 'closed';
      pull.merged = true;
      pull.mergedAt = new Date().toISOString();
      pull.mergeCommitSha = sha;
      return toolResult(pullJson(repo, pull));
    },
  },
];

/** The tools/list payload, built once so two calls answer byte-for-byte identical bytes. */
const TOOLS_LIST: readonly Record<string, unknown>[] = TOOLS.map((tool) => ({
  name: tool.name,
  description: tool.description,
  inputSchema: { type: 'object', properties: tool.properties, required: tool.required },
}));

interface RpcOutcome {
  status: number;
  payload: Record<string, unknown> | null; // null: no body (a notification's 202)
}

function rpcError(id: unknown, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, error: { code, message } };
}

function rpcResult(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, result };
}

function handleRpc(forge: Forge, message: Record<string, unknown>): RpcOutcome {
  const method = typeof message.method === 'string' ? message.method : '';
  const id = message.id;
  const isNotification = id === undefined;
  const params = (typeof message.params === 'object' && message.params !== null ? message.params : {}) as Record<string, unknown>;

  switch (method) {
    case 'server/discover':
      return {
        status: 200,
        payload: rpcResult(id, {
          protocolVersion: '2026-07-28',
          supportedVersions: SUPPORTED_VERSIONS,
          capabilities: { tools: {} },
          serverInfo: SERVER_INFO,
          _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
        }),
      };
    case 'initialize': {
      const offered = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
      const negotiated = (SUPPORTED_VERSIONS as readonly string[]).includes(offered) ? offered : '2025-11-25';
      return {
        status: 200,
        payload: rpcResult(id, { protocolVersion: negotiated, capabilities: { tools: {} }, serverInfo: SERVER_INFO }),
      };
    }
    case 'ping':
      return { status: 200, payload: rpcResult(id, {}) };
    case 'tools/list':
      return { status: 200, payload: rpcResult(id, { tools: TOOLS_LIST }) };
    case 'tools/call': {
      const name = typeof params.name === 'string' ? params.name : '';
      const tool = TOOLS.find((candidate) => candidate.name === name);
      if (tool === undefined) return { status: 200, payload: rpcError(id, -32602, `Unknown tool: ${name}`) };
      const args = (typeof params.arguments === 'object' && params.arguments !== null ? params.arguments : {}) as Record<string, unknown>;
      const missing = tool.required.filter((key) => args[key] === undefined || args[key] === null || args[key] === '');
      if (missing.length > 0) return { status: 200, payload: rpcError(id, -32602, `Missing required argument(s): ${missing.join(', ')}`) };
      try {
        return { status: 200, payload: rpcResult(id, tool.run(forge, args)) };
      } catch (err) {
        if (err instanceof ToolError) {
          return {
            status: 200,
            payload: rpcResult(id, { content: [{ type: 'text', text: err.message }], isError: true }),
          };
        }
        throw err;
      }
    }
    default:
      if (isNotification || method.startsWith('notifications/')) return { status: 202, payload: null };
      return { status: 200, payload: rpcError(id, -32601, `Method not found: ${method}`) };
  }
}

export function handleMcp(ctx: Ctx): void {
  let parsed: unknown;
  try {
    parsed = JSON.parse(ctx.body.toString('utf8'));
  } catch {
    ctx.res.statusCode = 400;
    ctx.res.setHeader('Content-Type', 'application/json');
    ctx.res.end(JSON.stringify(rpcError(null, -32700, 'Parse error: the body is not JSON')));
    return;
  }
  if (Array.isArray(parsed)) {
    const results = parsed
      .map((entry) => (typeof entry === 'object' && entry !== null ? handleRpc(ctx.forge, entry as Record<string, unknown>) : null))
      .map((outcome) => outcome?.payload)
      .filter((payload) => payload !== null && payload !== undefined);
    return ctx.send(200, results);
  }
  if (typeof parsed !== 'object' || parsed === null) {
    ctx.res.statusCode = 400;
    ctx.res.setHeader('Content-Type', 'application/json');
    ctx.res.end(JSON.stringify(rpcError(null, -32700, 'Parse error: the body is not a JSON-RPC message')));
    return;
  }
  const outcome = handleRpc(ctx.forge, parsed as Record<string, unknown>);
  if (outcome.payload === null) {
    ctx.res.statusCode = outcome.status;
    ctx.res.end();
    return;
  }
  return ctx.send(outcome.status, outcome.payload);
}

export { VIEWER };
