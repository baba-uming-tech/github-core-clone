import { buildCommit, getObject, nextRestTimestamp, parseCommit, parseTree, putObject, writeFile } from './git.ts';
import type { Pull, Repo, Store } from './store.ts';

export function commitTree(repo: Repo, sha: string): string | undefined {
  const obj = getObject(repo, sha);
  if (!obj || obj.type !== 'commit') return undefined;
  return parseCommit(obj.data).tree;
}

export function treeFiles(repo: Repo, treeOid: string): Map<string, { sha: string; mode: string }> {
  const out = new Map<string, { sha: string; mode: string }>();
  if (!treeOid) return out;
  const stack: { oid: string; prefix: string }[] = [{ oid: treeOid, prefix: '' }];
  const seen = new Set<string>();
  while (stack.length) {
    const { oid, prefix } = stack.pop()!;
    const key = `${prefix}\0${oid}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const obj = getObject(repo, oid);
    if (!obj || obj.type !== 'tree') continue;
    for (const e of parseTree(obj.data)) {
      const path = prefix ? `${prefix}/${e.name}` : e.name;
      if (e.mode === '40000') stack.push({ oid: e.oid, prefix: path });
      else out.set(path, { sha: e.oid, mode: e.mode });
    }
  }
  return out;
}

function commitDistances(repo: Repo, tip: string): Map<string, number> {
  const dist = new Map<string, number>();
  const queue: { oid: string; d: number }[] = [{ oid: tip, d: 0 }];
  let qi = 0;
  while (qi < queue.length) {
    const { oid, d } = queue[qi++];
    if (dist.has(oid)) continue;
    const obj = getObject(repo, oid);
    if (!obj || obj.type !== 'commit') continue;
    dist.set(oid, d);
    for (const parent of parseCommit(obj.data).parents) queue.push({ oid: parent, d: d + 1 });
  }
  return dist;
}

export function mergeBase(repo: Repo, a: string, b: string): string | undefined {
  const da = commitDistances(repo, a);
  const db = commitDistances(repo, b);
  let best: string | undefined;
  let bestScore = Infinity;
  for (const [oid, d] of da) {
    const other = db.get(oid);
    if (other === undefined) continue;
    const score = d + other;
    if (score < bestScore) {
      bestScore = score;
      best = oid;
    }
  }
  return best;
}

function lineCount(repo: Repo, sha: string): number {
  const obj = getObject(repo, sha);
  if (!obj || obj.type !== 'blob' || obj.data.length === 0) return 0;
  const text = obj.data.toString('utf8');
  let n = text.split('\n').length;
  if (text.endsWith('\n')) n--;
  return Math.max(1, n);
}

export interface PullFile {
  sha: string;
  filename: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
  blob_url: string;
  raw_url: string;
}

export function pullChangedFiles(repo: Repo, pull: Pull): PullFile[] {
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`) || pull.baseSha;
  const headTip = repo.refs.get(`refs/heads/${pull.headRef}`) || pull.headSha;
  const mb = mergeBase(repo, baseTip, headTip);
  const mbTree = mb ? commitTree(repo, mb) ?? null : null;
  const baseFiles = mbTree ? treeFiles(repo, mbTree) : new Map<string, { sha: string; mode: string }>();
  const headTree = commitTree(repo, headTip);
  const headFiles = headTree ? treeFiles(repo, headTree) : new Map<string, { sha: string; mode: string }>();
  const paths = Array.from(new Set([...baseFiles.keys(), ...headFiles.keys()])).sort();
  const files: PullFile[] = [];
  for (const path of paths) {
    const headEntry = headFiles.get(path);
    const baseEntry = baseFiles.get(path);
    let status: 'added' | 'modified' | 'removed';
    let additions = 0;
    let deletions = 0;
    let sha: string;
    if (headEntry && !baseEntry) {
      status = 'added';
      sha = headEntry.sha;
      additions = lineCount(repo, sha);
    } else if (headEntry && baseEntry && headEntry.sha !== baseEntry.sha) {
      status = 'modified';
      sha = headEntry.sha;
      additions = lineCount(repo, sha);
    } else if (!headEntry && baseEntry) {
      status = 'removed';
      sha = baseEntry.sha;
      deletions = lineCount(repo, sha);
    } else {
      continue;
    }
    files.push({
      sha,
      filename: path,
      status,
      additions,
      deletions,
      changes: additions + deletions,
      blob_url: `/${repo.owner}/${repo.name}/blob/${pull.headRef}/${path}`,
      raw_url: `/raw/${repo.owner}/${repo.name}/${pull.headRef}/${path}`,
    });
    if (files.length >= 300) break;
  }
  return files;
}

