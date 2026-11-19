import { Buffer } from 'node:buffer';
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import {
  buildCommit,
  getObject,
  listDir,
  nextRestTimestamp,
  parseCommit,
  parseIdentityLine,
  putObject,
  readFile,
  writeFile,
} from './git.ts';
import { type Comment, type Issue, type Label, type Pull, Repo, Store } from './store.ts';
import { handleGitHttp } from './git-http.ts';
import { mergePull, pullChangedFiles } from './merge.ts';
import { handleGraphql } from './graphql.ts';
import { cachedIssueJson, commentJson, issueJson, labelJson, pullJson, repoToJson } from './models.ts';
import { handleMcp } from './mcp.ts';

const JSON_TYPE = 'application/json; charset=utf-8';

type Json = any;

let requestSeq = 0;

function reqId(): string {
  return 'r' + (++requestSeq).toString(36) + '-' + Date.now().toString(36);
}

function sendRaw(res: ServerResponse, status: number, body: Buffer | null, headers: Record<string, string>): void {
  const h: Record<string, string> = { ...headers };
  if (!h['x-github-request-id']) h['x-github-request-id'] = reqId();
  if (body !== null) {
    h['content-length'] = String(body.length);
    if (!h['content-type']) h['content-type'] = JSON_TYPE;
  }
  res.writeHead(status, h);
  if (body !== null) res.end(body); else res.end();
}

function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  if (status === 204) {
    sendRaw(res, 204, null, { 'x-github-request-id': reqId(), ...headers });
    return;
  }
  const text = JSON.stringify(body);
  sendRaw(res, status, Buffer.from(text), { 'content-type': JSON_TYPE, 'x-github-request-id': reqId(), ...headers });
}

function sendError(res: ServerResponse, status: number, message: string, headers: Record<string, string> = {}): void {
  sendJson(res, status, { message }, headers);
}

function rawPath(rawUrl: string): string {
  const q = rawUrl.indexOf('?');
  return q < 0 ? rawUrl : rawUrl.slice(0, q);
}

function decodeQueryValue(s: string): string {
  if (s.indexOf('%') < 0 && s.indexOf('+') < 0) return s;
  try { return decodeURIComponent(s.replace(/\+/g, ' ')); } catch { return s; }
}

function queryParam(query: string, name: string): string | null {
  let pos = 0;
  while (pos <= query.length) {
    const amp = query.indexOf('&', pos);
    const part = amp < 0 ? query.slice(pos) : query.slice(pos, amp);
    const eq = part.indexOf('=');
    if (decodeQueryValue(eq < 0 ? part : part.slice(0, eq)) === name) {
      return eq < 0 ? '' : decodeQueryValue(part.slice(eq + 1));
    }
    if (amp < 0) break;
    pos = amp + 1;
  }
  return null;
}

function pathSegments(pathname: string): string[] {
  const out: string[] = [];
  let start = 0;
  while (start < pathname.length) {
    let end = pathname.indexOf('/', start);
    if (end < 0) end = pathname.length;
    if (end > start) {
      const seg = pathname.slice(start, end);
      out.push(seg.indexOf('%') < 0 ? seg : decodeURIComponentSafe(seg));
    }
    start = end + 1;
  }
  return out;
}

function decodeURIComponentSafe(s: string): string {
  try { return decodeURIComponent(s); } catch { return s; }
}

function requestHasBody(req: IncomingMessage, method: string): boolean {
  if (method === 'GET' || method === 'HEAD') return false;
  const len = req.headers['content-length'];
  if (len !== undefined) return Number(len) > 0;
  return req.headers['transfer-encoding'] !== undefined;
}

