import { Buffer } from 'node:buffer';
import { isPullMergeable, mergePull, pullChangedFiles } from './merge.ts';
import type { Comment, Issue, Label, Pull, Repo, Store } from './store.ts';

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'literal'; value: any }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: Record<string, ValueNode> };

interface ArgNode { name: string; value: ValueNode }
type Selection =
  | { kind: 'Field'; alias?: string; name: string; args: ArgNode[]; selections: Selection[] }
  | { kind: 'InlineFragment'; typeCondition: string; selections: Selection[] }
  | { kind: 'FragmentSpread'; name: string };
interface VariableDef { name: string; type: string; defaultValue?: ValueNode }
interface Operation { kind: 'query' | 'mutation'; name?: string; vars: VariableDef[]; selections: Selection[] }
interface Token { kind: 'name' | 'string' | 'number' | 'punct' | 'eof'; value: string }

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < source.length) {
    const c = source[i];
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',') { i++; continue; }
    if (c === '#') {
      while (i < source.length && source[i] !== '\n') i++;
      continue;
    }
    if (source.startsWith('"""', i)) {
      i += 3;
      let text = '';
      while (i < source.length && !source.startsWith('"""', i)) {
        if (source.startsWith('\\"""', i)) { text += '"""'; i += 4; }
        else { text += source[i++]; }
      }
      if (source.startsWith('"""', i)) i += 3;
      tokens.push({ kind: 'string', value: text });
      continue;
    }
    if (c === '"') {
      i++;
      let text = '';
      while (i < source.length && source[i] !== '"') {
        if (source[i] === '\\') {
          i++;
          const esc = source[i++];
          if (esc === 'n') text += '\n';
          else if (esc === 't') text += '\t';
          else if (esc === 'r') text += '\r';
          else if (esc === 'b') text += '\b';
          else if (esc === 'f') text += '\f';
          else if (esc === 'u' && i + 4 <= source.length) {
            const hex = source.slice(i, i + 4);
            if (/^[0-9a-fA-F]{4}$/.test(hex)) text += String.fromCharCode(parseInt(hex, 16));
            i += 4;
          } else text += esc ?? '';
        } else {
          text += source[i++];
        }
      }
      if (i < source.length) i++;
      tokens.push({ kind: 'string', value: text });
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i + 1;
      while (j < source.length && /[A-Za-z0-9_]/.test(source[j])) j++;
      tokens.push({ kind: 'name', value: source.slice(i, j) });
      i = j;
      continue;
    }
    if (/[0-9]/.test(c) || (c === '-' && i + 1 < source.length && /[0-9]/.test(source[i + 1]))) {
      let j = i;
      if (source[j] === '-') j++;
      while (j < source.length && /[0-9]/.test(source[j])) j++;
      if (source[j] === '.' && j + 1 < source.length && /[0-9]/.test(source[j + 1])) {
        j++;
        while (j < source.length && /[0-9]/.test(source[j])) j++;
      }
      if (source[j] === 'e' || source[j] === 'E') {
        j++;
        if (source[j] === '+' || source[j] === '-') j++;
        while (j < source.length && /[0-9]/.test(source[j])) j++;
      }
      tokens.push({ kind: 'number', value: source.slice(i, j) });
      i = j;
      continue;
    }
    if (source.startsWith('...', i)) {
      tokens.push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('$!:&{()[]@=|}'.includes(c)) {
      tokens.push({ kind: 'punct', value: c });
      i++;
      continue;
    }
    i++;
  }
  tokens.push({ kind: 'eof', value: '' });
  return tokens;
}

class Parser {
  tokens: Token[];
  pos = 0;

  constructor(tokens: Token[]) { this.tokens = tokens; }

