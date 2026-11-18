// The REST v3 surface: repositories, branches, commits, contents, labels, issues, comments and
// pull requests, in the shapes GitHub's clients read. Handlers receive the shared Ctx from
// app.ts and answer through its JSON helpers; ownership is the store's rule (the token's login
// owns every repository here, and the same name under another owner is a 404).

import { ZERO_SHA, ancestors, parseCommit, parseTree } from './git.ts';
import {
  Forge,
  NotFoundError,
  Repo,
  StaleShaError,
  VIEWER,
  flattenTree,
  pullFiles,
  pullMergeable,
  squashMerge,
  treeLookup,
  type IssueRec,
  type PullRec,
} from './store.ts';
import type { Ctx } from './app.ts';

type Json = Record<string, unknown>;

// ------------------------------------------------------------------ JSON views

export function userJson(): Json {
  return { login: VIEWER.login, id: VIEWER.id, type: 'User', name: VIEWER.name, email: VIEWER.email };
}

export function globalId(kind: string, ...parts: Array<string | number>): string {
  return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64');
}

export function repoJson(repo: Repo): Json {
  return {
    id: repo.id,
    node_id: globalId('Repository', repo.id),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER.id, type: 'User' },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    has_issues: true,
    created_at: repo.createdAt,
    updated_at: repo.createdAt,
    html_url: `https://forge.invalid/${repo.owner}/${repo.name}`,
    clone_url: `https://forge.invalid/${repo.owner}/${repo.name}.git`,
  };
}

export function labelJson(repo: Repo, name: string): Json {
  const label = repo.labels.get(name);
  return { name, color: label?.color ?? 'ededed', description: null, default: false };
}

export function issueJson(repo: Repo, issue: IssueRec): Json {
  return {
    id: issue.id,
    node_id: globalId('Issue', repo.id, issue.number),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => labelJson(repo, name)),
    user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    comments: issue.comments.length,
    pull_request: undefined,
    created_at: repo.createdAt,
    updated_at: repo.createdAt,
  };
}

export function commentJson(comment: { id: number; body: string }): Json {
  return {
    id: comment.id,
    node_id: globalId('IssueComment', comment.id),
    body: comment.body,
    user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };
}

export function pullJson(repo: Repo, pull: PullRec): Json {
  const headSha = repo.tipOf(pull.headRef) ?? ZERO_SHA;
  const baseSha = repo.tipOf(pull.baseRef) ?? ZERO_SHA;
  return {
    id: pull.id,
    node_id: globalId('PullRequest', repo.id, pull.number),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    mergeable: pull.state === 'open' ? pullMergeable(repo, pull) : null,
    user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    head: { label: `${repo.owner}:${pull.headRef}`, ref: pull.headRef, sha: headSha },
    base: { label: `${repo.owner}:${pull.baseRef}`, ref: pull.baseRef, sha: baseSha },
    created_at: repo.createdAt,
    updated_at: repo.createdAt,
  };
}

// ------------------------------------------------------------------ helpers

function pageParams(ctx: Ctx): { page: number; perPage: number } {
  const page = Math.max(1, Number.parseInt(ctx.url.searchParams.get('page') ?? '1', 10) || 1);
  const perPage = Math.min(100, Math.max(1, Number.parseInt(ctx.url.searchParams.get('per_page') ?? '30', 10) || 30));
  return { page, perPage };
}

function paginate<T>(ctx: Ctx, items: readonly T[]): T[] {
  const { page, perPage } = pageParams(ctx);
  const start = (page - 1) * perPage;
  if (start + perPage < items.length) {
    const base = `${ctx.origin}${ctx.url.pathname}`;
    const next = `${base}?per_page=${perPage}&page=${page + 1}`;
    const last = `${base}?per_page=${perPage}&page=${Math.ceil(items.length / perPage)}`;
    ctx.res.setHeader('Link', `<${next}>; rel="next", <${last}>; rel="last"`);
  }
  return items.slice(start, start + perPage);
}

