import { Buffer } from 'node:buffer';
import { createHash, randomUUID } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { deflateSync, inflateSync } from 'node:zlib';
import { collectReachable, parseCommit, parseTree } from './git.ts';
import { type GitObject, type ObjectType, type Repo, Store, putObject } from './store.ts';

const MAX_PKT = 65520;
const SIDEBAND_CHUNK = 65000;
const ZERO_OID = '0'.repeat(40);
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';
const UPLOAD_ADVERT = 'application/x-git-upload-pack-advertisement';
const RECEIVE_ADVERT = 'application/x-git-receive-pack-advertisement';

export function pkt(text: string | Buffer): Buffer {
  const payload = typeof text === 'string' ? Buffer.from(text, 'utf8') : text;
  const len = payload.length + 4;
  if (len > MAX_PKT) throw new Error('pkt-line too large');
  return Buffer.concat([Buffer.from(len.toString(16).padStart(4, '0'), 'ascii'), payload]);
}

export function flushPkt(): Buffer {
  return Buffer.from('0000', 'ascii');
}

export function delimPkt(): Buffer {
  return Buffer.from('0001', 'ascii');
}

type PacketKind = 'data' | 'flush' | 'delim' | 'response-end';
interface Packet { kind: PacketKind; payload: Buffer }
export interface DecodedPackets { packets: Packet[]; consumedBytes: number }

export function decodePackets(buf: Buffer): DecodedPackets {
  const packets: Packet[] = [];
  let pos = 0;
  while (pos + 4 <= buf.length) {
    const hex = buf.toString('ascii', pos, pos + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error('malformed pkt-line');
    const len = parseInt(hex, 16);
    pos += 4;
    if (len === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      break;
    }
    if (len === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      continue;
    }
    if (len === 2) {
      packets.push({ kind: 'response-end', payload: Buffer.alloc(0) });
      continue;
    }
    if (len < 4 || pos + len - 4 > buf.length) throw new Error('malformed pkt-line length');
    packets.push({ kind: 'data', payload: buf.subarray(pos, pos + len - 4) });
    pos += len - 4;
  }
  return { packets, consumedBytes: pos };
}

function sendGit(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  if (res.headersSent) return;
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': String(body.length),
    'x-github-request-id': randomUUID(),
  });
  res.end(body);
}

function text(str: string): Buffer {
  return pkt(str + '\n');
}

function stripNl(s: string): string {
  return s.endsWith('\n') ? s.slice(0, -1) : s;
}

function firstOid(s: string): string {
  return s.split(/\s+/)[0];
}

function sortNames<T extends { name: string }>(a: T, b: T): number {
  return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
}

function peel(repo: Repo, oid: string): string | undefined {
  let cur = oid;
  const seen = new Set<string>();
  for (;;) {
    const obj = repo.objects.get(cur);
    if (!obj) return undefined;
    if (obj.type !== 'tag') return cur;
    if (seen.has(cur)) return undefined;
    seen.add(cur);
    const m = obj.data.toString('utf8').match(/^object ([0-9a-fA-F]{40})/m);
    if (!m) return undefined;
    cur = m[1];
  }
}

interface RefInfo { name: string; oid: string; peeled?: string }

function listRefs(repo: Repo): RefInfo[] {
  const heads: RefInfo[] = [];
  const tags: RefInfo[] = [];
  for (const [name, oid] of repo.refs.entries()) {
    if (name.startsWith('refs/heads/')) heads.push({ name, oid });
    else if (name.startsWith('refs/tags/')) tags.push({ name, oid, peeled: peel(repo, oid) });
  }
  heads.sort(sortNames);
  tags.sort(sortNames);
  return [...heads, ...tags];
}

