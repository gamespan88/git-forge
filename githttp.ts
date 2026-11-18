// Git smart HTTP: the ref advertisement, protocol-v2 ls-refs and fetch, protocol-v0 fetch, and
// receive-pack with report-status. The dialect is the one the suite's own client speaks
// (kit/sdk/conformance/git-client.ts): v2 is honoured when the Git-Protocol header asks for it,
// receive-pack is v0 only, packs are whole objects (no deltas), and ref updates are
// fast-forward-only with a per-ref ok/ng report.

import { ZERO_SHA, isAncestor, parseCommit, parseTag, parseTree } from './git.ts';
import { Forge, Repo } from './store.ts';
import type { Ctx } from './app.ts';

const AGENT = 'forge/1.0';

// ------------------------------------------------------------------ pkt-line codec

export function pkt(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const header = Buffer.alloc(4);
  header.write((data.length + 4).toString(16).padStart(4, '0'), 0, 'ascii');
  return Buffer.concat([header, data]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

export interface Packet {
  kind: 'data' | 'flush' | 'delim' | 'response-end';
  payload: Buffer;
}

/** Parse a whole request body into packets; throws on malformed framing. */
export function parseAllPackets(body: Buffer): Packet[] {
  const packets: Packet[] = [];
  let pos = 0;
  while (pos < body.length) {
    if (pos + 4 > body.length) throw new Error('truncated pkt-line header');
    const hex = body.subarray(pos, pos + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error(`invalid pkt-line length ${JSON.stringify(hex)}`);
    const length = Number.parseInt(hex, 16);
    if (length === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      pos += 4;
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      pos += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'response-end', payload: Buffer.alloc(0) });
      pos += 4;
      continue;
    }
    if (length < 4 || pos + length > body.length) throw new Error('truncated pkt-line');
    packets.push({ kind: 'data', payload: body.subarray(pos + 4, pos + length) });
    pos += length;
  }
  return packets;
}

/** Parse the command section of a receive-pack body; everything after the first flush is the pack. */
export function parsePackets(body: Buffer): { packets: Packet[]; rest: Buffer } {
  const packets: Packet[] = [];
  let pos = 0;
  while (pos < body.length) {
    if (pos + 4 > body.length) throw new Error('truncated pkt-line header');
    const hex = body.subarray(pos, pos + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error(`invalid pkt-line length ${JSON.stringify(hex)}`);
    const length = Number.parseInt(hex, 16);
    if (length === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      pos += 4;
      // Anything after a flush in a receive-pack body is the packfile itself.
      return { packets, rest: body.subarray(pos) };
    }
    if (length === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      pos += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'response-end', payload: Buffer.alloc(0) });
      pos += 4;
      continue;
    }
    if (length < 4 || pos + length > body.length) throw new Error('truncated pkt-line');
    packets.push({ kind: 'data', payload: body.subarray(pos + 4, pos + length) });
    pos += length;
  }
  return { packets, rest: Buffer.alloc(0) };
}

function lines(packets: readonly Packet[]): string[] {
  return packets.filter((packet) => packet.kind === 'data').map((packet) => packet.payload.toString('utf8').replace(/\n$/, ''));
}

// ------------------------------------------------------------------ advertisements

function peelToCommit(repo: Repo, sha: string): string {
  let current = sha;
  for (let guard = 0; guard < 16; guard += 1) {
    const obj = repo.objects.get(current);
    if (obj === undefined || obj.type !== 'tag') return current;
    current = parseTag(obj.data).object;
  }
  return current;
}

interface RefLine {
  oid: string;
  name: string;
  peeled?: string;
}

function sortedRefLines(repo: Repo): RefLine[] {
  const out: RefLine[] = [];
  for (const name of [...repo.refs.keys()].sort()) {
    const oid = repo.refs.get(name) as string;
    const entry: RefLine = { oid, name };
    const target = repo.objects.get(oid);
    if (target !== undefined && target.type === 'tag') entry.peeled = peelToCommit(repo, oid);
    out.push(entry);
  }
  return out;
}