function bodyObject(ctx: Ctx): Json {
  const parsed = ctx.json();
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return {};
  return parsed as Json;
}

function branchOf(value: unknown): string {
  if (typeof value !== 'string') return '';
  const colon = value.indexOf(':');
  return colon === -1 ? value : value.slice(colon + 1);
}

// ------------------------------------------------------------------ the router

export function handleRest(ctx: Ctx, segments: readonly string[]): void {
  const forge: Forge = ctx.forge;
  const [head, ...rest] = segments;

  if (segments.length === 0) {
    if (ctx.req.method !== 'GET') return ctx.notFound();
    return ctx.send(200, {
      current_user_url: `${ctx.origin}/api/v3/user`,
      repository_url: `${ctx.origin}/api/v3/repos/{owner}/{repo}`,
      user_repositories_url: `${ctx.origin}/api/v3/user/repos`,
      issue_search_url: `${ctx.origin}/api/v3/search/issues`,
    });
  }

  if (head === 'meta' && segments.length === 1) {
    if (ctx.req.method !== 'GET') return ctx.notFound();
    return ctx.send(200, { installed_version: '3.10.0-forge', verifiable_password_authentication: false });
  }

  if (head === 'user' && segments.length === 1) {
    if (ctx.req.method !== 'GET') return ctx.notFound();
    return ctx.send(200, userJson());
  }

  if (head === 'user' && rest[0] === 'repos' && segments.length === 2) {
    if (ctx.req.method === 'GET') {
      const repos = forge.listRepos().map(repoJson);
      return ctx.send(200, paginate(ctx, repos));
    }
    if (ctx.req.method === 'POST') {
      const body = bodyObject(ctx);
      const name = typeof body.name === 'string' ? body.name : '';
      if (name === '') return ctx.fail(422, 'Validation Failed: name is required');
      const created = forge.createRepo(name, typeof body.description === 'string' ? body.description : '', body.private !== false);
      if (created === 'exists') return ctx.fail(422, 'Validation Failed: name already exists on this account');
      if (body.auto_init === true) {
        ctx.forge.writeFile(created, created.defaultBranch, 'README.md', Buffer.from(`# ${name}\n`, 'utf8'), 'Initial commit');
      }
      return ctx.send(201, repoJson(created));
    }
    return ctx.notFound();
  }

  if (head === 'repos' && rest.length >= 2) {
    const owner = decodeURIComponent(rest[0] as string);
    const name = decodeURIComponent(rest[1] as string);
    const repo = forge.getRepo(owner, name);
    if (repo === undefined) return ctx.fail(404, 'Not Found');
    return handleRepo(ctx, repo, rest.slice(2));
  }

  return ctx.notFound();
}

function handleRepo(ctx: Ctx, repo: Repo, segments: readonly string[]): void {
  const method = ctx.req.method ?? 'GET';
  if (segments.length === 0) {
    if (method === 'GET') return ctx.send(200, repoJson(repo));
    if (method === 'PATCH') {
      const body = bodyObject(ctx);
      if (typeof body.description === 'string') repo.description = body.description;
      if (typeof body.private === 'boolean') repo.isPrivate = body.private;
      if (typeof body.name === 'string' && body.name !== '' && body.name !== repo.name) {
        // Renaming moves the map key; nothing in the suite renames, but keep the store coherent.
        ctx.forge.repos.delete(ctx.forge.repoKey(repo.owner, repo.name));
        (repo as { name: string }).name = body.name;
        ctx.forge.repos.set(ctx.forge.repoKey(repo.owner, repo.name), repo);
      }
      return ctx.send(200, repoJson(repo));
    }
    if (method === 'DELETE') {
      ctx.forge.deleteRepo(repo.owner, repo.name);
      return ctx.sendNoContent();
    }
    return ctx.notFound();
  }

  const [head, ...rest] = segments as [string, ...(readonly string[])];
  switch (head) {
    case 'branches':
      return handleBranches(ctx, repo, rest);
    case 'commits':
      return handleCommits(ctx, repo, rest);
    case 'contents':
      return handleContents(ctx, repo, rest.join('/'));
    case 'git':
      return handleGitData(ctx, repo, rest);
    case 'labels':
      return handleLabels(ctx, repo, rest);
    case 'issues':
      return handleIssues(ctx, repo, rest);
    case 'pulls':
      return handlePulls(ctx, repo, rest);
    default:
      return ctx.notFound();
  }
}