async function readBody(req: IncomingMessage, max = 8 * 1024 * 1024): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let total = 0;
  return await new Promise<Buffer>((resolve, reject) => {
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > max) {
        reject(new Error('body too large'));
        req.destroy();
        return;
      }
      chunks.push(Buffer.from(chunk));
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function isAuthed(req: IncomingMessage): boolean {
  const h = req.headers.authorization;
  if (!h || !h.startsWith('Bearer ')) return false;
  const token = h.slice(7);
  if (!token) return false;
  const expected = process.env.MAKEFUN_SHOP_TOKEN;
  if (!expected) return true; // unset or empty => accept any non-empty bearer token
  return token === expected;
}

function getTip(repo: Repo, ref: string): string | undefined {
  return repo.refs.get(ref);
}

function resolveCommitRef(repo: Repo, shaParam?: string): string | undefined {
  if (!shaParam) {
    return getTip(repo, `refs/heads/${repo.defaultBranch}`);
  }
  if (shaParam.startsWith('refs/')) {
    return repo.refs.get(shaParam);
  }
  const asBranch = repo.refs.get(`refs/heads/${shaParam}`);
  if (asBranch) return asBranch;
  if (repo.objects.has(shaParam)) return shaParam;
  // try as tag ref? not specified
  return undefined;
}

function commitToJson(repo: Repo, sha: string) {
  const obj = getObject(repo, sha);
  if (!obj || obj.type !== 'commit') return null;
  const c = parseCommit(obj.data);
  const author = parseIdentityLine(c.authorLine);
  const committer = parseIdentityLine(c.committerLine);
  const msg = c.message.endsWith('\n') ? c.message.slice(0, -1) : c.message;
  return {
    sha,
    commit: {
      message: msg,
      author: { name: author.name, email: author.email, date: author.date },
      committer: { name: committer.name, email: committer.email, date: committer.date },
    },
    parents: c.parents.map((p) => ({ sha: p })),
  };
}

function walkCommitsFirstParent(repo: Repo, tip: string): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  let cur: string | undefined = tip;
  while (cur && !seen.has(cur)) {
    const obj = getObject(repo, cur);
    if (!obj || obj.type !== 'commit') break;
    seen.add(cur);
    out.push(cur);
    const c = parseCommit(obj.data);
    cur = c.parents[0];
  }
  return out;
}