export function handleInfoRefs(ctx: Ctx, owner: string, repoName: string): void {
  const service = ctx.url.searchParams.get('service') ?? '';
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') return ctx.fail(400, 'Unsupported service');
  const repo = ctx.forge.getRepo(owner, repoName);
  if (repo === undefined) return ctx.fail(404, 'Not Found');
  const wantsV2 = (ctx.req.headers['git-protocol'] ?? '').includes('version=2');
  ctx.res.setHeader('Content-Type', `application/x-${service}-advertisement`);
  ctx.res.setHeader('Cache-Control', 'no-cache');

  if (service === 'git-upload-pack' && wantsV2) {
    const body = Buffer.concat([
      pkt('version 2\n'),
      pkt(`agent=${AGENT}\n`),
      pkt('ls-refs=unborn\n'),
      pkt('fetch=shallow\n'),
      pkt('object-format=sha1\n'),
      PKT_FLUSH,
    ]);
    return ctx.sendRaw(200, body);
  }

  const headRef = repo.headRef();
  const headOid = repo.refs.get(headRef);
  const caps =
    service === 'git-upload-pack'
      ? `side-band-64k ofs-delta shallow agent=${AGENT} symref=HEAD:${headRef}`
      : `report-status side-band-64k delete-refs ofs-delta agent=${AGENT} symref=HEAD:${headRef}`;
  const parts: Buffer[] = [pkt(`# service=${service}\n`), PKT_FLUSH];
  const refs = sortedRefLines(repo);
  if (headOid === undefined && refs.length === 0) {
    parts.push(pkt(`${ZERO_SHA} capabilities^{}\0${caps}\n`));
  } else {
    let first = true;
    if (headOid !== undefined) {
      parts.push(pkt(`${headOid} HEAD\0${caps}\n`));
      first = false;
    }
    for (const ref of refs) {
      parts.push(pkt(first ? `${ref.oid} ${ref.name}\0${caps}\n` : `${ref.oid} ${ref.name}\n`));
      first = false;
      if (ref.peeled !== undefined) parts.push(pkt(`${ref.peeled} ${ref.name}^{}\n`));
    }
  }
  parts.push(PKT_FLUSH);
  return ctx.sendRaw(200, Buffer.concat(parts));
}

// ------------------------------------------------------------------ upload-pack

export function handleUploadPack(ctx: Ctx, owner: string, repoName: string): void {
  const repo = ctx.forge.getRepo(owner, repoName);
  if (repo === undefined) return ctx.fail(404, 'Not Found');
  ctx.res.setHeader('Content-Type', 'application/x-git-upload-pack-result');
  ctx.res.setHeader('Cache-Control', 'no-cache');
  const packets = parseAllPackets(ctx.body);
  const firstData = packets.find((packet) => packet.kind === 'data');
  const firstLine = firstData?.payload.toString('utf8') ?? '';
  if (firstLine.startsWith('command=ls-refs')) return lsRefs(ctx, repo, packets);
  if (firstLine.startsWith('command=fetch')) return fetchV2(ctx, repo, packets);
  if (firstLine.startsWith('want ')) return fetchV0(ctx, repo, packets);
  throw new Error('unrecognized upload-pack request');
}

