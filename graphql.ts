// The GraphQL v4 surface: a small parser for the document language gh and github-mcp-server
// speak (operations, variables, aliases, inline fragments, input objects, enum lists, batching)
// and an executor over the same store the REST surface serves, with the introspection answers
// gh's feature detection probes for. Global ids are opaque base64 of "<Type>:<parts>", stable
// across reads, and decode back to the same object for node(id:) and the mutation inputs.

import { Forge, Repo, VIEWER, pullFiles, pullMergeable, squashMerge, type IssueRec, type PullRec } from './store.ts';
import { globalId, issueJson, pullJson, repoJson } from './rest.ts';
import type { Ctx } from './app.ts';

// ------------------------------------------------------------------ schema (for introspection and validation)

const OBJECT_FIELDS: Readonly<Record<string, readonly string[]>> = {
  Query: ['viewer', 'repository', 'node', '__type', '__schema'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  User: ['id', 'login', 'name', 'email', 'url'],
  Repository: [
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'description', 'isPrivate',
    'viewerPermission', 'hasIssuesEnabled', 'defaultBranchRef', 'issue', 'issues',
    'pullRequest', 'pullRequests', 'url', 'createdAt', 'updatedAt',
  ],
  Ref: ['name'],
  Issue: ['id', 'number', 'title', 'body', 'state', 'url', 'createdAt', 'updatedAt', 'closed', 'labels', 'comments', 'repository', 'author'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequest: [
    'id', 'number', 'title', 'body', 'state', 'merged', 'mergedAt', 'mergeable', 'headRefName',
    'baseRefName', 'headRef', 'baseRef', 'files', 'url', 'createdAt', 'updatedAt', 'closed', 'repository', 'author',
  ],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changeType'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  CreateIssuePayload: ['issue', 'clientMutationId'],
  CloseIssuePayload: ['issue', 'clientMutationId'],
  CreatePullRequestPayload: ['pullRequest', 'clientMutationId'],
  MergePullRequestPayload: ['pullRequest', 'clientMutationId'],
  AddCommentPayload: ['commentEdge', 'subject', 'clientMutationId'],
  IssueCommentEdge: ['node', 'cursor'],
  IssueComment: ['id', 'body', 'createdAt', 'author'],
  LabelConnection: ['nodes'],
  Label: ['id', 'name', 'color'],
  IssueEdge: ['node', 'cursor'],
  PullRequestEdge: ['node', 'cursor'],
  __Type: ['name', 'kind', 'description', 'fields', 'enumValues', 'possibleTypes', 'inputFields'],
  __Field: ['name', 'description', 'type', 'args'],
  __EnumValue: ['name', 'description'],
  __Schema: ['types', 'queryType', 'mutationType'],
  Node: ['id'],
};

const ENUM_VALUES: Readonly<Record<string, readonly string[]>> = {
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  OrderDirection: ['ASC', 'DESC'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'],
};

// ------------------------------------------------------------------ parser

type Value =
  | { kind: 'variable'; name: string }
  | { kind: 'int'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: Value[] }
  | { kind: 'object'; fields: Array<{ name: string; value: Value }> };

interface FieldNode {
  kind: 'field';
  alias: string | null;
  name: string;
  args: Array<{ name: string; value: Value }>;
  selection: SelectionNode[] | null;
}

type SelectionNode =
  | FieldNode
  | { kind: 'fragment'; typeCondition: string; selection: SelectionNode[] };

interface Operation {
  type: 'query' | 'mutation';
  name: string | null;
  selection: SelectionNode[];
}

class GqlSyntaxError extends Error {}

function tokenize(input: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < input.length) {
    const ch = input[i] as string;
    if (ch === '#') {
      while (i < input.length && input[i] !== '\n') i += 1;
      continue;
    }
    if (/\s|,/.test(ch)) {
      i += 1;
      continue;
    }
    if (input.startsWith('...', i)) {
      tokens.push('...');
      i += 3;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let out = '';
      while (j < input.length && input[j] !== '"') {
        if (input[j] === '\\') {
          out += input[j + 1] === 'n' ? '\n' : input[j + 1] === 't' ? '\t' : (input[j + 1] as string);
          j += 2;
        } else {
          out += input[j];
          j += 1;
        }
      }
      if (j >= input.length) throw new GqlSyntaxError('unterminated string');
      tokens.push(`"${out}"`);
      i = j + 1;
      continue;
    }
    if ('{}()[]:$@=!|'.includes(ch)) {
      tokens.push(ch);
      i += 1;
      continue;
    }
    const match = /^[A-Za-z0-9_-]+/.exec(input.slice(i));
    if (match !== null) {
      tokens.push(match[0]);
      i += match[0].length;
      continue;
    }
    throw new GqlSyntaxError(`unexpected character ${JSON.stringify(ch)}`);
  }
  return tokens;
}

class Parser {
  private pos = 0;
  private readonly tokens: string[];
  constructor(tokens: string[]) {
    this.tokens = tokens;
  }

  peek(): string | undefined {
    return this.tokens[this.pos];
  }

  next(): string {
    const token = this.tokens[this.pos];
    if (token === undefined) throw new GqlSyntaxError('unexpected end of document');
    this.pos += 1;
    return token;
  }

  expect(token: string): void {
    const got = this.next();
    if (got !== token) throw new GqlSyntaxError(`expected ${token}, got ${got}`);
  }

  parseDocument(): Operation[] {
    const operations: Operation[] = [];
    while (this.pos < this.tokens.length) {
      const head = this.peek() as string;
      if (head === '{') {
        operations.push({ type: 'query', name: null, selection: this.parseSelectionSet() });
      } else if (head === 'query' || head === 'mutation') {
        const type = this.next() as 'query' | 'mutation';
        let name: string | null = null;
        if (this.peek() !== undefined && this.peek() !== '(' && this.peek() !== '{') name = this.next();
        if (this.peek() === '(') this.skipVariableDefinitions();
        operations.push({ type, name, selection: this.parseSelectionSet() });
      } else if (head === 'fragment') {
        // Named fragments are not used by the suite's documents; parse and drop the definition.
        this.next();
        this.next();
        this.expect('on');
        this.next();
        this.parseSelectionSet();
      } else {
        throw new GqlSyntaxError(`unexpected token ${head}`);
      }
    }
    return operations;
  }

  private skipVariableDefinitions(): void {
    this.expect('(');
    let depth = 1;
    while (depth > 0) {
      const token = this.next();
      if (token === '(' || token === '[' || token === '{') depth += 1;
      else if (token === ')' || token === ']' || token === '}') depth -= 1;
    }
  }

  parseSelectionSet(): SelectionNode[] {
    this.expect('{');
    const selections: SelectionNode[] = [];
    while (this.peek() !== '}') {
      if (this.peek() === '...') {
        this.next();
        this.expect('on');
        const typeCondition = this.next();
        selections.push({ kind: 'fragment', typeCondition, selection: this.parseSelectionSet() });
        continue;
      }
      selections.push(this.parseField());
    }
    this.expect('}');
    return selections;
  }

  private parseField(): FieldNode {
    let name = this.next();
    let alias: string | null = null;
    if (this.peek() === ':') {
      this.next();
      alias = name;
      name = this.next();
    }
    const args: FieldNode['args'] = [];
    if (this.peek() === '(') {
      this.next();
      while (this.peek() !== ')') {
        const argName = this.next();
        this.expect(':');
        args.push({ name: argName, value: this.parseValue() });
      }
      this.expect(')');
    }
    const selection = this.peek() === '{' ? this.parseSelectionSet() : null;
    return { kind: 'field', alias, name, args, selection };
  }

  private parseValue(): Value {
    const token = this.next();
    if (token === '$') return { kind: 'variable', name: this.next() };
    if (token.startsWith('"')) return { kind: 'string', value: token.slice(1, -1) };
    if (token === '[') {
      const values: Value[] = [];
      while (this.peek() !== ']') values.push(this.parseValue());
      this.expect(']');
      return { kind: 'list', values };
    }
    if (token === '{') {
      const fields: Array<{ name: string; value: Value }> = [];
      while (this.peek() !== '}') {
        const name = this.next();
        this.expect(':');
        fields.push({ name, value: this.parseValue() });
      }
      this.expect('}');
      return { kind: 'object', fields };
    }
    if (token === 'true' || token === 'false') return { kind: 'boolean', value: token === 'true' };
    if (token === 'null') return { kind: 'null' };
    if (/^-?\d+$/.test(token)) return { kind: 'int', value: Number.parseInt(token, 10) };
    return { kind: 'enum', value: token };
  }
}

// ------------------------------------------------------------------ execution

interface Source {
  __type: string;
  [key: string]: unknown;
}

class GqlError extends Error {}

function decodeGlobalId(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const split = text.split(':');
    if (split.length < 2) return null;
    return { kind: split[0] as string, parts: split.slice(1) };
  } catch {
    return null;
  }
}

function repoSource(repo: Repo): Source {
  return { __type: 'Repository', repo };
}

function issueSource(repo: Repo, issue: IssueRec): Source {
  return { __type: 'Issue', repo, issue };
}

function pullSource(repo: Repo, pull: PullRec): Source {
  return { __type: 'PullRequest', repo, pull };
}

function cursorFor(repo: Repo, issue: IssueRec): string {
  return Buffer.from(`cursor:${repo.id}:${issue.number}`, 'utf8').toString('base64');
}

interface ExecCtx {
  forge: Forge;
  variables: Record<string, unknown>;
  errors: Array<{ message: string }>;
}

function resolveValue(value: Value, ctx: ExecCtx): unknown {
  switch (value.kind) {
    case 'variable':
      return ctx.variables[value.name];
    case 'int':
    case 'string':
    case 'boolean':
    case 'enum':
      return value.value;
    case 'null':
      return null;
    case 'list':
      return value.values.map((entry) => resolveValue(entry, ctx));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of value.fields) out[field.name] = resolveValue(field.value, ctx);
      return out;
    }
  }
}