function defaultRef(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

function uploadCaps(repo: Repo): string {
  return `symref=HEAD:${defaultRef(repo)} side-band-64k thin-pack ofs-delta shallow no-progress multi_ack_detailed agent=github-core-clone/1.0`;
}

const RECEIVE_CAPS = 'report-status side-band-64k delete-refs ofs-delta agent=github-core-clone/1.0';

function uploadV2Advert(repo: Repo): Buffer {
  const key = clearCaches(repo);
  const hit = uploadV2Cache.get(key);
  if (hit) return hit;
  const advert = Buffer.concat([
    text('version 2'),
    text('agent=github-core-clone/1.0'),
    text('ls-refs=unborn'),
    text('fetch=shallow wait-for-done'),
    text('server-option'),
    text('object-format=sha1'),
    flushPkt(),
  ]);
  uploadV2Cache.set(key, advert);
  return advert;
}

function uploadV0Advert(repo: Repo): Buffer {
  const refs = listRefs(repo);
  const out: Buffer[] = [text('# service=git-upload-pack'), flushPkt()];
  if (refs.length === 0) {
    out.push(text(`${ZERO_OID} capabilities^{}\0${uploadCaps(repo)}`));
  } else {
    const oid = repo.refs.get(defaultRef(repo)) || refs[0].oid;
    out.push(text(`${oid} HEAD\0${uploadCaps(repo)}`));
    for (const r of refs) {
      out.push(text(`${r.oid} ${r.name}`));
      if (r.peeled) out.push(text(`${r.peeled} ${r.name}^{}`));
    }
  }
  out.push(flushPkt());
  return Buffer.concat(out);
}

function receiveAdvert(repo: Repo): Buffer {
  const key = clearCaches(repo);
  const hit = receiveAdvertCache.get(key);
  if (hit) return hit;
  const refs = listRefs(repo);
  const out: Buffer[] = [text('# service=git-receive-pack'), flushPkt()];
  if (refs.length === 0) {
    out.push(text(`${ZERO_OID} capabilities^{}\0${RECEIVE_CAPS}`));
  } else {
    for (let i = 0; i < refs.length; i++) {
      out.push(text(i === 0 ? `${refs[i].oid} ${refs[i].name}\0${RECEIVE_CAPS}` : `${refs[i].oid} ${refs[i].name}`));
    }
  }
  out.push(flushPkt());
  const advert = Buffer.concat(out);
  receiveAdvertCache.set(key, advert);
  return advert;
}

interface Selection { oids: string[]; newShallow: string[]; unshallow: string[] }

function collectReachableWithShallow(
  tips: string[],
  shallow: Set<string>,
  lookup: (sha: string) => GitObject | undefined,
): Set<string> {
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
      stack.push(c.tree);
      if (!shallow.has(sha)) stack.push(...c.parents);
    } else if (obj.type === 'tree') {
      for (const e of parseTree(obj.data)) stack.push(e.oid);
    } else if (obj.type === 'tag') {
      const m = obj.data.toString('utf8').match(/^object ([0-9a-fA-F]{40})/m);
      if (m) stack.push(m[1]);
    }
  }
  return seen;
}

function selectObjects(repo: Repo, wants: string[], haves: string[], clientShallow: string[], deepen?: number): Selection {
  const lookup = (sha: string) => repo.objects.get(sha);
  const hasDeepen = deepen !== undefined && Number.isFinite(deepen) && deepen >= 1;
  const exclude = hasDeepen
    ? collectReachableWithShallow(haves, new Set(clientShallow), lookup)
    : collectReachable(haves, lookup);
  const newShallow: string[] = [];
  const included = new Set<string>();

  if (!hasDeepen) {
    for (const oid of collectReachable(wants, lookup)) {
      if (!exclude.has(oid)) included.add(oid);
    }
  } else {
    const commitTips: string[] = [];
    for (const w of wants) {
      const obj = lookup(w);
      if (!obj) continue;
      if (obj.type === 'tag') {
        included.add(w);
        const p = peel(repo, w);
        if (p) commitTips.push(p);
      } else if (obj.type === 'commit') {
        commitTips.push(w);
      } else {
        included.add(w);
      }
    }
    const distance = new Map<string, number>();
    const queue: { oid: string; d: number }[] = commitTips.map((oid) => ({ oid, d: 0 }));
    let qi = 0;
    while (qi < queue.length) {
      const { oid, d } = queue[qi++];
      if (distance.has(oid)) continue;
      const obj = lookup(oid);
      if (!obj || obj.type !== 'commit') continue;
      distance.set(oid, d);
      if (d + 1 >= deepen) continue;
      for (const parent of parseCommit(obj.data).parents) queue.push({ oid: parent, d: d + 1 });
    }
    for (const [oid, d] of distance.entries()) {
      const obj = lookup(oid)!;
      const c = parseCommit(obj.data);
      included.add(oid);
      for (const treeOid of collectReachable([c.tree], lookup)) included.add(treeOid);
      if (d === deepen - 1 && c.parents.length > 0) newShallow.push(oid);
    }
    for (const oid of exclude) included.delete(oid);
  }

  const unshallow = clientShallow.filter((s) => !newShallow.includes(s));
  return {
    oids: Array.from(included).sort(),
    newShallow: newShallow.sort(),
    unshallow: Array.from(new Set(unshallow)),
  };
}

