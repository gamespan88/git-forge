// The git object core of the forge: an in-memory object store with SHA-1 ids, tree and commit
// codecs, packfile reading (including ofs/ref deltas) and packfile writing. Pure Node standard
// library (node:crypto for SHA-1, node:zlib for the deflate streams inside packs).
//
// Objects are stored by their raw content: a commit is its header block plus message, a tree is
// its sequence of "<mode> <name>\0<20-byte id>" entries, a blob is the file bytes. The id of an
// object is sha1("<type> <length>\0" + content), exactly as git computes it, so ids the suite
// computes on its side match the ids this store computes here.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  readonly type: ObjType;
  readonly data: Buffer;
}

export const ZERO_SHA = '0000000000000000000000000000000000000000';

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

export function objectId(type: ObjType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return sha1Hex(Buffer.concat([header, data]));
}

/** Inflate one zlib stream out of `buf` starting at `offset`, knowing its inflated size. */
export function inflateAt(buf: Buffer, offset: number, expectedSize: number): { data: Buffer; next: number } {
  // zlib's inflateSync ignores trailing bytes, so "inflates to the expected size" is a monotone
  // predicate of the slice length: the shortest slice that satisfies it ends exactly at the end
  // of this stream. Probe exponentially from a guess, then binary-search the boundary.
  const remaining = buf.length - offset;
  const tryLen = (len: number): Buffer | null => {
    try {
      const out = inflateSync(buf.subarray(offset, offset + len));
      return out.length === expectedSize ? out : null;
    } catch {
      return null;
    }
  };
  let lo = 0; // known too short (or unprobed)
  let hi = Math.min(remaining, Math.max(16, Math.ceil(expectedSize * 0.6)));
  let out = tryLen(hi);
  while (out === null) {
    lo = hi;
    if (hi >= remaining) throw new Error('pack object does not inflate to its stated size');
    hi = Math.min(remaining, hi * 2);
    out = tryLen(hi);
  }
  while (lo + 1 < hi) {
    const mid = (lo + hi) >> 1;
    const midOut = tryLen(mid);
    if (midOut === null) {
      lo = mid;
    } else {
      hi = mid;
      out = midOut;
    }
  }
  return { data: out as Buffer, next: offset + hi };
}