  peek(): Token { return this.tokens[this.pos] ?? { kind: 'eof', value: '' }; }
  next(): Token { return this.tokens[this.pos++] ?? { kind: 'eof', value: '' }; }
  eat(value: string): boolean {
    const t = this.peek();
    if (t.kind === 'punct' && t.value === value) { this.pos++; return true; }
    return false;
  }
  expect(value: string): void {
    if (!this.eat(value)) throw new Error(`Expected "${value}" in GraphQL document`);
  }
  skipName(value: string): boolean {
    const t = this.peek();
    if (t.kind === 'name' && t.value === value) { this.pos++; return true; }
    return false;
  }
  expectName(): string {
    const t = this.next();
    if (t.kind !== 'name') throw new Error(`Expected a name in GraphQL document, got "${t.value}"`);
    return t.value;
  }

  parseDocument(): Operation[] {
    const ops: Operation[] = [];
    while (this.peek().kind !== 'eof') {
      if (this.eat('{')) {
        ops.push({ kind: 'query', vars: [], selections: this.parseSelectionList() });
        continue;
      }
      const first = this.peek();
      let kind: 'query' | 'mutation' = 'query';
      if (first.kind === 'name' && (first.value === 'query' || first.value === 'mutation')) {
        this.pos++;
        kind = first.value as 'query' | 'mutation';
      }
      let name: string | undefined;
      if (this.peek().kind === 'name') name = this.next().value;
      const vars = this.parseVariableDefinitions();
      this.skipDirectives();
      let selections: Selection[] = [];
      if (this.eat('{')) selections = this.parseSelectionList();
      else if (this.peek().kind === 'name') selections = this.parseSelectionList();
      ops.push({ kind, name, vars, selections });
    }
    return ops;
  }

  parseVariableDefinitions(): VariableDef[] {
    const defs: VariableDef[] = [];
    if (!this.eat('(')) return defs;
    while (this.peek().kind !== 'eof' && !this.eat(')')) {
      if (!this.eat('$')) { this.next(); continue; }
      const name = this.expectName();
      this.expect(':');
      let type = '';
      for (;;) {
        const t = this.peek();
        if (!(t.kind === 'name' || (t.kind === 'punct' && (t.value === '[' || t.value === ']' || t.value === '!')))) break;
        type += this.next().value;
      }
      let defaultValue: ValueNode | undefined;
      if (this.eat('=')) defaultValue = this.parseValue();
      defs.push({ name, type, defaultValue });
    }
    return defs;
  }

  skipDirectives(): void {
    while (this.eat('@')) {
      this.expectName();
      if (this.eat('(')) {
        while (this.peek().kind !== 'eof' && !this.eat(')')) {
          this.expectName();
          this.expect(':');
          this.parseValue();
        }
      }
    }
  }

  parseArguments(): ArgNode[] {
    const args: ArgNode[] = [];
    if (!this.eat('(')) return args;
    while (this.peek().kind !== 'eof' && !this.eat(')')) {
      const name = this.expectName();
      this.expect(':');
      args.push({ name, value: this.parseValue() });
    }
    return args;
  }

  parseSelectionList(): Selection[] {
    const list: Selection[] = [];
    while (this.peek().kind !== 'eof') {
      const t = this.peek();
      if (t.kind === 'punct' && t.value === '}') { this.pos++; break; }
      list.push(this.parseSelection());
    }
    return list;
  }

  parseSelection(): Selection {
    if (this.eat('...')) {
      this.skipDirectives();
      if (this.skipName('on')) {
        const typeCondition = this.expectName();
        this.skipDirectives();
        let selections: Selection[] = [];
        if (this.eat('{')) selections = this.parseSelectionList();
        return { kind: 'InlineFragment', typeCondition, selections };
      }
      const name = this.expectName();
      this.skipDirectives();
      return { kind: 'FragmentSpread', name };
    }
    let name = this.expectName();
    let alias: string | undefined;
    if (this.eat(':')) {
      alias = name;
      name = this.expectName();
    }
    const args = this.parseArguments();
    this.skipDirectives();
    let selections: Selection[] = [];
    if (this.eat('{')) selections = this.parseSelectionList();
    return { kind: 'Field', alias, name, args, selections };
  }