interface MergePlan {
  baseRefName: string;
  baseTip: string;
  baseTree: string;
  headTree: string;
  baseFiles: Map<string, { sha: string; mode: string }>;
  headFiles: Map<string, { sha: string; mode: string }>;
  conflict: boolean;
}

function planMerge(repo: Repo, pull: Pull): MergePlan | null {
  const baseRefName = `refs/heads/${pull.baseRef}`;
  const baseTip = repo.refs.get(baseRefName);
  const baseTree = baseTip ? commitTree(repo, baseTip) : undefined;
  const headTip = repo.refs.get(`refs/heads/${pull.headRef}`) || pull.headSha;
  const headTree = commitTree(repo, headTip);
  if (!baseTip || !baseTree || !headTree) return null;
  const mb = mergeBase(repo, baseTip, headTip);
  const mbTree = mb ? commitTree(repo, mb) ?? null : null;
  const baseFiles = mbTree ? treeFiles(repo, mbTree) : new Map<string, { sha: string; mode: string }>();
  const headFiles = treeFiles(repo, headTree);
  const tipFiles = treeFiles(repo, baseTree);
  let conflict = false;
  for (const [path, headEntry] of headFiles) {
    const mbEntry = baseFiles.get(path);
    if (mbEntry && mbEntry.sha === headEntry.sha) continue;
    const tipEntry = tipFiles.get(path);
    if (tipEntry && (!mbEntry || mbEntry.sha !== tipEntry.sha) && tipEntry.sha !== headEntry.sha) {
      conflict = true;
      break;
    }
  }
  if (!conflict) {
    for (const [path, mbEntry] of baseFiles) {
      if (headFiles.has(path)) continue;
      const tipEntry = tipFiles.get(path);
      if (tipEntry && tipEntry.sha !== mbEntry.sha) {
        conflict = true;
        break;
      }
    }
  }
  return { baseRefName, baseTip, baseTree, headTree, baseFiles, headFiles, conflict };
}

export function isPullMergeable(repo: Repo, pull: Pull): boolean {
  const plan = planMerge(repo, pull);
  return plan ? !plan.conflict : false;
}

export type MergeResult =
  | { ok: true; sha: string }
  | { ok: false; conflict: boolean; message: string };

export function mergePull(store: Store, repo: Repo, pull: Pull, _method?: string, commitHeadline?: string): MergeResult {
  const plan = planMerge(repo, pull);
  if (!plan) return { ok: false, conflict: false, message: 'Validation Failed' };
  if (plan.conflict) return { ok: false, conflict: true, message: 'Pull request is not mergeable' };

  let tree = plan.baseTree;
  for (const [path, headEntry] of plan.headFiles) tree = writeFile(repo, tree, path, headEntry.sha);
  for (const path of plan.baseFiles.keys()) {
    if (!plan.headFiles.has(path)) tree = writeFile(repo, tree, path, null);
  }

  const ident = { name: 'ada iversen', email: 'adaiversen@example.invalid', timestamp: nextRestTimestamp() };
  const message = `Merge pull request #${pull.number} from ${pull.headOwner}:${pull.headRef}`;
  const commitSha = putObject(repo, 'commit', buildCommit({
    tree,
    parents: [plan.baseTip],
    message,
    author: ident,
    committer: ident,
  }));
  repo.refs.set(plan.baseRefName, commitSha);
  const mergedAt = store.nowIso();
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = mergedAt;
  pull.mergeCommitSha = commitSha;
  pull.closedAt = mergedAt;
  pull.updatedAt = mergedAt;
  repo.rev++;
  return { ok: true, sha: commitSha };
}