/** Apply a git delta (the format used by pack ofs-delta and ref-delta objects) to a base object. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[pos] as number;
      pos += 1;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) break;
    }
    return value;
  };
  const sourceSize = readVarint();
  if (sourceSize !== base.length) throw new Error(`delta base size mismatch: ${sourceSize} != ${base.length}`);
  const targetSize = readVarint();
  const parts: Buffer[] = [];
  let produced = 0;
  while (pos < delta.length) {
    const op = delta[pos] as number;
    pos += 1;
    if (op & 0x80) {
      let offset = 0;
      let size = 0;
      if (op & 0x01) { offset = delta[pos] as number; pos += 1; }
      if (op & 0x02) { offset |= (delta[pos] as number) << 8; pos += 1; }
      if (op & 0x04) { offset |= (delta[pos] as number) << 16; pos += 1; }
      if (op & 0x08) { offset |= (delta[pos] as number) << 24; pos += 1; }
      if (op & 0x10) { size = delta[pos] as number; pos += 1; }
      if (op & 0x20) { size |= (delta[pos] as number) << 8; pos += 1; }
      if (op & 0x40) { size |= (delta[pos] as number) << 16; pos += 1; }
      if (size === 0) size = 0x10000;
      parts.push(base.subarray(offset, offset + size));
      produced += size;
    } else if (op !== 0) {
      parts.push(delta.subarray(pos, pos + op));
      pos += op;
      produced += op;
    } else {
      throw new Error('delta opcode 0 is reserved');
    }
  }
  const out = Buffer.concat(parts);
  if (out.length !== targetSize || produced !== targetSize) throw new Error('delta target size mismatch');
  return out;
}

const PACK_TYPES: Readonly<Record<number, ObjType>> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export class ObjectStore {
  readonly objects = new Map<string, GitObject>();

  has(sha: string): boolean {
    return this.objects.has(sha);
  }

  get(sha: string): GitObject | undefined {
    return this.objects.get(sha);
  }

  write(type: ObjType, data: Buffer): string {
    const sha = objectId(type, data);
    if (!this.objects.has(sha)) this.objects.set(sha, { type, data });
    return sha;
  }

  /**
   * Read every object out of a packfile into this store. Deltas are resolved against objects
   * already in the store and objects read earlier from the same pack, in whatever order they
   * settle. Returns the ids written, in pack order where that is knowable.
   */
  ingestPack(pack: Buffer): string[] {
    if (pack.length < 12 + 20 || pack.subarray(0, 4).toString('latin1') !== 'PACK') {
      throw new Error('not a packfile');
    }
    const version = pack.readUInt32BE(4);
    if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
    const count = pack.readUInt32BE(8);
    let pos = 12;
    const pending: Array<{ kind: 'ofs'; baseOffset: number; type: ObjType; delta: Buffer } | { kind: 'ref'; baseSha: string; type: ObjType; delta: Buffer }> = [];
    const offsetToSha = new Map<number, string>();
    const written: string[] = [];
    const place = (type: ObjType, data: Buffer, atOffset: number): string => {
      const sha = this.write(type, data);
      offsetToSha.set(atOffset, sha);
      written.push(sha);
      return sha;
    };
    for (let i = 0; i < count; i += 1) {
      const start = pos;
      let byte = pack[pos] as number;
      pos += 1;
      const typeNum = (byte >> 4) & 0x7;
      let size = byte & 0x0f;
      let shift = 4;
      while (byte & 0x80) {
        byte = pack[pos] as number;
        pos += 1;
        size |= (byte & 0x7f) << shift;
        shift += 7;
      }
      if (typeNum === 6) {
        // ofs-delta: a negative offset back to the base object's start in this pack.
        let b = pack[pos] as number;
        pos += 1;
        let baseOffset = b & 0x7f;
        while (b & 0x80) {
          b = pack[pos] as number;
          pos += 1;
          baseOffset = ((baseOffset + 1) << 7) | (b & 0x7f);
        }
        const { data, next } = inflateAt(pack, pos, size);
        pos = next;
        pending.push({ kind: 'ofs', baseOffset: start - baseOffset, type: 'blob', delta: data });
        // The type is the base's type; remember the slot by pack offset for resolution below.
        (pending[pending.length - 1] as { atOffset?: number }).atOffset = start;
      } else if (typeNum === 7) {
        const baseSha = pack.subarray(pos, pos + 20).toString('hex');
        pos += 20;
        const { data, next } = inflateAt(pack, pos, size);
        pos = next;
        pending.push({ kind: 'ref', baseSha, type: 'blob', delta: data });
        (pending[pending.length - 1] as { atOffset?: number }).atOffset = start;
      } else {
        const type = PACK_TYPES[typeNum];
        if (type === undefined) throw new Error(`unknown pack object type ${typeNum}`);
        const { data, next } = inflateAt(pack, pos, size);
        pos = next;
        place(type, data, start);
      }
    }
    // Resolve deltas until none can make progress; each resolution may unlock later ones.
    let progressed = true;
    while (pending.length > 0 && progressed) {
      progressed = false;
      for (let i = pending.length - 1; i >= 0; i -= 1) {
        const item = pending[i] as (typeof pending)[number] & { atOffset?: number };
        const baseSha = item.kind === 'ofs' ? offsetToSha.get(item.baseOffset) : item.baseSha;
        if (baseSha === undefined) continue;
        const base = this.objects.get(baseSha);
        if (base === undefined) continue;
        const data = applyDelta(base.data, item.delta);
        place(base.type, data, item.atOffset ?? -1);
        pending.splice(i, 1);
        progressed = true;
      }
    }
    if (pending.length > 0) throw new Error(`${pending.length} pack delta(s) have no base in the store`);
    return written;
  }

  /** Build a packfile holding the given objects whole (no deltas), in the order given. */
  buildPack(shas: readonly string[]): Buffer {
    const parts: Buffer[] = [];
    const header = Buffer.alloc(12);
    header.write('PACK', 0, 'latin1');
    header.writeUInt32BE(2, 4);
    header.writeUInt32BE(shas.length, 8);
    parts.push(header);
    const typeNums: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
    for (const sha of shas) {
      const obj = this.objects.get(sha);
      if (obj === undefined) throw new Error(`cannot pack missing object ${sha}`);
      let size = obj.data.length;
      const first = (typeNums[obj.type] << 4) | (size & 0x0f);
      size >>= 4;
      const head: number[] = [];
      let byte = first;
      while (size > 0) {
        head.push(byte | 0x80);
        byte = size & 0x7f;
        size >>= 7;
      }
      head.push(byte);
      parts.push(Buffer.from(head));
      parts.push(deflateSync(obj.data));
    }
    const body = Buffer.concat(parts);
    const trailer = createHash('sha1').update(body).digest();
    return Buffer.concat([body, trailer]);
  }
}