  parseValue(): ValueNode {
    const t = this.peek();
    if (t.kind === 'punct' && t.value === '$') {
      this.pos++;
      return { kind: 'variable', name: this.expectName() };
    }
    if (t.kind === 'string') {
      this.pos++;
      return { kind: 'literal', value: t.value };
    }
    if (t.kind === 'number') {
      this.pos++;
      return { kind: 'literal', value: Number(t.value) };
    }
    if (t.kind === 'name') {
      this.pos++;
      if (t.value === 'true') return { kind: 'literal', value: true };
      if (t.value === 'false') return { kind: 'literal', value: false };
      if (t.value === 'null') return { kind: 'literal', value: null };
      return { kind: 'enum', value: t.value };
    }
    if (t.kind === 'punct' && t.value === '[') {
      this.pos++;
      const values: ValueNode[] = [];
      while (this.peek().kind !== 'eof') {
        if (this.peek().kind === 'punct' && this.peek().value === ']') { this.pos++; break; }
        values.push(this.parseValue());
      }
      return { kind: 'list', values };
    }
    if (t.kind === 'punct' && t.value === '{') {
      this.pos++;
      const fields: Record<string, ValueNode> = {};
      while (this.peek().kind !== 'eof') {
        const cur = this.peek();
        if (cur.kind === 'punct' && cur.value === '}') { this.pos++; break; }
        const key = this.expectName();
        this.expect(':');
        fields[key] = this.parseValue();
      }
      return { kind: 'object', fields };
    }
    throw new Error(`Unexpected token "${t.value}" in GraphQL document`);
  }
}

interface Obj { __t: string; [key: string]: any }
function typed(t: string, props: Record<string, any> = {}): Obj { return { __t: t, ...props }; }

const TYPE_FIELDS: Record<string, string[]> = {
  Query: ['viewer', 'repository', 'node', '__type'],
  Mutation: ['createIssue', 'closeIssue', 'reopenIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  User: ['login'],
  Ref: ['name'],
  Repository: [
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef',
    'viewerPermission', 'hasIssuesEnabled', 'isPrivate', 'description', 'createdAt',
    'issue', 'issues', 'pullRequests', 'pullRequest', 'url',
  ],
  IssueConnection: ['nodes', 'totalCount', 'pageInfo'],
  PageInfo: ['hasNextPage', 'endCursor'],
  Issue: [
    'id', 'number', 'title', 'body', 'state', 'createdAt', 'updatedAt', 'closedAt',
    'author', 'labels', 'comments',
  ],
  IssueLabelConnection: ['nodes'],
  IssueLabel: ['name'],
  IssueCommentConnection: ['nodes'],
  IssueComment: ['id', 'body', 'createdAt', 'author'],
  IssueCommentEdge: ['node'],
  PullRequestConnection: ['nodes', 'totalCount', 'pageInfo'],
  PullRequest: [
    'id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName',
    'baseRefName', 'mergedAt', 'createdAt', 'files',
  ],
  PullRequestFileConnection: ['nodes'],
  PullRequestFile: ['path', 'additions', 'deletions', 'changeType'],
  CreateIssuePayload: ['issue'],
  CloseIssuePayload: ['issue'],
  ReopenIssuePayload: ['issue'],
  CreatePullRequestPayload: ['pullRequest'],
  MergePullRequestPayload: ['pullRequest'],
  AddCommentPayload: ['commentEdge'],
  __Type: ['name', 'fields', 'enumValues'],
  __Field: ['name'],
  __EnumValue: ['name'],
};

const b64 = (text: string): string => Buffer.from(text, 'utf8').toString('base64');
const gid = (type: string, ...parts: (string | number)[]): string => b64(`${type}:${parts.join(':')}`);

function decodeGid(id: any): { type: string; parts: string[] } | null {
  if (typeof id !== 'string' || id.length === 0) return null;
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const parts = text.split(':');
    if (!parts[0] || !/^[A-Za-z]/.test(parts[0])) return null;
    return { type: parts[0], parts: parts.slice(1) };
  } catch {
    return null;
  }
}