// ------------------------------------------------------------------ branches & commits

function handleBranches(ctx: Ctx, repo: Repo, segments: readonly string[]): void {
  if (segments.length === 0) {
    if (ctx.req.method !== 'GET') return ctx.notFound();
    const list = [...repo.refs.entries()]
      .filter(([ref]) => ref.startsWith('refs/heads/'))
      .map(([ref, sha]) => ({ name: ref.slice('refs/heads/'.length), commit: { sha }, protected: false }))
      .sort((a, b) => (a.name < b.name ? -1 : 1));
    return ctx.send(200, paginate(ctx, list));
  }
  if (ctx.req.method !== 'GET') return ctx.notFound();
  const branch = segments.map(decodeURIComponent).join('/');
  const sha = repo.tipOf(branch);
  if (sha === undefined) return ctx.fail(404, 'Branch not found');
  return ctx.send(200, { name: branch, commit: { sha }, protected: false });
}

function commitJson(repo: Repo, sha: string): Json {
  const obj = repo.objects.get(sha);
  const parsed = obj !== undefined && obj.type === 'commit' ? parseCommit(obj.data) : null;
  const identOf = (line: string): Json => {
    const match = /^(.*) <(.*)> (\d+) /.exec(line);
    return {
      name: match?.[1] ?? '',
      email: match?.[2] ?? '',
      date: new Date(Number(match?.[3] ?? 0) * 1000).toISOString(),
    };
  };
  return {
    sha,
    commit: {
      message: parsed?.message.replace(/\n$/, '') ?? '',
      tree: { sha: parsed?.tree ?? ZERO_SHA },
      author: identOf(parsed?.author ?? ''),
      committer: identOf(parsed?.committer ?? ''),
    },
    parents: (parsed?.parents ?? []).map((parent) => ({ sha: parent })),
  };
}

function handleCommits(ctx: Ctx, repo: Repo, segments: readonly string[]): void {
  if (ctx.req.method !== 'GET') return ctx.notFound();
  if (segments.length === 1) {
    const sha = decodeURIComponent(segments[0] as string);
    if (!repo.objects.has(sha)) return ctx.fail(404, 'Not Found');
    return ctx.send(200, commitJson(repo, sha));
  }
  const shaParam = ctx.url.searchParams.get('sha');
  let tip: string | undefined;
  if (shaParam === null || shaParam === '') tip = repo.tipOf(repo.defaultBranch);
  else if (/^[0-9a-f]{40}$/i.test(shaParam)) tip = repo.objects.has(shaParam) ? shaParam : undefined;
  else tip = repo.tipOf(shaParam);
  if (tip === undefined) return ctx.send(200, []);
  const list = ancestors(repo.objects, tip).map((sha) => commitJson(repo, sha));
  return ctx.send(200, paginate(ctx, list));
}

// ------------------------------------------------------------------ contents