// ------------------------------------------------------------- trees and commits

export interface TreeEntry {
  mode: string; // e.g. "100644", "100755", "40000" (git writes tree modes without the leading 0)
  name: string;
  sha: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let pos = 0;
  while (pos < data.length) {
    const space = data.indexOf(0x20, pos);
    const nul = data.indexOf(0x00, space);
    const mode = data.subarray(pos, space).toString('utf8');
    const name = data.subarray(space + 1, nul).toString('utf8');
    const sha = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, sha });
    pos = nul + 21;
  }
  return entries;
}

export function serializeTree(entries: readonly TreeEntry[]): Buffer {
  // Git orders tree entries by name, with directories compared as if they ended in "/".
  const sorted = [...entries].sort((a, b) => {
    const ka = a.mode === '40000' ? `${a.name}/` : a.name;
    const kb = b.mode === '40000' ? `${b.name}/` : b.name;
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.sha, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface CommitInfo {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split === -1 ? text : text.slice(0, split);
  const message = split === -1 ? '' : text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

export interface TagInfo {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  message: string;
}

export function parseTag(data: Buffer): TagInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split === -1 ? text : text.slice(0, split);
  const headers = new Map<string, string>();
  for (const line of head.split('\n')) {
    const space = line.indexOf(' ');
    if (space > 0) headers.set(line.slice(0, space), line.slice(space + 1));
  }
  return {
    object: headers.get('object') ?? '',
    type: headers.get('type') ?? '',
    tag: headers.get('tag') ?? '',
    tagger: headers.get('tagger') ?? '',
    message: split === -1 ? '' : text.slice(split + 2),
  };
}

export function serializeCommit(info: CommitInfo): Buffer {  const lines = [`tree ${info.tree}`];
  for (const parent of info.parents) lines.push(`parent ${parent}`);
  lines.push(`author ${info.author}`);
  lines.push(`committer ${info.committer}`);
  return Buffer.from(`${lines.join('\n')}\n\n${info.message}`, 'utf8');
}

/** Format an ident line the way git does: "Name <email> <unix-seconds> <tz>". */
export function ident(name: string, email: string, whenSec: number, tz = '+0000'): string {
  return `${name} <${email}> ${whenSec} ${tz}`;
}

/** Every ancestor of `tip`, tip first, following all parents (the store's commits only). */
export function ancestors(store: ObjectStore, tip: string): string[] {
  const seen = new Set<string>();
  const order: string[] = [];
  const queue = [tip];
  while (queue.length > 0) {
    const sha = queue.shift() as string;
    if (seen.has(sha) || sha === ZERO_SHA) continue;
    const obj = store.get(sha);
    if (obj === undefined || obj.type !== 'commit') continue;
    seen.add(sha);
    order.push(sha);
    for (const parent of parseCommit(obj.data).parents) queue.push(parent);
  }
  return order;
}

export function isAncestor(store: ObjectStore, maybeAncestor: string, tip: string): boolean {
  // Depth-first from the tip with early exit: the common question ("is the ref's current tip an
  // ancestor of the proposed new tip?") is answered by the first parent step, and only a true
  // non-ancestor pays for the whole walk.
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0) {
    const sha = stack.pop() as string;
    if (seen.has(sha) || sha === ZERO_SHA) continue;
    seen.add(sha);
    const obj = store.get(sha);
    if (obj === undefined || obj.type !== 'commit') continue;
    if (sha === maybeAncestor) return true;
    for (const parent of parseCommit(obj.data).parents) stack.push(parent);
  }
  return false;
}

/** All objects reachable from a commit: the commit, its trees and blobs, and its history. */
export function reachableObjects(store: ObjectStore, tips: readonly string[]): string[] {
  const out = new Set<string>();
  const visitTree = (sha: string): void => {
    if (out.has(sha)) return;
    const obj = store.get(sha);
    if (obj === undefined) return;
    out.add(sha);
    if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) visitTree(entry.sha);
    }
  };
  for (const tip of tips) {
    for (const commitSha of ancestors(store, tip)) {
      out.add(commitSha);
      const commit = store.get(commitSha);
      if (commit !== undefined) visitTree(parseCommit(commit.data).tree);
    }
  }
  return [...out];
}