function typeCode(type: ObjectType): number {
  if (type === 'commit') return 1;
  if (type === 'tree') return 2;
  if (type === 'blob') return 3;
  return 4;
}

const TYPE_NAME: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function encodeTypeSize(code: number, size: number): Buffer {
  const bytes: number[] = [];
  let b = (code << 4) | (size & 0x0f);
  size = Math.floor(size / 16);
  while (size > 0) {
    bytes.push(b | 0x80);
    b = size & 0x7f;
    size = Math.floor(size / 128);
  }
  bytes.push(b);
  return Buffer.from(bytes);
}

function writePack(repo: Repo, oids: string[]): Buffer {
  const head = Buffer.alloc(12);
  head.write('PACK', 0, 'ascii');
  head.writeUInt32BE(2, 4);
  head.writeUInt32BE(oids.length, 8);
  const chunks: Buffer[] = [head];
  for (const oid of oids) {
    const obj = repo.objects.get(oid);
    if (!obj) continue;
    chunks.push(encodeTypeSize(typeCode(obj.type), obj.data.length));
    chunks.push(deflateSync(obj.data));
  }
  const hash = createHash('sha1');
  for (const chunk of chunks) hash.update(chunk);
  chunks.push(hash.digest());
  return Buffer.concat(chunks);
}

let cachedRepoRev = '';
const packCache = new Map<string, { pack: Buffer; framed: Buffer }>();
const uploadV2Cache = new Map<string, Buffer>();
const receiveAdvertCache = new Map<string, Buffer>();
const lsRefsCache = new Map<string, Buffer>();

function clearCaches(repo: Repo): string {
  const repoRev = `${repo.id}:${repo.rev}`;
  if (cachedRepoRev !== repoRev) {
    packCache.clear();
    uploadV2Cache.clear();
    receiveAdvertCache.clear();
    lsRefsCache.clear();
    cachedRepoRev = repoRev;
  }
  return repoRev;
}

function getPack(repo: Repo, wants: string[], haves: string[], deepen: number | undefined, oids: string[]): { pack: Buffer; framed: Buffer } {
  const repoRev = clearCaches(repo);
  const key = `${repoRev}|${[...wants].sort().join(',')}|${[...haves].sort().join(',')}|${deepen ?? ''}|${oids.join(',')}`;
  const hit = packCache.get(key);
  if (hit) return hit;
  const pack = writePack(repo, oids);
  const entry = { pack, framed: sidebandData(pack) };
  if (packCache.size >= 16) packCache.clear();
  packCache.set(key, entry);
  return entry;
}

function sidebandData(pack: Buffer): Buffer {
  const out: Buffer[] = [];
  for (let off = 0; off < pack.length; off += SIDEBAND_CHUNK) {
    const chunk = pack.subarray(off, Math.min(off + SIDEBAND_CHUNK, pack.length));
    out.push(pkt(Buffer.concat([Buffer.from([1]), chunk])));
  }
  return Buffer.concat(out);
}

