import { randomUUID } from 'node:crypto';
import type { ServerResponse } from 'node:http';
import { mergePull } from './merge.ts';
import { issueJson, pullJson, repoToJson } from './models.ts';
import { type Issue, type Pull, Repo, type Store } from './store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const SERVER_INFO = { name: 'github-core-clone-01', version: '1.0.0' };

const DISCOVER_RESULT = {
  protocolVersion: '2026-07-28',
  supportedVersions: ['2026-07-28', '2025-11-25'],
  serverInfo: SERVER_INFO,
  capabilities: { tools: { listChanged: false } },
  instructions: 'A GitHub-compatible forge.',
  _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
};

type PropType = 'string' | 'boolean' | 'integer';

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: PropType }>;
    required: string[];
  };
}

const TOOL_DEFS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated login.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string' },
        description: { type: 'string' },
        private: { type: 'boolean' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
        title: { type: 'string' },
        body: { type: 'string' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
        state: { type: 'string' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
        title: { type: 'string' },
        head: { type: 'string' },
        base: { type: 'string' },
        body: { type: 'string' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request into its base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
        pull_number: { type: 'integer' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

const TOOLS_RESULT = { tools: TOOL_DEFS };

interface TextContent { type: 'text'; text: string }

interface ToolResult {
  content: TextContent[];
  structuredContent?: Record<string, unknown>;
  isError: boolean;
}

function toolSuccess(structured: Record<string, unknown>): ToolResult {
  return {
    content: [{ type: 'text', text: JSON.stringify(structured) }],
    structuredContent: structured,
    isError: false,
  };
}

function toolError(message: string): ToolResult {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function validateArgs(def: ToolDef, args: Record<string, unknown>): string | null {
  for (const key of def.inputSchema.required) {
    const value = args[key];
    if (value === undefined || value === null || value === '') {
      return `Missing required argument: ${key}`;
    }
  }
  for (const [key, value] of Object.entries(args)) {
    const prop = def.inputSchema.properties[key];
    if (!prop || value === undefined || value === null) continue;
    if (prop.type === 'integer') {
      if (typeof value !== 'number' || !Number.isInteger(value)) return `Invalid argument: ${key}`;
    } else if (typeof value !== prop.type) {
      return `Invalid argument: ${key}`;
    }
  }
  return null;
}

function getRepo(store: Store, owner: string, name: string): Repo | undefined {
  return store.getRepo(owner, name);
}

function callTool(def: ToolDef, args: Record<string, unknown>, store: Store): ToolResult {
  const invalid = validateArgs(def, args);
  if (invalid) return toolError(invalid);

  const owner = typeof args.owner === 'string' ? args.owner : '';
  const repoName = typeof args.repo === 'string' ? args.repo : '';

  switch (def.name) {
    case 'create_repo': {
      const name = args.name as string;
      if (store.repos.has(store.repoKey(store.login, name))) {
        return toolError('Repository creation failed: name already exists');
      }
      const description = typeof args.description === 'string' ? args.description : '';
      const isPrivate = typeof args.private === 'boolean' ? args.private : false;
      const repo = new Repo(store.login, name, store.nextRepoId++, description, isPrivate, store.nowIso());
      store.repos.set(store.repoKey(repo.owner, repo.name), repo);
      return toolSuccess(repoToJson(repo));
    }
    case 'get_repo': {
      const repo = getRepo(store, owner, repoName);
      if (!repo) return toolError(`Not Found: ${owner}/${repoName}`);
      return toolSuccess(repoToJson(repo));
    }
    case 'create_issue': {
      const repo = getRepo(store, owner, repoName);
      if (!repo) return toolError(`Not Found: ${owner}/${repoName}`);
      const ts = store.nowIso();
      const issue: Issue = {
        id: store.nextIssueId++,
        number: ++repo.issueSeq,
        title: args.title as string,
        body: typeof args.body === 'string' ? args.body : '',
        state: 'open',
        stateReason: null,
        labels: [],
        createdAt: ts,
        updatedAt: ts,
        closedAt: null,
        comments: [],
        version: 0,
      };
      repo.issues.push(issue);
      issue.version++;
      return toolSuccess(issueJson(repo, store, issue));
    }
    case 'list_issues': {
      const repo = getRepo(store, owner, repoName);
      if (!repo) return toolError(`Not Found: ${owner}/${repoName}`);
      const state = typeof args.state === 'string' ? args.state : 'all';
      const list = repo.issues.filter((i) => state === 'all' || i.state === state);
      return toolSuccess({ issues: list.map((i) => issueJson(repo, store, i)) });
    }
    case 'create_pull_request': {
      const repo = getRepo(store, owner, repoName);
      if (!repo) return toolError(`Not Found: ${owner}/${repoName}`);
      const headRaw = args.head as string;
      const baseRef = args.base as string;
      let headOwner = repo.owner;
      let headRef = headRaw;
      const colon = headRaw.indexOf(':');
      if (colon >= 0) {
        headOwner = headRaw.slice(0, colon);
        headRef = headRaw.slice(colon + 1);
      }
      const headSha = repo.refs.get(`refs/heads/${headRef}`);
      const baseSha = repo.refs.get(`refs/heads/${baseRef}`);
      if (!headSha || !baseSha) return toolError('Validation Failed');
      const existing = repo.pulls.find((p) => p.state === 'open' && p.headRef === headRef && p.baseRef === baseRef);
      if (existing) return toolSuccess(pullJson(repo, store, existing));
      const ts = store.nowIso();
      const pull: Pull = {
        id: store.nextPullId++,
        number: ++repo.pullSeq,
        title: args.title as string,
        body: typeof args.body === 'string' ? args.body : '',
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
        draft: false,
        createdAt: ts,
        updatedAt: ts,
        closedAt: null,
      };
      repo.pulls.push(pull);
      return toolSuccess(pullJson(repo, store, pull));
    }
    case 'merge_pull_request': {
      const repo = getRepo(store, owner, repoName);
      if (!repo) return toolError(`Not Found: ${owner}/${repoName}`);
      const pullNumber = args.pull_number as number;
      const pull = repo.findPull(pullNumber);
      if (!pull) return toolError(`Pull request not found: ${owner}/${repoName}#${pullNumber}`);
      const result = mergePull(store, repo, pull);
      if (!result.ok) return toolError(result.message);
      return toolSuccess({
        merged: true,
        number: pull.number,
        sha: result.sha,
        message: 'Pull Request successfully merged',
      });
    }
    default:
      return toolError(`Unknown tool: ${def.name}`);
  }
}

interface Prepared {
  status: number;
  body: string | null;
}

function rpcResult(id: unknown, result: unknown): Prepared {
  return { status: 200, body: JSON.stringify({ jsonrpc: '2.0', id, result }) };
}

function rpcError(id: unknown, code: number, message: string): Prepared {
  return { status: 200, body: JSON.stringify({ jsonrpc: '2.0', id, error: { code, message } }) };
}

function parseError(): Prepared {
  return { status: 400, body: JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } }) };
}

function asObject(value: unknown): Record<string, unknown> | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  return value as Record<string, unknown>;
}

function dispatch(request: Record<string, unknown>, store: Store): Prepared {
  if (!('id' in request) || request.id === null) return { status: 202, body: null };
  const id = request.id;
  const params = asObject(request.params) ?? {};

  switch (request.method) {
    case 'server/discover':
      return rpcResult(id, DISCOVER_RESULT);
    case 'initialize':
      return rpcResult(id, {
        protocolVersion: typeof params.protocolVersion === 'string' ? params.protocolVersion : '2025-11-25',
        capabilities: { tools: { listChanged: false } },
        serverInfo: SERVER_INFO,
      });
    case 'tools/list':
      return rpcResult(id, TOOLS_RESULT);
    case 'tools/call': {
      const name = typeof params.name === 'string' ? params.name : '';
      const def = TOOL_DEFS.find((t) => t.name === name);
      if (!def) return rpcError(id, -32602, `Unknown tool: ${name}`);
      const args = asObject(params.arguments) ?? {};
      return rpcResult(id, callTool(def, args, store));
    }
    default:
      return rpcError(id, -32601, `Method not found: ${String(request.method)}`);
  }
}

function send(res: ServerResponse, status: number, body: string | null): void {
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'x-github-request-id': randomUUID(),
  });
  if (body === null) res.end(); else res.end(body);
}

export function handleMcp(res: ServerResponse, method: string, body: Buffer | null, store: Store): void {
  if (method === 'DELETE') {
    send(res, 204, null);
    return;
  }
  if (method !== 'POST') {
    send(res, 405, JSON.stringify({ message: 'Method Not Allowed' }));
    return;
  }
  if (body === null) {
    const error = parseError();
    send(res, error.status, error.body);
    return;
  }

  let payload: unknown;
  try {
    payload = JSON.parse(body.toString('utf8'));
  } catch {
    const error = parseError();
    send(res, error.status, error.body);
    return;
  }

  if (Array.isArray(payload)) {
    const responses: unknown[] = [];
    for (const item of payload) {
      const request = asObject(item);
      if (!request) {
        responses.push({ jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } });
        continue;
      }
      const prepared = dispatch(request, store);
      if (prepared.body !== null) responses.push(JSON.parse(prepared.body));
    }
    send(res, 200, JSON.stringify(responses));
    return;
  }

  const request = asObject(payload);
  if (!request) {
    send(res, 400, JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } }));
    return;
  }
  const prepared = dispatch(request, store);
  send(res, prepared.status, prepared.body);
}