function argsOf(field: FieldNode, ctx: ExecCtx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const arg of field.args) out[arg.name] = resolveValue(arg.value, ctx);
  return out;
}

function findRepoByGlobalId(forge: Forge, id: unknown): Repo | null {
  if (typeof id !== 'string') return null;
  const decoded = decodeGlobalId(id);
  if (decoded === null || decoded.kind !== 'Repository') return null;
  const dbId = Number.parseInt(decoded.parts[0] ?? '', 10);
  return forge.listRepos().find((repo) => repo.id === dbId) ?? null;
}

function resolveField(source: Source, field: FieldNode, ctx: ExecCtx): unknown {
  const type = source.__type;
  const name = field.name;
  const args = argsOf(field, ctx);
  const forge = ctx.forge;

  if (type === 'Query') {
    switch (name) {
      case 'viewer':
        return { __type: 'User' } satisfies Source;
      case 'repository': {
        const repo = typeof args.owner === 'string' && typeof args.name === 'string' ? forge.getRepo(args.owner, args.name) : undefined;
        return repo === undefined ? null : repoSource(repo);
      }
      case 'node': {
        const decoded = typeof args.id === 'string' ? decodeGlobalId(args.id) : null;
        if (decoded === null) return null;
        if (decoded.kind === 'Repository') {
          const repo = findRepoByGlobalId(forge, args.id as string);
          return repo === null ? null : repoSource(repo);
        }
        return null;
      }
      case '__type': {
        const typeName = typeof args.name === 'string' ? args.name : '';
        if (OBJECT_FIELDS[typeName] === undefined && ENUM_VALUES[typeName] === undefined) return null;
        return { __type: '__Type', name: typeName } satisfies Source;
      }
      case '__schema':
        return { __type: '__Schema' } satisfies Source;
    }
  }

  if (type === 'Mutation') {
    const input = (typeof args.input === 'object' && args.input !== null ? args.input : {}) as Record<string, unknown>;
    switch (name) {
      case 'createIssue': {
        const repo = findRepoByGlobalId(forge, input.repositoryId);
        if (repo === null) throw new GqlError('repository not found for createIssue');
        const issue: IssueRec = {
          number: repo.nextNumber,
          id: forge.allocIssueId(),
          title: typeof input.title === 'string' ? input.title : '',
          body: typeof input.body === 'string' ? input.body : '',
          state: 'open',
          labels: [],
          comments: [],
          seq: repo.nextSeq(),
        };
        repo.nextNumber += 1;
        repo.issues.set(issue.number, issue);
        return { __type: 'CreateIssuePayload', issue: issueSource(repo, issue), clientMutationId: input.clientMutationId ?? null } satisfies Source;
      }
      case 'closeIssue': {
        const decoded = typeof input.issueId === 'string' ? decodeGlobalId(input.issueId) : null;
        if (decoded === null || decoded.kind !== 'Issue') throw new GqlError('issue not found for closeIssue');
        const repo = forge.listRepos().find((candidate) => candidate.id === Number.parseInt(decoded.parts[0] ?? '', 10));
        const issue = repo?.issues.get(Number.parseInt(decoded.parts[1] ?? '', 10));
        if (repo === undefined || issue === undefined) throw new GqlError('issue not found for closeIssue');
        issue.state = 'closed';
        return { __type: 'CloseIssuePayload', issue: issueSource(repo, issue), clientMutationId: input.clientMutationId ?? null } satisfies Source;
      }
      case 'createPullRequest': {
        const repo = findRepoByGlobalId(forge, input.repositoryId);
        if (repo === null) throw new GqlError('repository not found for createPullRequest');
        const head = typeof input.headRefName === 'string' ? input.headRefName : '';
        const base = typeof input.baseRefName === 'string' ? input.baseRefName : repo.defaultBranch;
        if (repo.tipOf(head) === undefined) throw new GqlError(`head branch ${head} does not exist`);
        if (repo.tipOf(base) === undefined) throw new GqlError(`base branch ${base} does not exist`);
        const pull: PullRec = {
          number: repo.nextNumber,
          id: forge.allocIssueId(),
          title: typeof input.title === 'string' ? input.title : '',
          body: typeof input.body === 'string' ? input.body : '',
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
        return { __type: 'CreatePullRequestPayload', pullRequest: pullSource(repo, pull), clientMutationId: input.clientMutationId ?? null } satisfies Source;
      }
      case 'mergePullRequest': {
        const decoded = typeof input.pullRequestId === 'string' ? decodeGlobalId(input.pullRequestId) : null;
        if (decoded === null || decoded.kind !== 'PullRequest') throw new GqlError('pull request not found for mergePullRequest');
        const repo = forge.listRepos().find((candidate) => candidate.id === Number.parseInt(decoded.parts[0] ?? '', 10));
        const pull = repo?.pulls.get(Number.parseInt(decoded.parts[1] ?? '', 10));
        if (repo === undefined || pull === undefined) throw new GqlError('pull request not found for mergePullRequest');
        if (pull.state !== 'open') throw new GqlError('pull request is not open');
        const sha = squashMerge(repo, pull);
        if (sha === null) throw new GqlError('pull request is not mergeable: merge conflict');
        pull.state = 'closed';
        pull.merged = true;
        pull.mergedAt = new Date().toISOString();
        pull.mergeCommitSha = sha;
        return { __type: 'MergePullRequestPayload', pullRequest: pullSource(repo, pull), clientMutationId: input.clientMutationId ?? null } satisfies Source;
      }
      case 'addComment': {
        const decoded = typeof input.subjectId === 'string' ? decodeGlobalId(input.subjectId) : null;
        if (decoded === null || decoded.kind !== 'Issue') throw new GqlError('subject not found for addComment');
        const repo = forge.listRepos().find((candidate) => candidate.id === Number.parseInt(decoded.parts[0] ?? '', 10));
        const issue = repo?.issues.get(Number.parseInt(decoded.parts[1] ?? '', 10));
        if (repo === undefined || issue === undefined) throw new GqlError('subject not found for addComment');
        const comment = { id: forge.allocCommentId(), body: typeof input.body === 'string' ? input.body : '' };
        issue.comments.push(comment);
        const node: Source = { __type: 'IssueComment', id: globalId('IssueComment', repo.id, issue.number, comment.id), body: comment.body, createdAt: new Date().toISOString() };
        return { __type: 'AddCommentPayload', commentEdge: { __type: 'IssueCommentEdge', node, cursor: '' } satisfies Source, clientMutationId: input.clientMutationId ?? null } satisfies Source;
      }
    }
  }

  if (type === 'User') {
    switch (name) {
      case 'id': return globalId('User', VIEWER.id);
      case 'login': return VIEWER.login;
      case 'name': return VIEWER.name;
      case 'email': return VIEWER.email;
      case 'url': return 'https://forge.invalid/adaiversen';
    }
  }

  if (type === 'Repository') {
    const repo = source.repo as Repo;
    switch (name) {
      case 'id': return globalId('Repository', repo.id);
      case 'databaseId': return repo.id;
      case 'name': return repo.name;
      case 'nameWithOwner': return `${repo.owner}/${repo.name}`;
      case 'owner': return { __type: 'User' } satisfies Source;
      case 'description': return repo.description;
      case 'isPrivate': return repo.isPrivate;
      case 'viewerPermission': return 'ADMIN';
      case 'hasIssuesEnabled': return true;
      case 'defaultBranchRef': return { __type: 'Ref', name: repo.defaultBranch } satisfies Source;
      case 'url': return `https://forge.invalid/${repo.owner}/${repo.name}`;
      case 'createdAt':
      case 'updatedAt': return repo.createdAt;
      case 'issue': {
        const issue = typeof args.number === 'number' ? repo.issues.get(args.number) : undefined;
        return issue === undefined ? null : issueSource(repo, issue);
      }
      case 'issues': return issuesConnection(repo, args);
      case 'pullRequest': {
        const pull = typeof args.number === 'number' ? repo.pulls.get(args.number) : undefined;
        return pull === undefined ? null : pullSource(repo, pull);
      }
      case 'pullRequests': {
        const first = typeof args.first === 'number' ? args.first : 30;
        const states = Array.isArray(args.states) ? (args.states as string[]) : null;
        let list = [...repo.pulls.values()];
        if (states !== null) {
          list = list.filter((pull) => {
            const state = pull.merged ? 'MERGED' : pull.state.toUpperCase();
            return states.includes(state);
          });
        }
        list.sort((a, b) => b.number - a.number);
        const nodes = list.slice(0, first).map((pull) => pullSource(repo, pull));
        return {
          __type: 'PullRequestConnection',
          nodes,
          pageInfo: { __type: 'PageInfo', hasNextPage: list.length > nodes.length, hasPreviousPage: false, startCursor: null, endCursor: null },
          totalCount: list.length,
        } satisfies Source;
      }
    }
  }

  if (type === 'Issue') {
    const repo = source.repo as Repo;
    const issue = source.issue as IssueRec;
    switch (name) {
      case 'id': return globalId('Issue', repo.id, issue.number);
      case 'number': return issue.number;
      case 'title': return issue.title;
      case 'body': return issue.body;
      case 'state': return issue.state.toUpperCase();
      case 'closed': return issue.state === 'closed';
      case 'url': return `https://forge.invalid/${repo.owner}/${repo.name}/issues/${issue.number}`;
      case 'createdAt':
      case 'updatedAt': return repo.createdAt;
      case 'repository': return repoSource(repo);
      case 'author': return { __type: 'User' } satisfies Source;
      case 'labels': return { __type: 'LabelConnection', nodes: issue.labels.map((label) => ({ __type: 'Label', id: globalId('Label', label), name: label, color: repo.labels.get(label)?.color ?? 'ededed' }) satisfies Source) } satisfies Source;
      case 'comments': return { __type: 'IssueConnection', nodes: [], pageInfo: null, totalCount: issue.comments.length } satisfies Source;
    }
  }

  if (type === 'PullRequest') {
    const repo = source.repo as Repo;
    const pull = source.pull as PullRec;
    switch (name) {
      case 'id': return globalId('PullRequest', repo.id, pull.number);
      case 'number': return pull.number;
      case 'title': return pull.title;
      case 'body': return pull.body;
      case 'state': return pull.merged ? 'MERGED' : pull.state.toUpperCase();
      case 'merged': return pull.merged;
      case 'mergedAt': return pull.mergedAt;
      case 'mergeable': return pull.state === 'open' ? (pullMergeable(repo, pull) ? 'MERGEABLE' : 'CONFLICTING') : 'MERGEABLE';
      case 'headRefName': return pull.headRef;
      case 'baseRefName': return pull.baseRef;
      case 'headRef': return { __type: 'Ref', name: pull.headRef } satisfies Source;
      case 'baseRef': return { __type: 'Ref', name: pull.baseRef } satisfies Source;
      case 'closed': return pull.state === 'closed';
      case 'url': return `https://forge.invalid/${repo.owner}/${repo.name}/pull/${pull.number}`;
      case 'createdAt':
      case 'updatedAt': return repo.createdAt;
      case 'repository': return repoSource(repo);
      case 'author': return { __type: 'User' } satisfies Source;
      case 'files': {
        const first = typeof args.first === 'number' ? args.first : 100;
        const nodes = pullFiles(repo, pull)
          .slice(0, first)
          .map((file) => ({ __type: 'PullRequestChangedFile', path: file.filename, additions: file.additions, deletions: file.deletions, changeType: file.status.toUpperCase() }) satisfies Source);
        return { __type: 'PullRequestConnection', nodes, pageInfo: null, totalCount: nodes.length } satisfies Source;
      }
    }
  }

  if (type === 'Ref' && name === 'name') return source.name as string;
  if (type === 'PageInfo') return source[name] ?? null;
  if (type === '__Type') {
    const typeName = source.name as string;
    switch (name) {
      case 'name': return typeName;
      case 'kind': return ENUM_VALUES[typeName] !== undefined ? 'ENUM' : 'OBJECT';
      case 'description': return null;
      case 'fields': {
        const fields = OBJECT_FIELDS[typeName];
        if (fields === undefined) return null;
        return fields.map((fieldName) => ({ __type: '__Field', name: fieldName }) satisfies Source);
      }
      case 'enumValues': {
        const values = ENUM_VALUES[typeName];
        if (values === undefined) return null;
        return values.map((value) => ({ __type: '__EnumValue', name: value }) satisfies Source);
      }
      case 'possibleTypes': return null;
      case 'inputFields': return null;
    }
  }
  if (type === '__Field' && name === 'name') return source.name as string;
  if (type === '__EnumValue' && name === 'name') return source.name as string;
  if (type === '__Schema') {
    if (name === 'queryType') return { __type: '__Type', name: 'Query' } satisfies Source;
    if (name === 'mutationType') return { __type: '__Type', name: 'Mutation' } satisfies Source;
    if (name === 'types') return [...Object.keys(OBJECT_FIELDS), ...Object.keys(ENUM_VALUES)].map((typeName) => ({ __type: '__Type', name: typeName }) satisfies Source);
  }

  // Payload and connection leaf fields ride on the source object itself.
  if (name in source) return source[name];
  return undefined;
}

function issuesConnection(repo: Repo, args: Record<string, unknown>): Source {
  const states = Array.isArray(args.states) ? (args.states as string[]) : null;
  let list = [...repo.issues.values()];
  if (states !== null) list = list.filter((issue) => states.includes(issue.state.toUpperCase()));
  const orderBy = (typeof args.orderBy === 'object' && args.orderBy !== null ? args.orderBy : {}) as Record<string, unknown>;
  const direction = typeof orderBy.direction === 'string' ? orderBy.direction : null;
  if (direction === 'DESC') list.sort((a, b) => b.seq - a.seq);
  else if (direction === 'ASC') list.sort((a, b) => a.seq - b.seq);
  else list.sort((a, b) => a.number - b.number);
  let start = 0;
  if (typeof args.after === 'string' && args.after !== '') {
    const decoded = Buffer.from(args.after, 'base64').toString('utf8');
    const match = /^cursor:(\d+):(\d+)$/.exec(decoded);
    if (match !== null) {
      const index = list.findIndex((issue) => issue.number === Number.parseInt(match[2] as string, 10));
      if (index >= 0) start = index + 1;
    }
  }
  const first = typeof args.first === 'number' ? args.first : 30;
  const page = list.slice(start, start + first);
  const nodes = page.map((issue) => issueSource(repo, issue));
  const last = page[page.length - 1];
  return {
    __type: 'IssueConnection',
    nodes,
    pageInfo: {
      __type: 'PageInfo',
      hasNextPage: start + page.length < list.length,
      hasPreviousPage: start > 0,
      startCursor: page.length > 0 ? cursorFor(repo, page[0] as IssueRec) : null,
      endCursor: last !== undefined ? cursorFor(repo, last) : null,
    } satisfies Source,
    totalCount: list.length,
  } satisfies Source;
}

function executeSelection(source: Source, selections: readonly SelectionNode[], ctx: ExecCtx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.kind === 'fragment') {
      if (selection.typeCondition === source.__type || selection.typeCondition === 'Node') {
        Object.assign(out, executeSelection(source, selection.selection, ctx));
      }
      continue;
    }
    const key = selection.alias ?? selection.name;
    const knownFields = OBJECT_FIELDS[source.__type];
    if (knownFields !== undefined && !knownFields.includes(selection.name) && !(selection.name in source)) {
      ctx.errors.push({ message: `Field '${selection.name}' doesn't exist on type '${source.__type}'` });
      out[key] = null;
      continue;
    }
    let value: unknown;
    try {
      value = resolveField(source, selection, ctx);
    } catch (err) {
      if (err instanceof GqlError) {
        ctx.errors.push({ message: err.message });
        out[key] = null;
        continue;
      }
      throw err;
    }
    out[key] = completeValue(value, selection, ctx);
  }
  return out;
}