function ensureDefaultBranchRef(repo: Repo): string | undefined {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`);
}

function getOrInitBranchForWrite(repo: Repo, branchName: string): { tip: string | undefined; unborn: boolean } {
  const refName = `refs/heads/${branchName}`;
  const tip = repo.refs.get(refName);
  if (tip) return { tip, unborn: false };
  const defaultTip = ensureDefaultBranchRef(repo);
  if (!defaultTip) {
    // repo has no commits at all; allow unborn only if branch is default
    if (branchName === repo.defaultBranch) return { tip: undefined, unborn: true };
    return { tip: undefined, unborn: true }; // will be handled by caller logic
  }
  // named branch does not exist but repo has commits: create branch at default tip
  repo.refs.set(refName, defaultTip);
  repo.rev++;
  return { tip: defaultTip, unborn: false };
}

function fixedIdentity(bodyCommitter?: any): { name: string; email: string; timestamp: number } {
  if (bodyCommitter && typeof bodyCommitter.name === 'string' && typeof bodyCommitter.email === 'string') {
    let ts = nextRestTimestamp();
    if (typeof bodyCommitter.date === 'string') {
      const n = Date.parse(bodyCommitter.date);
      if (!Number.isNaN(n)) ts = Math.floor(n / 1000);
    }
    return { name: bodyCommitter.name, email: bodyCommitter.email, timestamp: ts };
  }
  return { name: 'ada iversen', email: 'adaiversen@example.invalid', timestamp: nextRestTimestamp() };
}

function paginate<T>(items: T[], query: string): T[] {
  const perPageRaw = parseInt(queryParam(query, 'per_page') || '30', 10);
  const pageRaw = parseInt(queryParam(query, 'page') || '1', 10);
  const perPage = Number.isFinite(perPageRaw) ? Math.max(1, perPageRaw) : 30;
  const page = Number.isFinite(pageRaw) ? Math.max(1, pageRaw) : 1;
  return items.slice((page - 1) * perPage, (page - 1) * perPage + perPage);
}

function resolveLabels(repo: Repo, names: any): Label[] | null {
  if (names === undefined || names === null) return [];
  if (!Array.isArray(names)) return null;
  const resolved: Label[] = [];
  for (const name of names) {
    if (typeof name !== 'string') return null;
    const label = repo.labels.get(name);
    if (!label) return null;
    resolved.push(label);
  }
  return resolved;
}

function routeIssuesPulls(
  res: ServerResponse,
  method: string,
  rest: string[],
  query: string,
  body: any,
  repo: Repo,
  store: Store,
): boolean {
  const b = body || {};

  if (rest.length === 1 && rest[0] === 'labels') {
    if (method === 'GET') {
      sendJson(res, 200, Array.from(repo.labels.values()).map(labelJson));
      return true;
    }
    if (method === 'POST') {
      if (typeof b.name !== 'string' || !b.name) {
        sendError(res, 422, 'Validation Failed');
        return true;
      }
      const rawColor = typeof b.color === 'string' && b.color ? b.color : 'ededed';
      const label: Label = {
        id: store.nextLabelId++,
        name: b.name,
        color: rawColor.startsWith('#') ? rawColor.slice(1) : rawColor,
        description: null,
      };
      repo.labels.set(label.name, label);
      sendJson(res, 201, labelJson(label));
      return true;
    }
    return false;
  }

  if (rest.length === 2 && rest[0] === 'labels' && method === 'DELETE') {
    repo.labels.delete(rest[1]);
    sendJson(res, 204, null);
    return true;
  }

  if (rest.length === 1 && rest[0] === 'issues') {
    if (method === 'POST') {
      if (typeof b.title !== 'string' || !b.title) {
        sendError(res, 422, 'Validation Failed');
        return true;
      }
      const labels = resolveLabels(repo, b.labels);
      if (!labels) {
        sendError(res, 422, 'Validation Failed');
        return true;
      }
      const ts = store.nowIso();
      const issue: Issue = {
        id: store.nextIssueId++,
        number: ++repo.issueSeq,
        title: b.title,
        body: typeof b.body === 'string' ? b.body : '',
        state: 'open',
        stateReason: null,
        labels,
        createdAt: ts,
        updatedAt: ts,
        closedAt: null,
        comments: [],
        version: 0,
      };
      repo.issues.push(issue);
      issue.version++;
      sendJson(res, 201, issueJson(repo, store, issue));
      return true;
    }
    if (method === 'GET') {
      const state = queryParam(query, 'state') || 'open';
      const sort = queryParam(query, 'sort') || 'created';
      const direction = queryParam(query, 'direction') || 'desc';
      let list = repo.issues.filter((i) => state === 'all' || i.state === state);
      list = list.slice().sort((a, z) => {
        if (sort === 'updated') {
          if (a.updatedAt < z.updatedAt) return -1;
          if (a.updatedAt > z.updatedAt) return 1;
          return 0;
        }
        return a.number - z.number;
      });
      if (direction !== 'asc') list.reverse();
      sendJson(res, 200, paginate(list, query).map((i) => issueJson(repo, store, i)));
      return true;
    }
    return false;
  }

  if (rest.length >= 2 && rest[0] === 'issues') {
    const number = parseInt(rest[1], 10);
    const issue = Number.isFinite(number) ? repo.findIssue(number) : undefined;
    if (!issue) {
      sendError(res, 404, 'Not Found');
      return true;
    }
      if (rest.length === 2) {
        if (method === 'GET') {
          sendRaw(res, 200, cachedIssueJson(repo, store, issue), { 'content-type': JSON_TYPE });
          return true;
        }
      if (method === 'PATCH') {
        let labels: Label[] | null = null;
        if (b.labels !== undefined) {
          labels = resolveLabels(repo, b.labels);
          if (!labels) {
            sendError(res, 422, 'Validation Failed');
            return true;
          }
        }
        if (labels !== null) issue.labels = labels;
        if (typeof b.title === 'string') issue.title = b.title;
        if (typeof b.body === 'string') issue.body = b.body;
        if (b.state === 'closed') {
          issue.state = 'closed';
          issue.closedAt = store.nowIso();
        } else if (b.state === 'open') {
          issue.state = 'open';
          issue.closedAt = null;
        }
        issue.updatedAt = store.nowIso();
        issue.version++;
        sendJson(res, 200, issueJson(repo, store, issue));
        return true;
      }
      return false;
    }
    if (rest.length === 3 && rest[2] === 'comments') {
      if (method === 'GET') {
        sendJson(res, 200, issue.comments.map((c) => commentJson(store, c)));
        return true;
      }
      if (method === 'POST') {
        const ts = store.nowIso();
        const comment: Comment = {
          id: store.nextCommentId++,
          body: typeof b.body === 'string' ? b.body : '',
          createdAt: ts,
          updatedAt: ts,
        };
        issue.comments.push(comment);
        issue.updatedAt = ts;
        issue.version++;
        sendJson(res, 201, commentJson(store, comment));
        return true;
      }
      return false;
    }
    return false;
  }

  if (rest.length === 1 && rest[0] === 'pulls') {
    if (method === 'POST') {
      const headRaw = typeof b.head === 'string' ? b.head : '';
      const baseRef = typeof b.base === 'string' ? b.base : '';
      let headOwner = repo.owner;
      let headRef = headRaw;
      const colon = headRaw.indexOf(':');
      if (colon >= 0) {
        headOwner = headRaw.slice(0, colon);
        headRef = headRaw.slice(colon + 1);
      }
      const headSha = repo.refs.get(`refs/heads/${headRef}`);
      const baseSha = repo.refs.get(`refs/heads/${baseRef}`);
      if (!headSha || !baseSha) {
        sendError(res, 422, 'Validation Failed');
        return true;
      }
      const existing = repo.pulls.find((p) => p.state === 'open' && p.headRef === headRef && p.baseRef === baseRef);
      if (existing) {
        sendJson(res, 201, pullJson(repo, store, existing));
        return true;
      }
      const ts = store.nowIso();
      const pull: Pull = {
        id: store.nextPullId++,
        number: ++repo.pullSeq,
        title: typeof b.title === 'string' ? b.title : '',
        body: typeof b.body === 'string' ? b.body : '',
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
        draft: typeof b.draft === 'boolean' ? b.draft : false,
        createdAt: ts,
        updatedAt: ts,
        closedAt: null,
      };
      repo.pulls.push(pull);
      sendJson(res, 201, pullJson(repo, store, pull));
      return true;
    }
    if (method === 'GET') {
      const state = queryParam(query, 'state') || 'open';
      const list = repo.pulls.filter((p) => state === 'all' || p.state === state);
      sendJson(res, 200, paginate(list, query).map((p) => pullJson(repo, store, p)));
      return true;
    }
    return false;
  }

  if (rest.length >= 2 && rest[0] === 'pulls') {
    const number = parseInt(rest[1], 10);
    const pull = Number.isFinite(number) ? repo.findPull(number) : undefined;
    if (!pull) {
      sendError(res, 404, 'Not Found');
      return true;
    }
    if (rest.length === 2 && method === 'GET') {
      sendJson(res, 200, pullJson(repo, store, pull));
      return true;
    }
    if (rest.length === 3 && rest[2] === 'files' && method === 'GET') {
      sendJson(res, 200, paginate(pullChangedFiles(repo, pull), query));
      return true;
    }
    if (rest.length === 3 && rest[2] === 'merge') {
      if (method === 'GET') {
        if (pull.merged) sendJson(res, 204, null);
        else sendError(res, 404, 'Not Found');
        return true;
      }
      if (method === 'PUT') {
        const result = mergePull(store, repo, pull, typeof b.merge_method === 'string' ? b.merge_method : undefined);
        if (!result.ok) {
          if (result.conflict) sendError(res, 405, 'Pull Request is not mergeable');
          else sendError(res, 422, 'Validation Failed');
          return true;
        }
        sendJson(res, 200, {
          sha: result.sha,
          merged: true,
          message: 'Pull Request successfully merged',
        });
        return true;
      }
      return false;
    }
    return false;
  }

  return false;
}

export function createApp(): Server {
  const store = new Store();

  return createServer(async (req: IncomingMessage, res: ServerResponse) => {
    try {
      const method = (req.method || 'GET').toUpperCase();
      const rawUrl = req.url ?? '/';
      const qmark = rawUrl.indexOf('?');
      const pathname = qmark < 0 ? rawUrl : rawUrl.slice(0, qmark);
      const query = qmark < 0 ? '' : rawUrl.slice(qmark + 1);
      const segs = pathSegments(pathname);

      // /healthz needs no auth
      if (method === 'GET' && pathname === '/healthz') {
        sendJson(res, 200, { ok: true });
        return;
      }

      // All /api/v3 requires auth (including unknown)
      if (pathname === '/api/v3' || pathname.startsWith('/api/v3/')) {
        if (!isAuthed(req)) {
          sendError(res, 401, 'Requires authentication');
          return;
        }

        // Read body before routing
        let bodyBuf: Buffer = Buffer.alloc(0);
        if (requestHasBody(req, method)) {
          try {
            bodyBuf = await readBody(req);
          } catch {
            sendError(res, 400, 'Problems parsing JSON');
            return;
          }
        }
        let bodyJson: any = undefined;
        let bodyPresent = bodyBuf.length > 0;
        if (bodyPresent) {
          const s = bodyBuf.toString('utf8');
          if (s.trim().length > 0) {
            try {
              bodyJson = JSON.parse(s);
            } catch {
              sendError(res, 400, 'Problems parsing JSON');
              return;
            }
          } else {
            bodyPresent = false;
          }
        }

        const apiHandled = await routeApiV3(req, res, method, pathname, segs, query, bodyJson, bodyPresent, store);
        if (!apiHandled) {
          // unknown /api/v3 route
          sendError(res, 404, 'Not Found');
        }
        return;
      }

      if (pathname === '/api/graphql') {
        if (!isAuthed(req)) {
          sendError(res, 401, 'Requires authentication');
          return;
        }
        if (method !== 'POST') {
          sendError(res, 404, 'Not Found');
          return;
        }
        let gqlBody: Buffer;
        try {
          gqlBody = await readBody(req);
        } catch {
          sendError(res, 400, 'Problems parsing JSON');
          return;
        }
        let gqlPayload: unknown;
        try {
          gqlPayload = JSON.parse(gqlBody.toString('utf8'));
        } catch {
          sendError(res, 400, 'Problems parsing JSON');
          return;
        }
        sendJson(res, 200, handleGraphql(gqlPayload, store));
        return;
      }
      if (pathname === '/mcp' || pathname.startsWith('/mcp/')) {
        if (!isAuthed(req)) {
          sendError(res, 401, 'Requires authentication');
          return;
        }
        let mcpBody: Buffer | null = null;
        if (method === 'POST') {
          try {
            mcpBody = await readBody(req);
          } catch {
            mcpBody = null;
          }
        }
        handleMcp(res, method, mcpBody, store);
        return;
      }
      // git smart HTTP
      if (segs.length >= 2 && segs[1].endsWith('.git')) {
        let gitBody: Buffer = Buffer.alloc(0);
        if (method === 'POST') {
          try {
            gitBody = await readBody(req, 32 * 1024 * 1024);
          } catch {
            sendError(res, 400, 'Bad request');
            return;
          }
        }
        const handled = await handleGitHttp(req, res, method, segs, gitBody, store, new URL(rawUrl, 'http://entry'));
        if (handled) return;
        sendError(res, 404, 'Not Found');
        return;
      }

      // Everything else
      res.writeHead(404, { 'content-type': JSON_TYPE });
      res.end(JSON.stringify({ message: 'Not Found' }));
    } catch (e) {
      try {
        // If it's an /api/v3 request, must include request-id per spec; but if headers sent can't change.
        if (!res.headersSent) {
          const p = rawPath(req.url ?? '/');
          if (p === '/api/v3' || p.startsWith('/api/v3/')) {
            sendError(res, 500, 'Internal Server Error');
          } else {
            res.writeHead(500, { 'content-type': JSON_TYPE });
            res.end(JSON.stringify({ message: 'Internal Server Error' }));
          }
        } else {
          res.end();
        }
      } catch {}
    }
  });
}

async function routeApiV3(
  req: IncomingMessage,
  res: ServerResponse,
  method: string,
  pathname: string,
  segs: string[],
  query: string,
  body: any,
  _bodyPresent: boolean,
  store: Store,
): Promise<boolean> {
  // segs example: ['api','v3',...]
  // 1. GET /api/v3/
  if ((pathname === '/api/v3' || pathname === '/api/v3/') && method === 'GET') {
    sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'X-OAuth-Scopes': 'repo, read:org' });
    return true;
  }
  if (segs.length === 3 && segs[0] === 'api' && segs[1] === 'v3' && segs[2] === 'meta' && method === 'GET') {
    sendJson(res, 200, { installed_version: 'github-core-clone-01 1.0.0' });
    return true;
  }
  if (segs.length === 3 && segs[0] === 'api' && segs[1] === 'v3' && segs[2] === 'user' && method === 'GET') {
    sendJson(res, 200, { login: store.login, id: 1000, type: 'User' });
    return true;
  }

  // 4. GET /api/v3/user/repos
  if (segs.length === 4 && segs[0] === 'api' && segs[1] === 'v3' && segs[2] === 'user' && segs[3] === 'repos' && method === 'GET') {
    let perPage = parseInt(queryParam(query, 'per_page') || '30', 10);
    let page = parseInt(queryParam(query, 'page') || '1', 10);
    if (!Number.isFinite(perPage)) perPage = 30;
    if (!Number.isFinite(page)) page = 1;
    perPage = Math.max(1, Math.min(100, perPage));
    page = Math.max(1, page);
    const owned = Array.from(store.repos.values()).filter((r) => r.owner === store.login);
    owned.sort((a, b) => b.id - a.id); // newest first, deterministic
    const start = (page - 1) * perPage;
    const slice = owned.slice(start, start + perPage);
    const hasNext = start + perPage < owned.length;
    const headers: Record<string, string> = {};
    if (hasNext) {
      const host = req.headers.host || 'localhost';
      headers['Link'] = `<http://${host}/api/v3/user/repos?per_page=${perPage}&page=${page + 1}>; rel="next"`;
    }
    sendJson(res, 200, slice.map(repoToJson), headers);
    return true;
  }

  // 5. POST /api/v3/user/repos
  if (segs.length === 4 && segs[0] === 'api' && segs[1] === 'v3' && segs[2] === 'user' && segs[3] === 'repos' && method === 'POST') {
    const b = body || {};
    const name = typeof b.name === 'string' ? b.name : '';
    if (!name) {
      sendError(res, 422, 'Repository creation failed');
      return true;
    }
    if (store.repos.has(store.repoKey(store.login, name))) {
      sendError(res, 422, 'Repository creation failed: name already exists');
      return true;
    }
    const description = typeof b.description === 'string' ? b.description : '';
    const isPrivate = typeof b.private === 'boolean' ? b.private : false;
    const repo = new Repo(store.login, name, store.nextRepoId++, description, isPrivate, store.nowIso());
    // auto_init ignored except we could create initial commit, but spec doesn't require
    store.repos.set(store.repoKey(repo.owner, repo.name), repo);
    sendJson(res, 201, repoToJson(repo));
    return true;
  }

  // Repo-scoped routes /api/v3/repos/{owner}/{repo}...
  if (segs.length >= 5 && segs[0] === 'api' && segs[1] === 'v3' && segs[2] === 'repos') {
    const owner = segs[3];
    const repoName = segs[4];
    const repo = store.getRepo(owner, repoName);
    // For routes on non-existent or foreign repo -> 404
    // But need to be careful: creation routes? none here.
    if (!repo) {
      // any /api/v3/repos/... unknown or missing -> 404
      sendError(res, 404, 'Not Found');
      return true;
    }

    // 6-8 /repos/{owner}/{repo}
    if (segs.length === 5) {
      if (method === 'GET') { sendJson(res, 200, repoToJson(repo)); return true; }
      if (method === 'PATCH') {
        const b = body || {};
        if (typeof b.description === 'string') repo.description = b.description;
        if (typeof b.private === 'boolean') repo.private = b.private;
        if (typeof b.name === 'string' && b.name && b.name !== repo.name) {
          // rename: update map key
          store.repos.delete(store.repoKey(repo.owner, repo.name));
          repo.name = b.name;
          store.repos.set(store.repoKey(repo.owner, repo.name), repo);
        }
        sendJson(res, 200, repoToJson(repo));
        return true;
      }
      if (method === 'DELETE') {
        store.repos.delete(store.repoKey(repo.owner, repo.name));
        sendJson(res, 204, null);
        return true;
      }
      return false;
    }

    const rest = segs.slice(5); // path under repo

    if (routeIssuesPulls(res, method, rest, query, body, repo, store)) return true;

    // 9. POST /git/refs
    if (rest.length === 2 && rest[0] === 'git' && rest[1] === 'refs' && method === 'POST') {
      const b = body || {};
      const ref = b.ref;
      const sha = b.sha;
      if (typeof ref !== 'string' || typeof sha !== 'string') {
        sendError(res, 422, 'Reference update failed');
        return true;
      }
      if (!repo.objects.has(sha) || repo.refs.has(ref)) {
        sendError(res, 422, 'Reference update failed');
        return true;
      }
      repo.refs.set(ref, sha);
      repo.rev++;
      sendJson(res, 201, { ref, object: { type: 'commit', sha }, url: `/api/v3/repos/${repo.owner}/${repo.name}/git/${ref}` });
      return true;
    }

    // 10. GET /git/refs/heads/{branch...}
    if (rest.length >= 4 && rest[0] === 'git' && rest[1] === 'refs' && rest[2] === 'heads' && method === 'GET') {
      const branch = rest.slice(3).join('/');
      const refName = `refs/heads/${branch}`;
      const sha = repo.refs.get(refName);
      if (!sha) { sendError(res, 404, 'Not Found'); return true; }
      sendJson(res, 200, { ref: refName, object: { type: 'commit', sha } });
      return true;
    }

    // 11. GET /branches
    if (rest.length === 1 && rest[0] === 'branches' && method === 'GET') {
      const branches: any[] = [];
      for (const [ref, sha] of repo.refs.entries()) {
        if (ref.startsWith('refs/heads/')) {
          const name = ref.slice('refs/heads/'.length);
          branches.push({ name, commit: { sha }, protected: false });
        }
      }
      branches.sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
      sendJson(res, 200, branches);
      return true;
    }

    // 12. GET /branches/{branch...}
    if (rest.length >= 2 && rest[0] === 'branches' && method === 'GET') {
      const branch = rest.slice(1).join('/');
      const sha = repo.refs.get(`refs/heads/${branch}`);
      if (!sha) { sendError(res, 404, 'Not Found'); return true; }
      sendJson(res, 200, { name: branch, commit: { sha } });
      return true;
    }

    // 13. GET /commits
    if (rest.length === 1 && rest[0] === 'commits' && method === 'GET') {
      const shaParam = queryParam(query, 'sha') || undefined;
      const perPageRaw = queryParam(query, 'per_page');
      const perPage = perPageRaw ? Math.max(1, parseInt(perPageRaw, 10) || 30) : undefined;
      const tip = resolveCommitRef(repo, shaParam);
      if (!tip) {
        // if repo has no commits and sha omitted -> 200 [] per spec
        if (!shaParam && !ensureDefaultBranchRef(repo)) {
          sendJson(res, 200, []);
          return true;
        }
        // if explicit sha/branch unknown
        if (shaParam) { sendError(res, 404, 'Not Found'); return true; }
        sendJson(res, 200, []);
        return true;
      }
      let shas = walkCommitsFirstParent(repo, tip);
      if (perPage !== undefined) shas = shas.slice(0, perPage);
      const commits = shas.map((s) => commitToJson(repo, s)).filter(Boolean);
      sendJson(res, 200, commits);
      return true;
    }

    // 14,15,16 contents
    if (rest[0] === 'contents' && rest.length >= 2) {
      const path = rest.slice(1).join('/');
      if (method === 'GET') {
        return handleGetContents(req, res, repo, path, queryParam(query, 'ref') || undefined);
      }
      if (method === 'PUT') {
        return handlePutContents(req, res, repo, path, body || {}, store);
      }
      if (method === 'DELETE') {
        return handleDeleteContents(req, res, repo, path, body || {}, store);
      }
      return false;
    }

    return false;
  }

  return false;
}