function lsRefs(repo: Repo, args: string[]): Buffer {
  const repoRev = clearCaches(repo);
  const key = `${repoRev}|${[...args].sort().join('|')}`;
  const hit = lsRefsCache.get(key);
  if (hit) return hit;
  const peelAsked = args.includes('peel');
  const symrefs = args.includes('symrefs');
  const unbornAsked = args.includes('unborn');
  const prefixes = args.filter((a) => a.startsWith('ref-prefix ')).map((a) => a.slice('ref-prefix '.length));
  const wanted = (name: string) => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  const headRef = defaultRef(repo);
  const tip = repo.refs.get(headRef);
  const out: Buffer[] = [];
  if (wanted('HEAD')) {
    if (unbornAsked && !tip) {
      out.push(text(`unborn HEAD symref-target:${headRef}`));
    } else if (tip) {
      out.push(text(`${tip} HEAD${symrefs ? ` symref-target:${headRef}` : ''}`));
    }
  }
  for (const ref of listRefs(repo)) {
    if (!wanted(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (peelAsked && ref.peeled) line += ` peeled:${ref.peeled}`;
    out.push(text(line));
  }
  out.push(flushPkt());
  const response = Buffer.concat(out);
  lsRefsCache.set(key, response);
  return response;
}

function readFetchArgs(args: string[]): { wants: string[]; haves: string[]; shallow: string[]; deepen?: number } {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let deepen: number | undefined;
  for (const arg of args) {
    if (arg.startsWith('want ')) wants.push(firstOid(arg.slice(5)));
    else if (arg.startsWith('have ')) haves.push(firstOid(arg.slice(5)));
    else if (arg.startsWith('shallow ')) shallow.push(firstOid(arg.slice(8)));
    else if (arg.startsWith('deepen ')) {
      const n = parseInt(arg.slice(7), 10);
      if (Number.isFinite(n)) deepen = n;
    }
  }
  return { wants, haves, shallow, deepen };
}

function fetchV2(repo: Repo, args: string[]): Buffer {
  const { wants, haves, shallow, deepen } = readFetchArgs(args);
  const sel = selectObjects(repo, wants, haves, shallow, deepen);
  const { framed } = getPack(repo, wants, haves, deepen, sel.oids);
  const out: Buffer[] = [];
  if (sel.newShallow.length > 0 || sel.unshallow.length > 0) {
    out.push(text('shallow-info'));
    for (const oid of sel.unshallow) out.push(text(`unshallow ${oid}`));
    for (const oid of sel.newShallow) out.push(text(`shallow ${oid}`));
    out.push(delimPkt());
  }
  out.push(text('packfile'));
  out.push(framed);
  out.push(flushPkt());
  return Buffer.concat(out);
}

function uploadPack(repo: Repo, body: Buffer, res: ServerResponse): void {
  const { packets } = decodePackets(body);
  const first = packets.find((p) => p.kind === 'data');
  if (first && first.payload.toString('utf8').startsWith('command=')) {
    let command = '';
    let inArgs = false;
    const args: string[] = [];
    for (const p of packets) {
      if (p.kind === 'delim') { inArgs = true; continue; }
      if (p.kind === 'flush' || p.kind === 'response-end') break;
      if (p.kind !== 'data') continue;
      const line = stripNl(p.payload.toString('utf8'));
      if (!inArgs) {
        if (line.startsWith('command=')) command = line.slice('command='.length);
      } else {
        args.push(line);
      }
    }
    if (command === 'ls-refs') {
      sendGit(res, 200, UPLOAD_RESULT, lsRefs(repo, args));
      return;
    }
    if (command === 'fetch') {
      sendGit(res, 200, UPLOAD_RESULT, fetchV2(repo, args));
      return;
    }
    sendGit(res, 400, 'text/plain', Buffer.from(`unsupported command: ${command}`));
    return;
  }

  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let deepen: number | undefined;
  let section = 0;
  for (const p of packets) {
    if (p.kind === 'flush') { section++; continue; }
    if (p.kind !== 'data') continue;
    const line = stripNl(p.payload.toString('utf8'));
    if (section === 0) {
      if (line.startsWith('want ')) wants.push(firstOid(line.slice(5)));
      else if (line.startsWith('shallow ')) shallow.push(firstOid(line.slice(8)));
      else if (line.startsWith('deepen ')) {
        const n = parseInt(line.slice(7), 10);
        if (Number.isFinite(n)) deepen = n;
      }
    } else if (section === 1 && line.startsWith('have ')) {
      haves.push(firstOid(line.slice(5)));
    }
  }
  const sel = selectObjects(repo, wants, haves, shallow, deepen);
  const { framed } = getPack(repo, wants, haves, deepen, sel.oids);
  sendGit(res, 200, UPLOAD_RESULT, Buffer.concat([text('NAK'), framed, flushPkt()]));
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    let b = 0;
    do {
      if (pos >= delta.length) throw new Error('truncated delta');
      b = delta[pos++];
      value += (b & 0x7f) * Math.pow(2, shift);
      shift += 7;
    } while (b & 0x80);
    return value;
  };
  const sourceSize = readVarint();
  const targetSize = readVarint();
  if (sourceSize !== base.length) throw new Error('delta source size mismatch');
  const pieces: Buffer[] = [];
  let produced = 0;
  while (pos < delta.length) {
    const op = delta[pos++];
    if (op & 0x80) {
      let offset = 0;
      let size = 0;
      let mul = 1;
      for (let i = 0; i < 4; i++) {
        if (op & (1 << i)) {
          if (pos >= delta.length) throw new Error('truncated delta');
          offset += delta[pos++] * mul;
        }
        mul *= 256;
      }
      mul = 1;
      for (let i = 0; i < 3; i++) {
        if (op & (0x10 << i)) {
          if (pos >= delta.length) throw new Error('truncated delta');
          size += delta[pos++] * mul;
        }
        mul *= 256;
      }
      if (size === 0) size = 0x10000;
      if (offset + size > base.length) throw new Error('delta copy out of range');
      pieces.push(base.subarray(offset, offset + size));
      produced += size;
    } else {
      if (op === 0) throw new Error('invalid delta opcode');
      if (pos + op > delta.length) throw new Error('truncated delta');
      pieces.push(delta.subarray(pos, pos + op));
      pos += op;
      produced += op;
    }
  }
  if (produced !== targetSize) throw new Error('delta target size mismatch');
  return Buffer.concat(pieces);
}

function readPack(repo: Repo, buf: Buffer): void {
  if (buf.length < 32 || buf.toString('ascii', 0, 4) !== 'PACK') throw new Error('bad pack signature');
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error('bad pack version');
  const count = buf.readUInt32BE(8);
  let pos = 12;
  const byOffset = new Map<number, { type: ObjectType; data: Buffer }>();
  for (let i = 0; i < count; i++) {
    const start = pos;
    if (pos >= buf.length) throw new Error('truncated pack');
    let b = buf[pos++];
    const type = (b >> 4) & 7;
    let size = b & 0x0f;
    let shift = 4;
    while (b & 0x80) {
      if (pos >= buf.length) throw new Error('truncated pack');
      b = buf[pos++];
      size |= (b & 0x7f) << shift;
      shift += 7;
    }
    let base: { type: ObjectType; data: Buffer } | undefined;
    if (type === 6) {
      if (pos >= buf.length) throw new Error('truncated pack');
      let c = buf[pos++];
      let off = c & 0x7f;
      while (c & 0x80) {
        if (pos >= buf.length) throw new Error('truncated pack');
        c = buf[pos++];
        off = ((off + 1) << 7) | (c & 0x7f);
      }
      base = byOffset.get(start - off);
      if (!base) throw new Error('bad ofs-delta base');
    } else if (type === 7) {
      if (pos + 20 > buf.length) throw new Error('truncated pack');
      const baseOid = buf.toString('hex', pos, pos + 20);
      pos += 20;
      base = repo.objects.get(baseOid);
      if (!base) throw new Error('bad ref-delta base');
    }
    if (pos >= buf.length) throw new Error('truncated pack');
    const r = inflateSync(buf.subarray(pos), { info: true });
    pos += r.engine.bytesWritten;
    const inflated = r.buffer;
    let obj: { type: ObjectType; data: Buffer };
    if (TYPE_NAME[type]) {
      obj = { type: TYPE_NAME[type], data: inflated };
    } else if (type === 6 || type === 7) {
      obj = { type: base!.type, data: applyDelta(base!.data, inflated) };
    } else {
      throw new Error('bad pack object type');
    }
    if (type >= 1 && type <= 4 && obj.data.length !== size) throw new Error('pack object size mismatch');
    putObject(repo, obj.type, obj.data);
    byOffset.set(start, obj);
  }
}

function isAncestor(repo: Repo, oldOid: string, newOid: string): boolean {
  const seen = new Set<string>();
  const stack = [newOid];
  let steps = 0;
  while (stack.length > 0 && steps++ < 200000) {
    const cur = stack.pop()!;
    if (cur === oldOid) return true;
    if (seen.has(cur)) continue;
    seen.add(cur);
    const obj = repo.objects.get(cur);
    if (!obj || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) stack.push(parent);
  }
  return false;
}

function wrapSideband(inner: Buffer, useSideband: boolean): Buffer {
  if (!useSideband) return inner;
  const out: Buffer[] = [];
  for (let off = 0; off < inner.length; off += SIDEBAND_CHUNK) {
    out.push(pkt(Buffer.concat([Buffer.from([1]), inner.subarray(off, off + SIDEBAND_CHUNK)])));
  }
  out.push(flushPkt());
  return Buffer.concat(out);
}

function sendReport(res: ServerResponse, lines: string[], useSideband: boolean): void {
  const inner: Buffer[] = lines.map(text);
  inner.push(flushPkt());
  sendGit(res, 200, RECEIVE_RESULT, wrapSideband(Buffer.concat(inner), useSideband));
}

interface PushCommand { oldOid: string; newOid: string; ref: string; caps: string }

function receivePack(repo: Repo, body: Buffer, res: ServerResponse): void {
  const { packets, consumedBytes } = decodePackets(body);
  const commands: PushCommand[] = [];
  for (const p of packets) {
    if (p.kind === 'flush') break;
    if (p.kind !== 'data') continue;
    const line = stripNl(p.payload.toString('utf8'));
    const nul = line.indexOf('\0');
    const head = nul >= 0 ? line.slice(0, nul) : line;
    const caps = nul >= 0 ? line.slice(nul + 1) : '';
    const parts = head.split(' ');
    if (parts.length < 3) continue;
    commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts.slice(2).join(' '), caps });
  }
  const useSideband = commands.some((c) => /(^|\s)side-band-64k(\s|$)/.test(c.caps));
  const packBuf = body.subarray(consumedBytes);
  const report: string[] = [];
  if (packBuf.length > 0) {
    try {
      readPack(repo, packBuf);
      report.push('unpack ok');
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      report.push(`unpack ${msg.replace(/[\r\n]+/g, ' ').slice(0, 100)}`);
      sendReport(res, report, useSideband);
      return;
    }
  } else {
    report.push('unpack ok');
  }

  for (const c of commands) {
    if (/^0+$/.test(c.newOid)) {
      if (repo.refs.has(c.ref)) {
        repo.refs.delete(c.ref);
        repo.rev++;
        report.push(`ok ${c.ref}`);
      } else {
        report.push(`ng ${c.ref} delete-current-branch`);
      }
      continue;
    }
    if (/^0+$/.test(c.oldOid)) {
      if (repo.refs.has(c.ref)) {
        report.push(`ng ${c.ref} already exists`);
        continue;
      }
      repo.refs.set(c.ref, c.newOid);
      repo.rev++;
      report.push(`ok ${c.ref}`);
      continue;
    }
    const current = repo.refs.get(c.ref);
    if (!current || current !== c.oldOid) {
      report.push(`ng ${c.ref} fetch first`);
      continue;
    }
    if (!c.ref.startsWith('refs/tags/') && !isAncestor(repo, c.oldOid, c.newOid)) {
      report.push(`ng ${c.ref} non-fast-forward`);
      continue;
    }
    repo.refs.set(c.ref, c.newOid);
    repo.rev++;
    report.push(`ok ${c.ref}`);
  }
  sendReport(res, report, useSideband);
}