function repoById(store: Store, raw: any): Repo | undefined {
  const id = Number(raw);
  if (!Number.isFinite(id)) return undefined;
  for (const repo of store.repos.values()) if (repo.id === id) return repo;
  return undefined;
}

function issueByGid(store: Store, id: any): { repo: Repo; issue: Issue } | null {
  const d = decodeGid(id);
  if (!d || d.type !== 'Issue' || d.parts.length < 2) return null;
  const repo = repoById(store, d.parts[0]);
  if (!repo) return null;
  const issue = repo.findIssue(Number(d.parts[1]));
  return issue ? { repo, issue } : null;
}

function pullByGid(store: Store, id: any): { repo: Repo; pull: Pull } | null {
  const d = decodeGid(id);
  if (!d || d.type !== 'PullRequest' || d.parts.length < 2) return null;
  const repo = repoById(store, d.parts[0]);
  if (!repo) return null;
  const pull = repo.findPull(Number(d.parts[1]));
  return pull ? { repo, pull } : null;
}

function commentByGid(store: Store, id: any): Comment | null {
  const d = decodeGid(id);
  if (!d || d.type !== 'IssueComment' || d.parts.length < 1) return null;
  const commentId = Number(d.parts[0]);
  for (const repo of store.repos.values()) {
    for (const issue of repo.issues) {
      const comment = issue.comments.find((c) => c.id === commentId);
      if (comment) return comment;
    }
  }
  return null;
}

function firstLimit(value: any): number {
  if (value === undefined || value === null) return 100;
  const n = Number(value);
  if (!Number.isFinite(n) || n < 0) return 100;
  return Math.min(100, Math.floor(n));
}

function cursorOffset(after: any): number {
  if (typeof after !== 'string' || after.length === 0) return 0;
  try {
    const text = Buffer.from(after, 'base64').toString('utf8');
    const match = /cursor:(\d+)/.exec(text);
    return match ? Number(match[1]) : 0;
  } catch {
    return 0;
  }
}

function pageInfo(end: number, total: number, count: number): Obj {
  return typed('PageInfo', {
    hasNextPage: end < total,
    endCursor: count > 0 ? b64(`cursor:${end}`) : null,
  });
}

const repoValue = (repo: Repo): Obj => typed('Repository', { repo });
const issueValue = (repo: Repo, issue: Issue): Obj => typed('Issue', { repo, issue });
const pullValue = (repo: Repo, pull: Pull): Obj => typed('PullRequest', { repo, pull });
const commentValue = (comment: Comment): Obj => typed('IssueComment', { comment });

function nodeValue(store: Store, id: any): Obj | null {
  const d = decodeGid(id);
  if (!d) return null;
  if (d.type === 'Repository') {
    const repo = repoById(store, d.parts[0]);
    return repo ? repoValue(repo) : null;
  }
  if (d.type === 'Issue') {
    const loc = issueByGid(store, id);
    return loc ? issueValue(loc.repo, loc.issue) : null;
  }
  if (d.type === 'PullRequest') {
    const loc = pullByGid(store, id);
    return loc ? pullValue(loc.repo, loc.pull) : null;
  }
  if (d.type === 'IssueComment') {
    const comment = commentByGid(store, id);
    return comment ? commentValue(comment) : null;
  }
  return null;
}