function handleGetContents(_req: IncomingMessage, res: ServerResponse, repo: Repo, path: string, refParam?: string): boolean {
  const tip = resolveCommitRef(repo, refParam);
  if (!tip) { sendError(res, 404, 'Not Found'); return true; }
  const commitObj = getObject(repo, tip);
  if (!commitObj || commitObj.type !== 'commit') { sendError(res, 404, 'Not Found'); return true; }
  const treeOid = parseCommit(commitObj.data).tree;

  // Determine if path is file or dir
  // Try file first
  const blobSha = readFile(repo, treeOid, path);
  if (blobSha) {
    const blob = getObject(repo, blobSha);
    if (!blob || blob.type !== 'blob') { sendError(res, 404, 'Not Found'); return true; }
    const name = path.split('/').pop() || path;
    const contentB64 = blob.data.toString('base64');
    sendJson(res, 200, {
      type: 'file',
      encoding: 'base64',
      size: blob.data.length,
      name,
      path,
      sha: blobSha,
      content: contentB64,
      git_url: `/api/v3/repos/${repo.owner}/${repo.name}/git/blobs/${blobSha}`,
      html_url: `/${repo.owner}/${repo.name}/blob/${refParam || repo.defaultBranch}/${path}`,
      download_url: `/raw/${repo.owner}/${repo.name}/${refParam || repo.defaultBranch}/${path}`,
    });
    return true;
  }

  // Try directory
  // listDir returns entries if it's a dir (including root case not applicable here since path non-empty)
  // We need to distinguish "is dir" from "not found". Check by traversing.
  const isDir = isDirectoryAtPath(repo, treeOid, path);
  if (isDir) {
    const entries = listDir(repo, treeOid, path);
    const arr = entries.map((e) => ({
      type: e.type,
      size: e.type === 'file' ? (e.size ?? 0) : 0,
      name: e.name,
      path: e.path,
      sha: e.sha,
    }));
    sendJson(res, 200, arr);
    return true;
  }

  sendError(res, 404, 'Not Found');
  return true;
}