function handleContents(ctx: Ctx, repo: Repo, rawPath: string): void {
  const path = rawPath.split('/').map(decodeURIComponent).join('/');
  const method = ctx.req.method ?? 'GET';

  if (method === 'GET') {
    const refParam = ctx.url.searchParams.get('ref');
    let tip: string | undefined;
    if (refParam === null || refParam === '') tip = repo.tipOf(repo.defaultBranch);
    else if (/^[0-9a-f]{40}$/i.test(refParam) && repo.objects.has(refParam)) tip = refParam;
    else tip = repo.tipOf(refParam);
    if (tip === undefined) return ctx.fail(404, 'Not Found');
    const tree = repo.treeOfCommit(tip);
    if (path === '') return listTree(ctx, repo, tree, '');
    const found = treeLookup(repo.objects, tree, path.split('/'));
    if (found === null) return ctx.fail(404, 'Not Found');
    if (found.mode === '40000') return listTree(ctx, repo, found.sha, path);
    const blob = repo.objects.get(found.sha);
    if (blob === undefined) return ctx.fail(404, 'Not Found');
    const name = path.split('/').pop() as string;
    return ctx.send(200, {
      type: 'file',
      encoding: 'base64',
      content: blob.data.toString('base64'),
      name,
      path,
      sha: found.sha,
      size: blob.data.length,
    });
  }

  if (method === 'PUT') {
    const body = bodyObject(ctx);
    const message = typeof body.message === 'string' ? body.message : 'Update file';
    const branch = typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
    const content = typeof body.content === 'string' ? Buffer.from(body.content, 'base64') : Buffer.alloc(0);
    try {
      const result = ctx.forge.writeFile(repo, branch, path, content, message, {
        expectSha: typeof body.sha === 'string' ? body.sha : undefined,
        createFrom: repo.defaultBranch,
      });
      const name = path.split('/').pop() as string;
      return ctx.send(result.created ? 201 : 200, {
        content: { name, path, sha: result.blobSha, size: content.length, encoding: 'base64', type: 'file' },
        commit: { sha: result.commitSha, message },
      });
    } catch (err) {
      if (err instanceof StaleShaError) return ctx.fail(409, 'Conflict: the file sha does not match');
      throw err;
    }
  }

  if (method === 'DELETE') {
    const body = bodyObject(ctx);
    const message = typeof body.message === 'string' ? body.message : 'Delete file';
    const branch = typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
    try {
      const result = ctx.forge.writeFile(repo, branch, path, null, message, {
        expectSha: typeof body.sha === 'string' ? body.sha : undefined,
      });
      return ctx.send(200, { content: null, commit: { sha: result.commitSha, message } });
    } catch (err) {
      if (err instanceof StaleShaError) return ctx.fail(409, 'Conflict: the file sha does not match');
      if (err instanceof NotFoundError) return ctx.fail(404, 'Not Found');
      throw err;
    }
  }

  return ctx.notFound();
}

function listTree(ctx: Ctx, repo: Repo, treeSha: string | null, prefix: string): void {
  if (treeSha === null) return ctx.fail(404, 'Not Found');
  const obj = repo.objects.get(treeSha);
  if (obj === undefined || obj.type !== 'tree') return ctx.fail(404, 'Not Found');
  const entries = parseTree(obj.data).map((entry) => {
    const isDir = entry.mode === '40000';
    const blob = isDir ? undefined : repo.objects.get(entry.sha);
    return {
      type: isDir ? 'dir' : 'file',
      name: entry.name,
      path: prefix === '' ? entry.name : `${prefix}/${entry.name}`,
      sha: entry.sha,
      size: blob?.data.length ?? 0,
    };
  });
  entries.sort((a, b) => (a.name < b.name ? -1 : 1));
  return ctx.send(200, entries);
}

// ------------------------------------------------------------------ git data (refs)

