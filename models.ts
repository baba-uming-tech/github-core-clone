import { Buffer } from 'node:buffer';
import type { Comment, Issue, Label, Pull, Repo, Store } from './store.ts';

function repoUrls(owner: string, name: string) {
  const full = `${owner}/${name}`;
  return {
    url: `/api/v3/repos/${full}`,
    html_url: `/${full}`,
    git_url: `/api/v3/repos/${full}.git`,
    clone_url: `/repos/${full}.git`,
  };
}

export function repoToJson(repo: Repo) {
  const urls = repoUrls(repo.owner, repo.name);
  const ts = repo.createdAt;
  return {
    id: repo.id,
    node_id: `repo:${repo.id}`,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: 1000, type: 'User' },
    private: repo.private,
    description: repo.description,
    fork: false,
    created_at: ts,
    updated_at: ts,
    pushed_at: ts,
    default_branch: repo.defaultBranch || 'main',
    size: 0,
    ...urls,
  };
}

export function userJson(store: Store) {
  return { login: store.login, id: 1000, type: 'User' };
}

export function labelJson(label: Label) {
  return {
    id: label.id,
    name: label.name,
    color: label.color,
    description: label.description ?? null,
  };
}

export function issueJson(repo: Repo, store: Store, issue: Issue) {
  const full = `${repo.owner}/${repo.name}`;
  return {
    id: issue.id,
    node_id: `issue:${issue.id}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    state_reason: issue.stateReason,
    locked: false,
    labels: issue.labels.map(labelJson),
    user: userJson(store),
    assignee: null,
    assignees: [],
    comments: issue.comments.length,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.closedAt,
    html_url: `/${full}/issues/${issue.number}`,
    url: `/api/v3/repos/${full}/issues/${issue.number}`,
  };
}

export function cachedIssueJson(repo: Repo, store: Store, issue: Issue): Buffer {
  if (issue.jsonCache === undefined || issue.jsonCacheVersion !== issue.version) {
    issue.jsonCache = Buffer.from(JSON.stringify(issueJson(repo, store, issue)));
    issue.jsonCacheVersion = issue.version;
  }
  return issue.jsonCache;
}

export function commentJson(store: Store, comment: Comment) {
  return {
    id: comment.id,
    body: comment.body,
    user: userJson(store),
    created_at: comment.createdAt,
    updated_at: comment.updatedAt,
  };
}

export function pullJson(repo: Repo, store: Store, pull: Pull) {
  const full = `${repo.owner}/${repo.name}`;
  const user = userJson(store);
  return {
    id: pull.id,
    node_id: `pull:${pull.id}`,
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    user,
    head: {
      label: `${pull.headOwner}:${pull.headRef}`,
      ref: pull.headRef,
      sha: pull.headSha,
      user,
    },
    base: {
      label: `${pull.baseOwner}:${pull.baseRef}`,
      ref: pull.baseRef,
      sha: pull.baseSha,
      user,
    },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    mergeable: true,
    mergeable_state: 'clean',
    draft: pull.draft,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    closed_at: pull.closedAt,
    html_url: `/${full}/pull/${pull.number}`,
    url: `/api/v3/repos/${full}/pulls/${pull.number}`,
  };
}