function isDirectoryAtPath(repo: Repo, treeOid: string, dirPath: string): boolean {
  const dp = dirPath.replace(/^\/+|\/+$/g, '');
  if (!dp) return true;
  const parts = dp.split('/').filter(Boolean);
  let cur = treeOid;
  for (const part of parts) {
    const obj = getObject(repo, cur);
    if (!obj || obj.type !== 'tree') return false;
    const ent = parseTreeLocal(obj.data).find((e) => e.name === part);
    if (!ent || ent.mode !== '40000') return false;
    cur = ent.oid;
  }
  return true;
}

function parseTreeLocal(data: Buffer): { mode: string; name: string; oid: string }[] {
  // lightweight local to avoid circular import issues? no, git.ts exports parseTree; but we imported from git.ts already not parseTree.
  const out: { mode: string; name: string; oid: string }[] = [];
  let i = 0;
  while (i < data.length) {
    const sp = data.indexOf(0x20, i);
    if (sp < 0) break;
    const nul = data.indexOf(0x00, sp + 1);
    if (nul < 0) break;
    const mode = data.subarray(i, sp).toString();
    const name = data.subarray(sp + 1, nul).toString();
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    out.push({ mode, name, oid });
    i = nul + 21;
  }
  return out;
}

function handlePutContents(_req: IncomingMessage, res: ServerResponse, repo: Repo, path: string, body: any, _store: Store): boolean {
  if (typeof body.message !== 'string' || typeof body.content !== 'string') {
    // missing required fields
    sendError(res, 422, 'Invalid request');
    return true;
  }
  const branchName = typeof body.branch === 'string' && body.branch ? body.branch : repo.defaultBranch;
  const refName = `refs/heads/${branchName}`;

  let baseTip: string | undefined = repo.refs.get(refName);
  let baseTree: string | null = null;

  if (!baseTip) {
    const defaultTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    if (!defaultTip) {
      // unborn repo: create first commit on default branch (spec says for unborn default branch)
      if (branchName !== repo.defaultBranch) {
        // named branch on unborn repo - spec states "For an UNBORN default branch ... PUT creates the first commit."
        // but if branch different, treat as unborn on that branch? probably create as first commit.
      }
      baseTip = undefined;
      baseTree = null;
    } else {
      // named branch doesn't exist but repo has commits: create branch at default tip
      repo.refs.set(refName, defaultTip);
      repo.rev++;
      baseTip = defaultTip;
    }
  }

  if (baseTip) {
    const cobj = getObject(repo, baseTip);
    if (cobj && cobj.type === 'commit') baseTree = parseCommit(cobj.data).tree;
  }

  const contentBuf = Buffer.from(String(body.content).replace(/\s+/g, ''), 'base64');
  const existingBlob = baseTree ? readFile(repo, baseTree, path) : null;
  const isCreate = !existingBlob;

  if (typeof body.sha === 'string') {
    if (!existingBlob) { sendError(res, 409, 'sha does not match'); return true; }
    if (existingBlob !== body.sha) { sendError(res, 409, 'sha does not match'); return true; }
  }

  const blobOid = putObject(repo, 'blob', contentBuf);
  const newTree = writeFile(repo, baseTree, path, blobOid);
  const ident = fixedIdentity(body.committer);
  let msg = body.message;
  if (!msg.endsWith('\n')) msg += '\n';
  const commitBuf = buildCommit({
    tree: newTree,
    parents: baseTip ? [baseTip] : [],
    message: msg,
    author: ident,
    committer: ident,
  });
  const commitSha = putObject(repo, 'commit', commitBuf);
  repo.refs.set(refName, commitSha);
  if (branchName === repo.defaultBranch) {
    repo.defaultBranch = 'main'; // always
  }
  repo.rev++;

  const commitJson = commitToJson(repo, commitSha);
  const name = path.split('/').pop() || path;
  sendJson(res, isCreate ? 201 : 200, {
    content: {
      name,
      path,
      sha: blobOid,
      size: contentBuf.length,
      encoding: 'base64',
      content: contentBuf.toString('base64'),
    },
    commit: commitJson ? { sha: commitJson.sha, message: commitJson.commit.message, author: commitJson.commit.author, committer: commitJson.commit.committer } : { sha: commitSha },
  });
  return true;
}

