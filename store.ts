import { createHash } from 'node:crypto';

export type ObjectType = 'blob' | 'tree' | 'commit' | 'tag';
export interface GitObject { type: ObjectType; data: Buffer }

export interface Label {
  id: number;
  name: string;
  color: string;
  description: string | null;
}
export interface Comment {
  id: number;
  body: string;
  createdAt: string;
  updatedAt: string;
}
export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  stateReason: string | null;
  labels: Label[];
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  comments: Comment[];
  version: number;
  jsonCache?: Buffer;
  jsonCacheVersion?: number;
}
export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  headOwner: string;
  headRef: string;
  headSha: string;
  baseOwner: string;
  baseRef: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  draft: boolean;
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
}

export class Repo {
  owner: string;
  name: string;
  id: number;
  description: string;
  private: boolean;
  createdAt: string;
  defaultBranch = 'main';
  refs = new Map<string, string>();
  objects = new Map<string, GitObject>();
  labels = new Map<string, Label>();
  issues: Issue[] = [];
  issueSeq = 0;
  commentSeq = 0;
  pulls: Pull[] = [];
  pullSeq = 0;
  rev = 0;

  constructor(owner: string, name: string, id: number, description: string, isPrivate: boolean, createdAt: string) {
    this.owner = owner;
    this.name = name;
    this.id = id;
    this.description = description;
    this.private = isPrivate;
    this.createdAt = createdAt;
  }

  findIssue(number: number): Issue | undefined {
    return this.issues.find((i) => i.number === number);
  }

  findPull(number: number): Pull | undefined {
    return this.pulls.find((p) => p.number === number);
  }
}

export class Store {
  login = 'adaiversen';
  repos = new Map<string, Repo>();
  nextRepoId = 1;
  nextIssueId = 1;
  nextLabelId = 1;
  nextCommentId = 1;
  nextPullId = 1;
  nowCounter = 0;

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`.toLowerCase();
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.getRepoByKey(this.repoKey(owner, name));
  }

  getRepoByKey(key: string): Repo | undefined {
    const repo = this.repos.get(key);
    if (!repo) return undefined;
    if (repo.owner !== this.login) return undefined;
    return repo;
  }

  nowIso(): string {
    // monotonic enough per process
    const base = Date.now() + this.nowCounter++;
    return new Date(base).toISOString();
  }
}

export function hashObject(type: ObjectType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`);
  return createHash('sha1').update(header).update(data).digest('hex');
}

export function putObject(repo: Repo, type: ObjectType, data: Buffer): string {
  const sha = hashObject(type, data);
  if (!repo.objects.has(sha)) {
    repo.objects.set(sha, { type, data });
    repo.rev++;
  }
  return sha;
}

export function getObject(repo: Repo, sha: string): GitObject | undefined {
  return repo.objects.get(sha);
}
