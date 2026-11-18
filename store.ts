// The forge's data store: repositories, each with its own git object store and refs, plus the
// issue / pull-request / label records the REST, GraphQL and MCP surfaces share. Everything is
// in memory: a trial starts from an empty forge and the suite builds every fixture itself.
//
// The one identity: the bearer token the suite presents belongs to the trial scenario's viewer
// (wire.ts's buildScenario() runs on one fixed seed in the sealed env, so the viewer is a
// constant of the contest): adaiversen, id 1000. A repository belongs to that login; the same
// name under any other owner does not exist for this token.

import {
  ObjectStore,
  ZERO_SHA,
  ancestors,
  ident,
  isAncestor,
  parseCommit,
  parseTree,
  serializeCommit,
  serializeTree,
  type TreeEntry,
} from './git.ts';

export const VIEWER = Object.freeze({
  id: 1000,
  login: 'adaiversen',
  name: 'ada iversen',
  email: 'adaiversen@example.invalid',
});

export interface LabelRec {
  name: string;
  color: string;
}

export interface CommentRec {
  id: number;
  body: string;
}

export interface IssueRec {
  number: number;
  id: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: CommentRec[];
  seq: number;
}

export interface PullRec {
  number: number;
  id: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  headRef: string;
  baseRef: string;
  seq: number;
}

export class Repo {
  readonly objects = new ObjectStore();
  /** Full ref names ("refs/heads/main") to commit ids. HEAD is always refs/heads/<defaultBranch>. */
  readonly refs = new Map<string, string>();
  readonly labels = new Map<string, LabelRec>();
  readonly issues = new Map<number, IssueRec>();
  readonly pulls = new Map<number, PullRec>();
  readonly createdAt = new Date().toISOString();
  readonly id: number;
  readonly name: string;
  readonly owner: string;
  description: string;
  isPrivate: boolean;
  readonly defaultBranch: string;
  nextNumber = 1;
  private seqCounter = 0;

  constructor(id: number, name: string, owner: string, description: string, isPrivate: boolean, defaultBranch = 'main') {
    this.id = id;
    this.name = name;
    this.owner = owner;
    this.description = description;
    this.isPrivate = isPrivate;
    this.defaultBranch = defaultBranch;
  }

  nextSeq(): number {
    this.seqCounter += 1;
    return this.seqCounter;
  }

  headRef(): string {
    return `refs/heads/${this.defaultBranch}`;
  }

  tipOf(branch: string): string | undefined {
    return this.refs.get(`refs/heads/${branch}`);
  }

  treeOfCommit(commitSha: string): string | null {
    const obj = this.objects.get(commitSha);
    if (obj === undefined || obj.type !== 'commit') return null;
    return parseCommit(obj.data).tree;
  }

  tipTree(branch: string): string | null {
    const tip = this.tipOf(branch);
    return tip === undefined ? null : this.treeOfCommit(tip);
  }
}

// ------------------------------------------------------------------ tree helpers

/** Walk a tree by path segments; returns the entry's sha and mode, or null. */
export function treeLookup(store: ObjectStore, treeSha: string | null, segments: readonly string[]): { sha: string; mode: string } | null {
  if (treeSha === null) return null;
  let current = treeSha;
  for (let i = 0; i < segments.length; i += 1) {
    const obj = store.get(current);
    if (obj === undefined || obj.type !== 'tree') return null;
    const entry = parseTree(obj.data).find((candidate) => candidate.name === segments[i]);
    if (entry === undefined) return null;
    if (i === segments.length - 1) return { sha: entry.sha, mode: entry.mode };
    if (entry.mode !== '40000') return null;
    current = entry.sha;
  }
  return null;
}

/** Flatten a tree into a map of full path -> blob sha (files only). */
export function flattenTree(store: ObjectStore, treeSha: string | null, prefix = '', out = new Map<string, string>()): Map<string, string> {
  if (treeSha === null) return out;
  const obj = store.get(treeSha);
  if (obj === undefined || obj.type !== 'tree') return out;
  for (const entry of parseTree(obj.data)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') flattenTree(store, entry.sha, path, out);
    else out.set(path, entry.sha);
  }
  return out;
}