function handleGitData(ctx: Ctx, repo: Repo, segments: readonly string[]): void {
  if (segments[0] !== 'refs') return ctx.notFound();
  if (segments.length === 1) {
    if (ctx.req.method !== 'POST') return ctx.notFound();
    const body = bodyObject(ctx);
    const ref = typeof body.ref === 'string' ? body.ref : '';
    const sha = typeof body.sha === 'string' ? body.sha : '';
    if (!ref.startsWith('refs/') || !repo.objects.has(sha)) return ctx.fail(422, 'Validation Failed: invalid ref or sha');
    repo.refs.set(ref, sha);
    return ctx.send(201, { ref, object: { sha, type: repo.objects.get(sha)?.type ?? 'commit' } });
  }
  if (ctx.req.method !== 'GET') return ctx.notFound();
  const ref = `refs/${segments.slice(1).map(decodeURIComponent).join('/')}`;
  const sha = repo.refs.get(ref);
  if (sha === undefined) return ctx.fail(404, 'Reference not found');
  return ctx.send(200, { ref, object: { sha, type: repo.objects.get(sha)?.type ?? 'commit' } });
}

// ------------------------------------------------------------------ labels

function handleLabels(ctx: Ctx, repo: Repo, segments: readonly string[]): void {
  if (segments.length === 0) {
    if (ctx.req.method === 'GET') return ctx.send(200, [...repo.labels.values()].map((label) => labelJson(repo, label.name)));
    if (ctx.req.method === 'POST') {
      const body = bodyObject(ctx);
      const name = typeof body.name === 'string' ? body.name : '';
      if (name === '') return ctx.fail(422, 'Validation Failed: name is required');
      if (repo.labels.has(name)) return ctx.fail(422, 'Validation Failed: label already exists');
      repo.labels.set(name, { name, color: typeof body.color === 'string' ? body.color.replace(/^#/, '') : 'ededed' });
      return ctx.send(201, labelJson(repo, name));
    }
  }
  return ctx.notFound();
}

// ------------------------------------------------------------------ issues

function handleIssues(ctx: Ctx, repo: Repo, segments: readonly string[]): void {
  const method = ctx.req.method ?? 'GET';
  if (segments.length === 0) {
    if (method === 'GET') {
      const state = ctx.url.searchParams.get('state') ?? 'open';
      const direction = ctx.url.searchParams.get('direction') ?? 'desc';
      let list = [...repo.issues.values()];
      if (state === 'open' || state === 'closed') list = list.filter((issue) => issue.state === state);
      list.sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
      return ctx.send(200, paginate(ctx, list.map((issue) => issueJson(repo, issue))));
    }
    if (method === 'POST') {
      const body = bodyObject(ctx);
      const title = typeof body.title === 'string' ? body.title : '';
      if (title === '') return ctx.fail(422, 'Validation Failed: title is required');
      const labels = Array.isArray(body.labels) ? body.labels.map((entry) => (typeof entry === 'string' ? entry : '')).filter(Boolean) : [];
      for (const label of labels) {
        if (!repo.labels.has(label)) return ctx.fail(422, `Validation Failed: label '${label}' does not exist`);
      }
      const issue: IssueRec = {
        number: repo.nextNumber,
        id: ctx.forge.allocIssueId(),
        title,
        body: typeof body.body === 'string' ? body.body : '',
        state: 'open',
        labels,
        comments: [],
        seq: repo.nextSeq(),
      };
      repo.nextNumber += 1;
      repo.issues.set(issue.number, issue);
      return ctx.send(201, issueJson(repo, issue));
    }
    return ctx.notFound();
  }

  const number = Number.parseInt(segments[0] as string, 10);
  const issue = Number.isInteger(number) ? repo.issues.get(number) : undefined;
  if (issue === undefined) return ctx.fail(404, 'Issue not found');

  if (segments.length === 1) {
    if (method === 'GET') return ctx.send(200, issueJson(repo, issue));
    if (method === 'PATCH') {
      const body = bodyObject(ctx);
      if (body.state === 'open' || body.state === 'closed') issue.state = body.state;
      if (typeof body.title === 'string') issue.title = body.title;
      if (typeof body.body === 'string') issue.body = body.body;
      if (Array.isArray(body.labels)) {
        const labels = body.labels.map((entry) => (typeof entry === 'string' ? entry : '')).filter(Boolean);
        for (const label of labels) {
          if (!repo.labels.has(label)) return ctx.fail(422, `Validation Failed: label '${label}' does not exist`);
        }
        issue.labels = labels;
      }
      return ctx.send(200, issueJson(repo, issue));
    }
    return ctx.notFound();
  }

  if (segments.length === 2 && segments[1] === 'comments') {
    if (method === 'GET') return ctx.send(200, paginate(ctx, issue.comments.map(commentJson)));
    if (method === 'POST') {
      const body = bodyObject(ctx);
      const comment = { id: ctx.forge.allocCommentId(), body: typeof body.body === 'string' ? body.body : '' };
      issue.comments.push(comment);
      return ctx.send(201, commentJson(comment));
    }
  }
  return ctx.notFound();
}

// ------------------------------------------------------------------ pull requests

function handlePulls(ctx: Ctx, repo: Repo, segments: readonly string[]): void {
  const method = ctx.req.method ?? 'GET';
  if (segments.length === 0) {
    if (method === 'GET') {
      const state = ctx.url.searchParams.get('state') ?? 'open';
      let list = [...repo.pulls.values()];
      if (state === 'open') list = list.filter((pull) => pull.state === 'open');
      else if (state === 'closed') list = list.filter((pull) => pull.state === 'closed');
      list.sort((a, b) => a.seq - b.seq);
      return ctx.send(200, paginate(ctx, list.map((pull) => pullJson(repo, pull))));
    }
    if (method === 'POST') {
      const body = bodyObject(ctx);
      const head = branchOf(body.head);
      const base = branchOf(body.base) || repo.defaultBranch;
      if (head === '' || repo.tipOf(head) === undefined) return ctx.fail(422, 'Validation Failed: head branch does not exist');
      if (repo.tipOf(base) === undefined) return ctx.fail(422, 'Validation Failed: base branch does not exist');
      if (head === base) return ctx.fail(422, 'Validation Failed: head and base are the same');
      for (const existing of repo.pulls.values()) {
        if (existing.state === 'open' && existing.headRef === head && existing.baseRef === base) {
          return ctx.fail(422, 'Validation Failed: a pull request already exists for this branch');
        }
      }
      const pull: PullRec = {
        number: repo.nextNumber,
        id: ctx.forge.allocIssueId(),
        title: typeof body.title === 'string' ? body.title : '',
        body: typeof body.body === 'string' ? body.body : '',
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
      return ctx.send(201, pullJson(repo, pull));
    }
    return ctx.notFound();
  }

  const number = Number.parseInt(segments[0] as string, 10);
  const pull = Number.isInteger(number) ? repo.pulls.get(number) : undefined;
  if (pull === undefined) return ctx.fail(404, 'Pull Request not found');

  if (segments.length === 1) {
    if (method === 'GET') return ctx.send(200, pullJson(repo, pull));
    return ctx.notFound();
  }

  if (segments.length === 2 && segments[1] === 'files') {
    if (method !== 'GET') return ctx.notFound();
    const files = pullFiles(repo, pull).map((file) => ({
      filename: file.filename,
      status: file.status,
      additions: file.additions,
      deletions: file.deletions,
      changes: file.changes,
      sha: repo.tipOf(pull.headRef) ?? ZERO_SHA,
    }));
    return ctx.send(200, paginate(ctx, files));
  }

  if (segments.length === 2 && segments[1] === 'merge') {
    if (method !== 'PUT') return ctx.notFound();
    if (pull.state !== 'open') return ctx.fail(405, 'Pull Request is not mergeable');
    const sha = squashMerge(repo, pull);
    if (sha === null) return ctx.fail(405, 'Pull Request is not mergeable: merge conflict');
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = new Date().toISOString();
    pull.mergeCommitSha = sha;
    return ctx.send(200, { sha, merged: true, message: 'Pull Request successfully merged' });
  }

  return ctx.notFound();
}