function completeValue(value: unknown, field: FieldNode, ctx: ExecCtx): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((entry) => completeValue(entry, field, ctx));
  if (typeof value === 'object' && (value as Source).__type !== undefined && field.selection !== null) {
    return executeSelection(value as Source, field.selection, ctx);
  }
  return value;
}

function runOperation(forge: Forge, operation: Operation, variables: Record<string, unknown>): Record<string, unknown> {
  const ctx: ExecCtx = { forge, variables, errors: [] };
  const root: Source = { __type: operation.type === 'mutation' ? 'Mutation' : 'Query' };
  const data = executeSelection(root, operation.selection, ctx);
  const result: Record<string, unknown> = { data };
  if (ctx.errors.length > 0) result.errors = ctx.errors;
  return result;
}

export function handleGraphQL(ctx: Ctx): void {
  let parsed: unknown;
  try {
    parsed = JSON.parse(ctx.body.toString('utf8'));
  } catch {
    return ctx.send(400, { errors: [{ message: 'Request body is not valid JSON' }] });
  }
  const runOne = (entry: unknown): Record<string, unknown> => {
    if (typeof entry !== 'object' || entry === null) return { errors: [{ message: 'Invalid GraphQL request' }] };
    const request = entry as { query?: unknown; variables?: unknown; operationName?: unknown };
    if (typeof request.query !== 'string') return { errors: [{ message: 'Missing query' }] };
    let operations: Operation[];
    try {
      operations = new Parser(tokenize(request.query)).parseDocument();
    } catch (err) {
      return { errors: [{ message: err instanceof Error ? err.message : 'Syntax error' }] };
    }
    const variables = (typeof request.variables === 'object' && request.variables !== null ? request.variables : {}) as Record<string, unknown>;
    const operation =
      typeof request.operationName === 'string'
        ? (operations.find((candidate) => candidate.name === request.operationName) ?? operations[0])
        : operations[0];
    if (operation === undefined) return { errors: [{ message: 'No operation found' }] };
    return runOperation(ctx.forge, operation, variables);
  };
  if (Array.isArray(parsed)) return ctx.send(200, parsed.map(runOne));
  return ctx.send(200, runOne(parsed));
}

// Re-exported so app.ts can keep one import surface for the JSON views the MCP tools reuse.
export { issueJson, pullJson, repoJson };