/** Build (and store) nested trees from a flat path -> blob sha map; returns the root tree sha. */
export function buildTreeFromFlat(store: ObjectStore, flat: ReadonlyMap<string, string>): string {
  interface Node {
    files: Map<string, string>;
    dirs: Map<string, Node>;
  }
  const root: Node = { files: new Map(), dirs: new Map() };
  for (const [path, sha] of flat) {
    const segments = path.split('/');
    let node = root;
    for (let i = 0; i < segments.length - 1; i += 1) {
      if (!node.dirs.has(segments[i] as string)) node.dirs.set(segments[i] as string, { files: new Map(), dirs: new Map() });
      node = node.dirs.get(segments[i] as string) as Node;
    }
    node.files.set(segments[segments.length - 1] as string, sha);
  }
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, sha] of node.files) entries.push({ mode: '100644', name, sha });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, sha: build(child) });
    return store.write('tree', serializeTree(entries));
  };
  return build(root);
}

/** Return a new tree sha with `segments` set to blobSha (or removed when blobSha is null). */
export function updateTree(store: ObjectStore, treeSha: string | null, segments: readonly string[], blobSha: string | null): string {
  const entries: TreeEntry[] = treeSha === null ? [] : [...parseTree((store.get(treeSha) as { data: Buffer }).data)];
  const [head, ...rest] = segments as [string, ...(readonly string[])];
  const index = entries.findIndex((entry) => entry.name === head);
  if (rest.length === 0) {
    if (blobSha === null) {
      if (index >= 0) entries.splice(index, 1);
    } else if (index >= 0) {
      entries[index] = { mode: '100644', name: head, sha: blobSha };
    } else {
      entries.push({ mode: '100644', name: head, sha: blobSha });
    }
  } else {
    const childTree = index >= 0 && entries[index]?.mode === '40000' ? (entries[index] as TreeEntry).sha : null;
    const newChild = updateTree(store, childTree, rest, blobSha);
    const childEmpty = (store.get(newChild)?.data.length ?? 0) === 0;
    if (childEmpty) {
      if (index >= 0) entries.splice(index, 1);
    } else if (index >= 0) {
      entries[index] = { mode: '40000', name: head, sha: newChild };
    } else {
      entries.push({ mode: '40000', name: head, sha: newChild });
    }
  }
  return store.write('tree', serializeTree(entries));
}

// ------------------------------------------------------------------ the forge

export class StaleShaError extends Error {}
export class NotFoundError extends Error {}

export class Forge {
  readonly repos = new Map<string, Repo>();
  private nextRepoId = 1;
  private nextIssueId = 1;
  private nextCommentId = 1;

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  /** The repository this token may see: only the viewer's own; anything else does not exist. */
  getRepo(owner: string, name: string): Repo | undefined {
    if (owner !== VIEWER.login) return undefined;
    return this.repos.get(this.repoKey(owner, name));
  }

