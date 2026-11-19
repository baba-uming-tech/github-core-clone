import { Buffer } from 'node:buffer';
import { type GitObject, type ObjectType, type Repo, getObject, hashObject, putObject } from './store.ts';

export { hashObject, putObject, getObject };

export interface TreeEntry { mode: string; name: string; oid: string }

function compareTreeName(a: string, aIsDir: boolean, b: string, bIsDir: boolean): number {
  const an = a + (aIsDir ? '/' : '');
  const bn = b + (bIsDir ? '/' : '');
  if (an < bn) return -1;
  if (an > bn) return 1;
  return 0;
}

export function encodeTree(entries: { mode: string; name: string; oid: string }[]): Buffer {
  const sorted = [...entries].sort((a, b) =>
    compareTreeName(a.name, a.mode === '40000', b.name, b.mode === '40000')
  );
  const chunks: Buffer[] = [];
  for (const e of sorted) {
    const head = Buffer.from(`${e.mode} ${e.name}\0`);
    const oid = Buffer.from(e.oid, 'hex');
    chunks.push(head, oid);
  }
  return Buffer.concat(chunks);
}

export function parseTree(data: Buffer): TreeEntry[] {
  const out: TreeEntry[] = [];
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

export interface CommitBuildInput {
  tree: string;
  parents: string[];
  message: string;
  author: { name: string; email: string; timestamp: number };
  committer: { name: string; email: string; timestamp: number };
}

export function buildCommit(input: CommitBuildInput): Buffer {
  const lines: string[] = [];
  lines.push(`tree ${input.tree}`);
  for (const p of input.parents) lines.push(`parent ${p}`);
  lines.push(`author ${input.author.name} <${input.author.email}> ${input.author.timestamp} +0000`);
  lines.push(`committer ${input.committer.name} <${input.committer.email}> ${input.committer.timestamp} +0000`);
  let msg = input.message;
  if (!msg.endsWith('\n')) msg += '\n';
  return Buffer.from(lines.join('\n') + '\n\n' + msg);
}

export interface ParsedCommit {
  tree: string;
  parents: string[];
  authorLine: string;
  committerLine: string;
  message: string;
}

export function parseCommit(data: Buffer): ParsedCommit {
  const text = data.toString();
  const sep = text.indexOf('\n\n');
  const head = sep >= 0 ? text.slice(0, sep) : text;
  const message = sep >= 0 ? text.slice(sep + 2) : '';
  const lines = head.split('\n');
  let tree = '';
  const parents: string[] = [];
  let authorLine = '';
  let committerLine = '';
  for (const line of lines) {
    if (line.startsWith('tree ')) tree = line.slice(5);
    else if (line.startsWith('parent ')) parents.push(line.slice(7));
    else if (line.startsWith('author ')) authorLine = line.slice(7);
    else if (line.startsWith('committer ')) committerLine = line.slice(10);
  }
  return { tree, parents, authorLine, committerLine, message };
}

export function collectReachable(tips: string[], lookup: (sha: string) => GitObject | undefined): Set<string> {
  const seen = new Set<string>();
  const stack = [...tips];
  while (stack.length) {
    const sha = stack.pop()!;
    if (!sha || seen.has(sha)) continue;
    const obj = lookup(sha);
    if (!obj) continue;
    seen.add(sha);
    if (obj.type === 'commit') {
      const c = parseCommit(obj.data);
      stack.push(c.tree, ...c.parents);
    } else if (obj.type === 'tree') {
      const entries = parseTree(obj.data);
      for (const e of entries) stack.push(e.oid);
    } else if (obj.type === 'tag') {
      // minimal: try to read object line if present
      const text = obj.data.toString();
      const m = text.match(/^object ([0-9a-f]{40})/m);
      if (m) stack.push(m[1]);
    }
  }
  return seen;
}

export function readFile(repo: Repo, treeOid: string, path: string): string | null {
  if (!path || path === '/' || path === '.') return null;
  const parts = path.split('/').filter(Boolean);
  let cur = treeOid;
  for (let i = 0; i < parts.length; i++) {
    const obj = getObject(repo, cur);
    if (!obj || obj.type !== 'tree') return null;
    const entries = parseTree(obj.data);
    const ent = entries.find((e) => e.name === parts[i]);
    if (!ent) return null;
    if (i === parts.length - 1) {
      return ent.mode === '40000' ? null : ent.oid;
    }
    if (ent.mode !== '40000') return null;
    cur = ent.oid;
  }
  return null;
}

export function listDir(repo: Repo, treeOid: string, dirPath: string): { name: string; path: string; type: 'file' | 'dir'; sha: string; size?: number }[] {
  let targetTree = treeOid;
  const dp = dirPath.replace(/^\/+|\/+$/g, '');
  if (dp) {
    const parts = dp.split('/').filter(Boolean);
    for (const part of parts) {
      const obj = getObject(repo, targetTree);
      if (!obj || obj.type !== 'tree') return [];
      const ent = parseTree(obj.data).find((e) => e.name === part);
      if (!ent || ent.mode !== '40000') return [];
      targetTree = ent.oid;
    }
  }
  const obj = getObject(repo, targetTree);
  if (!obj || obj.type !== 'tree') return [];
  const entries = parseTree(obj.data);
  return entries.map((e) => {
    const isDir = e.mode === '40000';
    const p = dp ? `${dp}/${e.name}` : e.name;
    const out: { name: string; path: string; type: 'file' | 'dir'; sha: string; size?: number } = {
      name: e.name,
      path: p,
      type: isDir ? 'dir' : 'file',
      sha: e.oid,
    };
    if (!isDir) {
      const blob = getObject(repo, e.oid);
      if (blob && blob.type === 'blob') out.size = blob.data.length;
      else out.size = 0;
    }
    return out;
  });
}

export function writeFile(repo: Repo, baseTreeOid: string | null, path: string, blobOid: string | null): string {
  const parts = path.split('/').filter(Boolean);
  if (parts.length === 0) throw new Error('empty path');

  function rewrite(treeOid: string | null, idx: number): string | null {
    const name = parts[idx];
    const isLast = idx === parts.length - 1;
    const existing = treeOid ? getObject(repo, treeOid) : undefined;
    const entries = existing && existing.type === 'tree' ? parseTree(existing.data) : [];
    const map = new Map<string, TreeEntry>();
    for (const e of entries) map.set(e.name, e);

    if (isLast) {
      if (blobOid === null) {
        map.delete(name);
      } else {
        map.set(name, { mode: '100644', name, oid: blobOid });
      }
    } else {
      const child = map.get(name);
      const childTree = child && child.mode === '40000' ? child.oid : null;
      const newChild = rewrite(childTree, idx + 1);
      if (newChild === null) {
        map.delete(name);
      } else {
        map.set(name, { mode: '40000', name, oid: newChild });
      }
    }

    const newEntries = Array.from(map.values());
    if (newEntries.length === 0) return null;
    const buf = encodeTree(newEntries);
    return putObject(repo, 'tree', buf);
  }

  const result = rewrite(baseTreeOid, 0);
  if (result === null) {
    // empty root tree
    return putObject(repo, 'tree', Buffer.alloc(0));
  }
  return result;
}

let commitCounter = 0;
export function nextRestTimestamp(): number {
  // fixed constant 1735689600 plus an incrementing counter
  return 1735689600 + (commitCounter++);
}

export function resetCommitCounterForTest() { commitCounter = 0; }

export function parseIdentityLine(line: string): { name: string; email: string; date: string } {
  // "Name <email> 1735689600 +0000"
  const m = line.match(/^(.*) <([^>]*)> (\d+) ([+-]\d{4})$/);
  if (!m) return { name: line, email: '', date: new Date(0).toISOString() };
  const ts = Number(m[3]) * 1000;
  return { name: m[1], email: m[2], date: new Date(ts).toISOString() };
}