function handleDeleteContents(_req: IncomingMessage, res: ServerResponse, repo: Repo, path: string, body: any, _store: Store): boolean {
  if (typeof body.message !== 'string') {
    sendError(res, 422, 'Invalid request');
    return true;
  }
  const branchName = typeof body.branch === 'string' && body.branch ? body.branch : repo.defaultBranch;
  const refName = `refs/heads/${branchName}`;
  const tip = repo.refs.get(refName);
  if (!tip) { sendError(res, 404, 'Not Found'); return true; }
  const cobj = getObject(repo, tip);
  if (!cobj || cobj.type !== 'commit') { sendError(res, 404, 'Not Found'); return true; }
  const baseTree = parseCommit(cobj.data).tree;
  const existingBlob = readFile(repo, baseTree, path);
  if (!existingBlob) { sendError(res, 404, 'Not Found'); return true; }
  if (typeof body.sha === 'string' && body.sha !== existingBlob) {
    sendError(res, 409, 'sha does not match');
    return true;
  }
  const newTree = writeFile(repo, baseTree, path, null);
  const ident = fixedIdentity(body.committer);
  let msg = body.message;
  if (!msg.endsWith('\n')) msg += '\n';
  const commitBuf = buildCommit({
    tree: newTree,
    parents: [tip],
    message: msg,
    author: ident,
    committer: ident,
  });
  const commitSha = putObject(repo, 'commit', commitBuf);
  repo.refs.set(refName, commitSha);
  repo.rev++;

  sendJson(res, 200, { content: null, commit: { sha: commitSha } });
  return true;
}