export async function handleGitHttp(
  req: IncomingMessage,
  res: ServerResponse,
  method: string,
  segs: string[],
  body: Buffer,
  store: Store,
  url: URL,
): Promise<boolean> {
  if (segs.length < 2 || !segs[1].endsWith('.git')) return false;
  const owner = segs[0];
  const name = segs[1].slice(0, -4);
  const repo = store.getRepo(owner, name);
  if (!repo) {
    sendGit(res, 404, 'text/plain', Buffer.from('Not Found'));
    return true;
  }
  const rest = segs.slice(2).join('/');
  try {
    if (method === 'GET' && rest === 'info/refs') {
      const service = url.searchParams.get('service');
      if (service === 'git-upload-pack') {
        const v2 = String(req.headers['git-protocol'] ?? '').includes('version=2');
        sendGit(res, 200, UPLOAD_ADVERT, v2 ? uploadV2Advert(repo) : uploadV0Advert(repo));
        return true;
      }
      if (service === 'git-receive-pack') {
        sendGit(res, 200, RECEIVE_ADVERT, receiveAdvert(repo));
        return true;
      }
      sendGit(res, 404, 'text/plain', Buffer.from('Not Found'));
      return true;
    }
    if (method === 'POST' && rest === 'git-upload-pack') {
      uploadPack(repo, body, res);
      return true;
    }
    if (method === 'POST' && rest === 'git-receive-pack') {
      receivePack(repo, body, res);
      return true;
    }
    sendGit(res, 404, 'text/plain', Buffer.from('Not Found'));
    return true;
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    sendGit(res, 400, 'text/plain', Buffer.from(msg));
    return true;
  }
}