  createRepo(name: string, description: string, isPrivate: boolean): Repo | 'exists' {
    const key = this.repoKey(VIEWER.login, name);
    if (this.repos.has(key)) return 'exists';
    const repo = new Repo(this.nextRepoId, name, VIEWER.login, description, isPrivate);
    this.nextRepoId += 1;
    this.repos.set(key, repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    if (owner !== VIEWER.login) return false;
    return this.repos.delete(this.repoKey(owner, name));
  }

  listRepos(): Repo[] {
    return [...this.repos.values()].sort((a, b) => a.id - b.id);
  }

  allocIssueId(): number {
    const id = this.nextIssueId;
    this.nextIssueId += 1;
    return id;
  }

  allocCommentId(): number {
    const id = this.nextCommentId;
    this.nextCommentId += 1;
    return id;
  }

  /**
   * Create or delete the file at `path` on `branch` as a new commit. When the branch does not
   * exist yet it is started from `createFrom`'s tip (or as a root commit when the repository is
   * empty). `expectSha`, when given, is the blob id the caller believes the file has now: a
   * mismatch is a StaleShaError, and a missing file with an expectation is stale too.
   */
  writeFile(
    repo: Repo,
    branch: string,
    path: string,
    content: Buffer | null,
    message: string,
    options: { expectSha?: string; createFrom?: string } = {},
  ): { commitSha: string; blobSha: string | null; created: boolean } {
    const ref = `refs/heads/${branch}`;
    let tip = repo.refs.get(ref);
    if (tip === undefined && options.createFrom !== undefined) tip = repo.refs.get(`refs/heads/${options.createFrom}`);
    const tipTree = tip === undefined ? null : repo.treeOfCommit(tip);
    const segments = path.split('/');
    const existing = treeLookup(repo.objects, tipTree, segments);
    if (options.expectSha !== undefined && (existing?.sha ?? null) !== options.expectSha) {
      throw new StaleShaError(`the file's sha is ${existing?.sha ?? 'absent'}, not ${options.expectSha}`);
    }
    if (content === null && existing === null) throw new NotFoundError(`no file at ${path}`);
    const blobSha = content === null ? null : repo.objects.write('blob', content);
    const newTree = updateTree(repo.objects, tipTree, segments, blobSha);
    const now = Math.floor(Date.now() / 1000);
    const who = ident(VIEWER.name, VIEWER.email, now);
    const commitSha = repo.objects.write(
      'commit',
      serializeCommit({
        tree: newTree,
        parents: tip === undefined ? [] : [tip],
        author: who,
        committer: who,
        message: message.endsWith('\n') ? message : `${message}\n`,
      }),
    );
    repo.refs.set(ref, commitSha);
    return { commitSha, blobSha, created: existing === null };
  }
}

// ------------------------------------------------------------------ merges and diffs

/** The best common ancestor of two commits, or null when they share no history. */
export function mergeBase(store: ObjectStore, a: string, b: string): string | null {
  const ancestorsOfA = new Set(ancestors(store, a));
  for (const candidate of ancestors(store, b)) {
    if (ancestorsOfA.has(candidate)) return candidate;
  }
  return null;
}

export interface MergeOutcome {
  conflict: boolean;
  treeSha: string | null;
}

/** A path-wise three-way merge of flattened trees. */
export function threeWayMerge(store: ObjectStore, baseTree: string | null, ourTree: string | null, theirTree: string | null): MergeOutcome {
  const base = flattenTree(store, baseTree);
  const ours = flattenTree(store, ourTree);
  const theirs = flattenTree(store, theirTree);
  const result = new Map<string, string>();
  const paths = new Set([...base.keys(), ...ours.keys(), ...theirs.keys()]);
  for (const path of paths) {
    const b = base.get(path) ?? null;
    const o = ours.get(path) ?? null;
    const t = theirs.get(path) ?? null;
    if (o === t) {
      if (o !== null) result.set(path, o);
    } else if (b === o) {
      if (t !== null) result.set(path, t);
    } else if (b === t) {
      if (o !== null) result.set(path, o);
    } else {
      return { conflict: true, treeSha: null };
    }
  }
  return { conflict: false, treeSha: buildTreeFromFlat(store, result) };
}

/** Whether the pull request (head into base, at their current tips) merges cleanly. */
export function pullMergeable(repo: Repo, pull: PullRec): boolean {
  const headTip = repo.tipOf(pull.headRef);
  const baseTip = repo.tipOf(pull.baseRef);
  if (headTip === undefined || baseTip === undefined) return false;
  if (isAncestor(repo.objects, headTip, baseTip)) return true; // already contained
  const base = mergeBase(repo.objects, baseTip, headTip);
  const baseTree = base === null ? null : repo.treeOfCommit(base);
  const outcome = threeWayMerge(repo.objects, baseTree, repo.treeOfCommit(baseTip), repo.treeOfCommit(headTip));
  return !outcome.conflict;
}

/**
 * Squash-merge the pull request onto its base branch: one commit whose tree is the three-way
 * merge, parented on the base tip. Returns the new commit sha, or null on conflict.
 */
export function squashMerge(repo: Repo, pull: PullRec): string | null {
  const headTip = repo.tipOf(pull.headRef);
  const baseTip = repo.tipOf(pull.baseRef);
  if (headTip === undefined || baseTip === undefined) return null;
  const base = mergeBase(repo.objects, baseTip, headTip);
  const baseTree = base === null ? null : repo.treeOfCommit(base);
  const outcome = threeWayMerge(repo.objects, baseTree, repo.treeOfCommit(baseTip), repo.treeOfCommit(headTip));
  if (outcome.conflict || outcome.treeSha === null) return null;
  const now = Math.floor(Date.now() / 1000);
  const who = ident(VIEWER.name, VIEWER.email, now);
  const commitSha = repo.objects.write(
    'commit',
    serializeCommit({
      tree: outcome.treeSha,
      parents: [baseTip],
      author: who,
      committer: who,
      message: `${pull.title} (#${pull.number})\n`,
    }),
  );
  repo.refs.set(`refs/heads/${pull.baseRef}`, commitSha);
  return commitSha;
}

export interface FileChange {
  filename: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
  changes: number;
}

/** Line counts of a change between two texts, from a longest-common-subsequence diff. */
export function lineDiff(oldText: string, newText: string): { additions: number; deletions: number } {
  if (oldText === newText) return { additions: 0, deletions: 0 };
  const a = oldText.split('\n');
  const b = newText.split('\n');
  // LCS over lines; the texts the suite diffs are small, and a cap keeps a pathological pair cheap.
  if (a.length * b.length > 4_000_000) {
    return { additions: b.length, deletions: a.length };
  }
  const dp = new Uint32Array((a.length + 1) * (b.length + 1));
  const width = b.length + 1;
  for (let i = a.length - 1; i >= 0; i -= 1) {
    for (let j = b.length - 1; j >= 0; j -= 1) {
      dp[i * width + j] = a[i] === b[j] ? (dp[(i + 1) * width + j + 1] as number) + 1 : Math.max(dp[(i + 1) * width + j] as number, dp[i * width + j + 1] as number);
    }
  }
  const common = dp[0] as number;
  return { additions: b.length - common, deletions: a.length - common };
}

/** The files a pull request changes: merge-base tree versus head tree. */
export function pullFiles(repo: Repo, pull: PullRec): FileChange[] {
  const headTip = repo.tipOf(pull.headRef);
  const baseTip = repo.tipOf(pull.baseRef);
  if (headTip === undefined || baseTip === undefined) return [];
  const base = mergeBase(repo.objects, baseTip, headTip);
  const from = flattenTree(repo.objects, base === null ? null : repo.treeOfCommit(base));
  const to = flattenTree(repo.objects, repo.treeOfCommit(headTip));
  const changes: FileChange[] = [];
  const paths = new Set([...from.keys(), ...to.keys()]);
  for (const path of [...paths].sort()) {
    const before = from.get(path) ?? null;
    const after = to.get(path) ?? null;
    if (before === after) continue;
    const oldText = before === null ? '' : (repo.objects.get(before)?.data.toString('utf8') ?? '');
    const newText = after === null ? '' : (repo.objects.get(after)?.data.toString('utf8') ?? '');
    const { additions, deletions } = before === null
      ? { additions: newText === '' ? 0 : newText.split('\n').length, deletions: 0 }
      : after === null
        ? { additions: 0, deletions: oldText === '' ? 0 : oldText.split('\n').length }
        : lineDiff(oldText, newText);
    changes.push({
      filename: path,
      status: before === null ? 'added' : after === null ? 'removed' : 'modified',
      additions,
      deletions,
      changes: additions + deletions,
    });
  }
  return changes;
}

export { ZERO_SHA };