function lsRefs(ctx: Ctx, repo: Repo, packets: readonly Packet[]): void {
  const args = lines(packets.slice(1));
  const peel = args.includes('peel');
  const symrefs = args.includes('symrefs');
  const prefixes = args.filter((arg) => arg.startsWith('ref-prefix ')).map((arg) => arg.slice('ref-prefix '.length));
  const matches = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const parts: Buffer[] = [];
  const headRef = repo.headRef();
  const headOid = repo.refs.get(headRef);
  if (matches('HEAD')) {
    if (headOid === undefined) parts.push(pkt(`unborn HEAD${symrefs ? ` symref-target:${headRef}` : ''}\n`));
    else parts.push(pkt(`${headOid} HEAD${symrefs ? ` symref-target:${headRef}` : ''}\n`));
  }
  for (const ref of sortedRefLines(repo)) {
    if (!matches(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (peel && ref.peeled !== undefined) line += ` peeled:${ref.peeled}`;
    parts.push(pkt(`${line}\n`));
  }
  parts.push(PKT_FLUSH);
  return ctx.sendRaw(200, Buffer.concat(parts));
}

// ---------------------------------------------------------- fetch object selection

interface FetchSelection {
  send: string[];
  boundary: string[];
  unshallow: string[];
}

/** Add a commit's whole tree (subtrees and blobs) to `into`. */
function addTreeClosure(repo: Repo, treeSha: string, into: Set<string>): void {
  if (into.has(treeSha)) return;
  const obj = repo.objects.get(treeSha);
  if (obj === undefined || obj.type !== 'tree') return;
  into.add(treeSha);
  for (const entry of parseTree(obj.data)) {
    if (entry.mode === '40000') addTreeClosure(repo, entry.sha, into);
    else into.add(entry.sha);
  }
}

function selectFetchObjects(
  repo: Repo,
  wants: readonly string[],
  haves: readonly string[],
  clientShallow: ReadonlySet<string>,
  depth: number | null,
): FetchSelection {
  // What the client already holds: its haves and their history, stopping at its shallow
  // boundary (a shallow commit's ancestors are NOT held), plus every tree of a held commit.
  const held = new Set<string>();
  {
    const seenCommits = new Set<string>();
    const queue = [...haves, ...clientShallow];
    while (queue.length > 0) {
      const sha = queue.pop() as string;
      if (seenCommits.has(sha)) continue;
      const obj = repo.objects.get(sha);
      if (obj === undefined || obj.type !== 'commit') continue;
      seenCommits.add(sha);
      held.add(sha);
      addTreeClosure(repo, parseCommit(obj.data).tree, held);
      if (clientShallow.has(sha)) continue;
      for (const parent of parseCommit(obj.data).parents) queue.push(parent);
    }
  }

  // What the client wants: the wants and (depth-limited) their history, with trees. A want may
  // be an annotated tag: the tag object itself is part of the answer, and its target commit is
  // where the history walk starts.
  const wanted = new Set<string>();
  const includedCommits = new Set<string>();
  const boundary: string[] = [];
  {
    const generation = new Map<string, number>();
    const queue: Array<{ sha: string; gen: number }> = [];
    for (const want of wants) {
      let target = want;
      for (let guard = 0; guard < 16; guard += 1) {
        const obj = repo.objects.get(target);
        if (obj === undefined) break;
        wanted.add(target);
        if (obj.type !== 'tag') break;
        target = parseTag(obj.data).object;
      }
      queue.push({ sha: target, gen: 0 });
    }
    while (queue.length > 0) {
      const { sha, gen } = queue.shift() as { sha: string; gen: number };
      if (generation.has(sha) && (generation.get(sha) as number) <= gen) continue;
      generation.set(sha, gen);
      const obj = repo.objects.get(sha);
      if (obj === undefined || obj.type !== 'commit') continue;
      includedCommits.add(sha);
      wanted.add(sha);
      addTreeClosure(repo, parseCommit(obj.data).tree, wanted);
      const parents = parseCommit(obj.data).parents;
      if (depth !== null && gen + 1 >= depth) {
        if (parents.length > 0) boundary.push(sha);
        continue;
      }
      for (const parent of parents) queue.push({ sha: parent, gen: gen + 1 });
    }
  }

  const send = [...wanted].filter((sha) => !held.has(sha));
  const unshallow: string[] = [];
  for (const sha of clientShallow) {
    if (includedCommits.has(sha) && !boundary.includes(sha)) unshallow.push(sha);
  }
  const newBoundary = boundary.filter((sha) => !clientShallow.has(sha));
  return { send, boundary: newBoundary, unshallow };
}

function sidebandPackets(pack: Buffer): Buffer[] {
  const parts: Buffer[] = [];
  // A pkt-line is at most 65520 bytes including its 4-byte header, and a sideband packet spends
  // one payload byte on the band, leaving 65515 for pack data.
  const CHUNK = 65515;
  for (let offset = 0; offset < pack.length; offset += CHUNK) {
    parts.push(pkt(Buffer.concat([Buffer.from([1]), pack.subarray(offset, offset + CHUNK)])));
  }
  return parts;
}

function fetchV2(ctx: Ctx, repo: Repo, packets: readonly Packet[]): void {
  const args = lines(packets.slice(1));
  const wants: string[] = [];
  const haves: string[] = [];
  const clientShallow = new Set<string>();
  let depth: number | null = null;
  for (const arg of args) {
    if (arg.startsWith('want ')) wants.push(arg.slice(5).trim());
    else if (arg.startsWith('have ')) haves.push(arg.slice(5).trim());
    else if (arg.startsWith('shallow ')) clientShallow.add(arg.slice(8).trim());
    else if (arg.startsWith('deepen ')) depth = Number.parseInt(arg.slice(7), 10) || null;
  }
  const selection = selectFetchObjects(repo, wants, haves, clientShallow, depth);
  const parts: Buffer[] = [];
  if (haves.length > 0) {
    parts.push(pkt('acknowledgments\n'));
    const common = haves.filter((have) => repo.objects.has(have) && wants.some((want) => have === want || isAncestor(repo.objects, have, want)));
    if (common.length === 0) parts.push(pkt('NAK\n'));
    else {
      for (const have of common) parts.push(pkt(`ACK ${have} common\n`));
      parts.push(pkt('ready\n'));
    }
    parts.push(PKT_FLUSH);
  }
  if (selection.boundary.length > 0 || selection.unshallow.length > 0) {
    parts.push(pkt('shallow-info\n'));
    for (const sha of selection.boundary) parts.push(pkt(`shallow ${sha}\n`));
    for (const sha of selection.unshallow) parts.push(pkt(`unshallow ${sha}\n`));
    parts.push(PKT_FLUSH);
  }
  if (selection.send.length > 0) {
    parts.push(pkt('packfile\n'));
    const pack = repo.objects.buildPack(selection.send);
    parts.push(...sidebandPackets(pack));
    parts.push(PKT_FLUSH);
  }
  return ctx.sendRaw(200, Buffer.concat(parts));
}

function fetchV0(ctx: Ctx, repo: Repo, packets: readonly Packet[]): void {
  const all = lines(packets);
  const wants: string[] = [];
  const haves: string[] = [];
  const clientShallow = new Set<string>();
  let depth: number | null = null;
  for (const arg of all) {
    if (arg.startsWith('want ')) wants.push(arg.slice(5).trim().split(' ')[0] as string);
    else if (arg.startsWith('have ')) haves.push(arg.slice(5).trim());
    else if (arg.startsWith('shallow ')) clientShallow.add(arg.slice(8).trim());
    else if (arg.startsWith('deepen ')) depth = Number.parseInt(arg.slice(7), 10) || null;
  }
  const selection = selectFetchObjects(repo, wants, haves, clientShallow, depth);
  const parts: Buffer[] = [];
  const common = haves.find((have) => repo.objects.has(have));
  parts.push(pkt(common === undefined ? 'NAK\n' : `ACK ${common}\n`));
  for (const sha of selection.boundary) parts.push(pkt(`shallow ${sha}\n`));
  for (const sha of selection.unshallow) parts.push(pkt(`unshallow ${sha}\n`));
  if (selection.send.length > 0) {
    const pack = repo.objects.buildPack(selection.send);
    parts.push(...sidebandPackets(pack));
  }
  parts.push(PKT_FLUSH);
  return ctx.sendRaw(200, Buffer.concat(parts));
}

// ------------------------------------------------------------------ receive-pack

export function handleReceivePack(ctx: Ctx, owner: string, repoName: string): void {
  const repo = ctx.forge.getRepo(owner, repoName);
  if (repo === undefined) return ctx.fail(404, 'Not Found');
  ctx.res.setHeader('Content-Type', 'application/x-git-receive-pack-result');
  ctx.res.setHeader('Cache-Control', 'no-cache');
  const { packets, rest } = parsePackets(ctx.body);
  const commandLines = lines(packets);
  if (commandLines.length === 0) throw new Error('empty receive-pack command list');

  let capabilities = '';
  const commands = commandLines.map((line, index) => {
    let text = line;
    if (index === 0) {
      const nul = text.indexOf('\0');
      if (nul >= 0) {
        capabilities = text.slice(nul + 1);
        text = text.slice(0, nul);
      }
    }
    const [oldOid, newOid, ref] = text.split(' ') as [string, string, string];
    return { oldOid, newOid, ref };
  });
  const useSideband = capabilities.split(' ').includes('side-band-64k');
  const wantReport = capabilities.split(' ').includes('report-status') || capabilities.split(' ').includes('report-status-v2');

  let unpackOk = true;
  let unpackDetail = 'ok';
  if (rest.length > 0) {
    try {
      repo.objects.ingestPack(rest);
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'error';
    }
  }

  const reportLines: string[] = [`unpack ${unpackOk ? 'ok' : unpackDetail}\n`];
  for (const command of commands) {
    const { oldOid, newOid, ref } = command;
    if (!unpackOk) {
      reportLines.push(`ng ${ref} unpacker error\n`);
      continue;
    }
    const current = repo.refs.get(ref) ?? ZERO_SHA;
    if (oldOid !== current) {
      reportLines.push(`ng ${ref} stale info\n`);
      continue;
    }
    if (newOid === ZERO_SHA) {
      if (current === ZERO_SHA) {
        reportLines.push(`ng ${ref} ref not found\n`);
        continue;
      }
      repo.refs.delete(ref);
      reportLines.push(`ok ${ref}\n`);
      continue;
    }
    const target = repo.objects.get(newOid);
    if (target === undefined) {
      reportLines.push(`ng ${ref} missing object\n`);
      continue;
    }
    if (current !== ZERO_SHA && target.type === 'commit' && !isAncestor(repo.objects, current, newOid)) {
      reportLines.push(`ng ${ref} non-fast-forward\n`);
      continue;
    }
    repo.refs.set(ref, newOid);
    reportLines.push(`ok ${ref}\n`);
  }

  if (!wantReport) return ctx.sendRaw(200, Buffer.alloc(0));
  const report = Buffer.concat([...reportLines.map((line) => pkt(line)), PKT_FLUSH]);
  if (useSideband) {
    const wrapped: Buffer[] = [];
    const CHUNK = 65515;
    for (let offset = 0; offset < report.length; offset += CHUNK) {
      wrapped.push(pkt(Buffer.concat([Buffer.from([1]), report.subarray(offset, offset + CHUNK)])));
    }
    wrapped.push(PKT_FLUSH);
    return ctx.sendRaw(200, Buffer.concat(wrapped));
  }
  return ctx.sendRaw(200, report);
}