function typeValue(name: any): Obj | null {
  if (name === 'Issue' || name === 'PullRequest' || name === 'Repository') {
    return typed('__Type', { name, fieldsList: TYPE_FIELDS[name] ?? [] });
  }
  if (name === 'SearchType') {
    return typed('__Type', { name, fieldsList: [], enumList: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'] });
  }
  return null;
}

function resolveLabels(repo: Repo, ids: any): Label[] {
  if (!Array.isArray(ids)) return [];
  const out: Label[] = [];
  for (const raw of ids) {
    const text = String(raw);
    const d = decodeGid(text);
    const key = d && d.parts.length > 0 ? d.parts[d.parts.length - 1] : text;
    const label = repo.labels.get(key) || Array.from(repo.labels.values()).find((l) => String(l.id) === key);
    if (label && !out.includes(label)) out.push(label);
  }
  return out;
}

function createIssueMutation(store: Store, input: any): Obj {
  const d = decodeGid(input.repositoryId);
  if (!d || d.type !== 'Repository') throw new Error('Repository not found');
  const repo = repoById(store, d.parts[0]);
  if (!repo) throw new Error('Repository not found');
  if (typeof input.title !== 'string' || input.title.length === 0) throw new Error('Title is required');
  const ts = store.nowIso();
  const issue: Issue = {
    id: store.nextIssueId++,
    number: ++repo.issueSeq,
    title: input.title,
    body: typeof input.body === 'string' ? input.body : '',
    state: 'open',
    stateReason: null,
    labels: resolveLabels(repo, input.labelIds),
    createdAt: ts,
    updatedAt: ts,
    closedAt: null,
    comments: [],
    version: 0,
  };
  repo.issues.push(issue);
  issue.version++;
  return typed('CreateIssuePayload', { issue: issueValue(repo, issue) });
}

function changeIssueStateMutation(store: Store, id: any, state: 'open' | 'closed'): Obj {
  const loc = issueByGid(store, id);
  if (!loc) throw new Error('Issue not found');
  const ts = store.nowIso();
  loc.issue.state = state;
  loc.issue.closedAt = state === 'closed' ? ts : null;
  loc.issue.updatedAt = ts;
  return typed(state === 'closed' ? 'CloseIssuePayload' : 'ReopenIssuePayload', {
    issue: issueValue(loc.repo, loc.issue),
  });
}

function createPullMutation(store: Store, input: any): Obj {
  const d = decodeGid(input.repositoryId);
  if (!d || d.type !== 'Repository') throw new Error('Repository not found');
  const repo = repoById(store, d.parts[0]);
  if (!repo) throw new Error('Repository not found');
  const headRaw = typeof input.headRefName === 'string' ? input.headRefName : '';
  const baseRef = typeof input.baseRefName === 'string' ? input.baseRefName : '';
  let headOwner = repo.owner;
  let headRef = headRaw;
  const colon = headRaw.indexOf(':');
  if (colon >= 0) {
    headOwner = headRaw.slice(0, colon);
    headRef = headRaw.slice(colon + 1);
  }
  const headSha = repo.refs.get(`refs/heads/${headRef}`);
  const baseSha = repo.refs.get(`refs/heads/${baseRef}`);
  if (!headSha || !baseSha) throw new Error('Head or base ref does not exist');
  const existing = repo.pulls.find((p) => p.state === 'open' && p.headRef === headRef && p.baseRef === baseRef);
  if (existing) return typed('CreatePullRequestPayload', { pullRequest: pullValue(repo, existing) });
  const ts = store.nowIso();
  const pull: Pull = {
    id: store.nextPullId++,
    number: ++repo.pullSeq,
    title: typeof input.title === 'string' ? input.title : '',
    body: typeof input.body === 'string' ? input.body : '',
    state: 'open',
    headOwner,
    headRef,
    headSha,
    baseOwner: repo.owner,
    baseRef,
    baseSha,
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    draft: typeof input.draft === 'boolean' ? input.draft : false,
    createdAt: ts,
    updatedAt: ts,
    closedAt: null,
  };
  repo.pulls.push(pull);
  return typed('CreatePullRequestPayload', { pullRequest: pullValue(repo, pull) });
}

function mergePullMutation(store: Store, input: any): Obj {
  const loc = pullByGid(store, input.pullRequestId);
  if (!loc) throw new Error('Pull request not found');
  if (loc.pull.merged) return typed('MergePullRequestPayload', { pullRequest: pullValue(loc.repo, loc.pull) });
  const method = typeof input.mergeMethod === 'string' ? input.mergeMethod.toUpperCase() : 'SQUASH';
  const headline = typeof input.commitHeadline === 'string' && input.commitHeadline ? input.commitHeadline : undefined;
  const result = mergePull(store, loc.repo, loc.pull, method, headline);
  if (!result.ok) throw new Error('Pull request is not mergeable');
  return typed('MergePullRequestPayload', { pullRequest: pullValue(loc.repo, loc.pull) });
}

function addCommentMutation(store: Store, input: any): Obj {
  const loc = issueByGid(store, input.subjectId);
  if (!loc) throw new Error('Subject not found');
  const ts = store.nowIso();
  const comment: Comment = {
    id: store.nextCommentId++,
    body: typeof input.body === 'string' ? input.body : '',
    createdAt: ts,
    updatedAt: ts,
  };
  loc.issue.comments.push(comment);
  loc.issue.updatedAt = ts;
  return typed('AddCommentPayload', {
    commentEdge: typed('IssueCommentEdge', { node: commentValue(comment) }),
  });
}

interface Ctx {
  store: Store;
  variables: Record<string, any>;
  operation: Operation;
  errors: string[];
}

function coerce(value: ValueNode, ctx: Ctx): any {
  if (value.kind === 'variable') {
    if (ctx.variables && Object.prototype.hasOwnProperty.call(ctx.variables, value.name)) {
      return ctx.variables[value.name];
    }
    const def = ctx.operation.vars.find((v) => v.name === value.name);
    return def && def.defaultValue ? coerce(def.defaultValue, ctx) : undefined;
  }
  if (value.kind === 'list') return value.values.map((v) => coerce(v, ctx));
  if (value.kind === 'object') {
    const out: Record<string, any> = {};
    for (const key of Object.keys(value.fields)) out[key] = coerce(value.fields[key], ctx);
    return out;
  }
  return value.value;
}

function stateList(raw: any): string[] {
  if (Array.isArray(raw)) return raw.map((s) => String(s).toUpperCase());
  if (raw !== undefined && raw !== null) return [String(raw).toUpperCase()];
  return [];
}

function resolveField(t: string, obj: Obj, name: string, args: Record<string, any>, ctx: Ctx): any {
  if (t === 'Query') {
    if (name === 'viewer') return typed('User', { login: ctx.store.login });
    if (name === 'repository') {
      const repo = ctx.store.getRepo(String(args.owner ?? ''), String(args.name ?? ''));
      return repo ? repoValue(repo) : null;
    }
    if (name === 'node') return nodeValue(ctx.store, args.id);
    if (name === '__type') return typeValue(args.name);
  }
  if (t === 'Mutation') {
    if (name === 'createIssue') return createIssueMutation(ctx.store, args.input ?? {});
    if (name === 'closeIssue') return changeIssueStateMutation(ctx.store, args.input?.issueId, 'closed');
    if (name === 'reopenIssue') return changeIssueStateMutation(ctx.store, args.input?.issueId, 'open');
    if (name === 'createPullRequest') return createPullMutation(ctx.store, args.input ?? {});
    if (name === 'mergePullRequest') return mergePullMutation(ctx.store, args.input ?? {});
    if (name === 'addComment') return addCommentMutation(ctx.store, args.input ?? {});
  }
  if (t === 'Repository') {
    const repo: Repo = obj.repo;
    if (name === 'id') return gid('Repository', repo.id);
    if (name === 'databaseId') return repo.id;
    if (name === 'name') return repo.name;
    if (name === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
    if (name === 'owner') return typed('User', { login: repo.owner });
    if (name === 'defaultBranchRef') {
      const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      return tip ? typed('Ref', { name: repo.defaultBranch }) : null;
    }
    if (name === 'viewerPermission') return 'ADMIN';
    if (name === 'hasIssuesEnabled') return true;
    if (name === 'isPrivate') return repo.private;
    if (name === 'description') return repo.description;
    if (name === 'createdAt') return repo.createdAt;
    if (name === 'url') return `https://github.com/${repo.owner}/${repo.name}`;
    if (name === 'issue') {
      const issue = repo.findIssue(Number(args.number));
      return issue ? issueValue(repo, issue) : null;
    }
    if (name === 'pullRequest') {
      const pull = repo.findPull(Number(args.number));
      return pull ? pullValue(repo, pull) : null;
    }
    if (name === 'issues') {
      let list = repo.issues.slice();
      const states = stateList(args.states);
      if (states.length > 0) list = list.filter((i) => states.includes(i.state.toUpperCase()));
      const direction = args.orderBy && typeof args.orderBy === 'object' && args.orderBy.direction
        ? String(args.orderBy.direction).toUpperCase()
        : 'DESC';
      list.sort((a, z) => (direction === 'ASC' ? a.number - z.number : z.number - a.number));
      const totalCount = list.length;
      const offset = cursorOffset(args.after);
      const first = firstLimit(args.first);
      const nodes = list.slice(offset, offset + first);
      return typed('IssueConnection', {
        nodes: nodes.map((i) => issueValue(repo, i)),
        totalCount,
        pageInfo: pageInfo(offset + nodes.length, totalCount, nodes.length),
      });
    }
    if (name === 'pullRequests') {
      let list = repo.pulls.slice();
      const states = stateList(args.states);
      if (states.length > 0) list = list.filter((p) => states.includes(p.state.toUpperCase()));
      list.sort((a, z) => z.number - a.number);
      const totalCount = list.length;
      const offset = cursorOffset(args.after);
      const first = firstLimit(args.first);
      const nodes = list.slice(offset, offset + first);
      return typed('PullRequestConnection', {
        nodes: nodes.map((p) => pullValue(repo, p)),
        totalCount,
        pageInfo: pageInfo(offset + nodes.length, totalCount, nodes.length),
      });
    }
  }
  if (t === 'Issue') {
    const repo: Repo = obj.repo;
    const issue: Issue = obj.issue;
    if (name === 'id') return gid('Issue', repo.id, issue.number);
    if (name === 'number') return issue.number;
    if (name === 'title') return issue.title;
    if (name === 'body') return issue.body;
    if (name === 'state') return issue.state.toUpperCase();
    if (name === 'createdAt') return issue.createdAt;
    if (name === 'updatedAt') return issue.updatedAt;
    if (name === 'closedAt') return issue.closedAt;
    if (name === 'author') return typed('User', { login: ctx.store.login });
    if (name === 'labels') {
      return typed('IssueLabelConnection', {
        nodes: issue.labels.slice(0, firstLimit(args.first)).map((l) => typed('IssueLabel', { name: l.name })),
      });
    }
    if (name === 'comments') {
      return typed('IssueCommentConnection', {
        nodes: issue.comments.slice(0, firstLimit(args.first)).map((c) => commentValue(c)),
      });
    }
  }
  if (t === 'PullRequest') {
    const repo: Repo = obj.repo;
    const pull: Pull = obj.pull;
    if (name === 'id') return gid('PullRequest', repo.id, pull.number);
    if (name === 'number') return pull.number;
    if (name === 'title') return pull.title;
    if (name === 'body') return pull.body;
    if (name === 'state') return pull.state.toUpperCase();
    if (name === 'merged') return pull.merged;
    if (name === 'mergeable') return isPullMergeable(repo, pull) ? 'MERGEABLE' : 'CONFLICTING';
    if (name === 'headRefName') return pull.headRef;
    if (name === 'baseRefName') return pull.baseRef;
    if (name === 'mergedAt') return pull.mergedAt;
    if (name === 'createdAt') return pull.createdAt;
    if (name === 'files') {
      return typed('PullRequestFileConnection', {
        nodes: pullChangedFiles(repo, pull).slice(0, firstLimit(args.first)).map((f) => typed('PullRequestFile', {
          path: f.filename,
          additions: f.additions,
          deletions: f.deletions,
          changeType: f.status.toUpperCase(),
        })),
      });
    }
  }
  if (t === 'IssueComment') {
    const comment: Comment = obj.comment;
    if (name === 'id') return gid('IssueComment', comment.id);
    if (name === 'body') return comment.body;
    if (name === 'createdAt') return comment.createdAt;
    if (name === 'author') return typed('User', { login: ctx.store.login });
  }
  if (t === '__Type') {
    if (name === 'name') return obj.name;
    if (name === 'fields') {
      return (obj.fieldsList ?? []).map((field: string) => typed('__Field', { name: field }));
    }
    if (name === 'enumValues') {
      return (obj.enumList ?? []).map((value: string) => typed('__EnumValue', { name: value }));
    }
  }
  if (name in obj) return obj[name];
  return null;
}

function executeSelectionSet(parent: Obj | null, selections: Selection[], ctx: Ctx): Record<string, any> {
  if (parent === null || parent === undefined) return null as any;
  const typeName = parent.__t;
  const out: Record<string, any> = {};
  for (const selection of selections) {
    if (selection.kind === 'FragmentSpread') continue;
    if (selection.kind === 'InlineFragment') {
      const condition = selection.typeCondition;
      const matches = condition === typeName || (condition === 'Node' && ['Repository', 'Issue', 'PullRequest', 'IssueComment'].includes(typeName));
      if (matches) {
        const part = executeSelectionSet(parent, selection.selections, ctx);
        if (part && typeof part === 'object') Object.assign(out, part);
      }
      continue;
    }
    const fieldName = selection.name;
    const allowed = TYPE_FIELDS[typeName];
    if (!allowed || !allowed.includes(fieldName)) {
      ctx.errors.push(`Cannot query field "${fieldName}" on type "${typeName}".`);
      continue;
    }
    const args: Record<string, any> = {};
    for (const arg of selection.args) args[arg.name] = coerce(arg.value, ctx);
    let value: any = null;
    try {
      value = resolveField(typeName, parent, fieldName, args, ctx);
    } catch (err) {
      ctx.errors.push(err instanceof Error ? err.message : String(err));
    }
    const key = selection.alias ?? fieldName;
    if (selection.selections.length > 0 && value !== null && value !== undefined) {
      if (Array.isArray(value)) out[key] = value.map((item) => executeSelectionSet(item, selection.selections, ctx));
      else out[key] = executeSelectionSet(value, selection.selections, ctx);
    } else {
      out[key] = value === undefined ? null : value;
    }
  }
  return out;
}

function runOperation(operation: Operation, store: Store, variables: Record<string, any>): Record<string, any> {
  const ctx: Ctx = { store, variables: variables ?? {}, operation, errors: [] };
  const rootType = operation.kind === 'mutation' ? 'Mutation' : 'Query';
  const data = executeSelectionSet(typed(rootType), operation.selections, ctx);
  const result: Record<string, any> = { data };
  const errors = Array.from(new Set(ctx.errors));
  if (errors.length > 0) result.errors = errors.map((message) => ({ message }));
  return result;
}

function handleOne(payload: any, store: Store): Record<string, any> {
  try {
    const source = typeof payload?.query === 'string' ? payload.query : '';
    const operations = new Parser(tokenize(source)).parseDocument();
    if (operations.length === 0) return { data: null, errors: [{ message: 'No operation found in GraphQL document' }] };
    let operation = operations[0];
    if (typeof payload.operationName === 'string') {
      const selected = operations.find((op) => op.name === payload.operationName);
      if (!selected) return { data: null, errors: [{ message: `Unknown operation "${payload.operationName}"` }] };
      operation = selected;
    }
    return runOperation(operation, store, payload.variables ?? {});
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : String(err) }] };
  }
}

export function handleGraphql(payload: unknown, store: Store): unknown {
  if (Array.isArray(payload)) return payload.map((item) => handleOne(item, store));
  return handleOne(payload, store);
}
