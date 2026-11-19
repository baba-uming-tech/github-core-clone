package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	maxPktLine        = 65520
	sidebandChunkSize = 65000
	zeroOID           = "0000000000000000000000000000000000000000"
	uploadResultCT    = "application/x-git-upload-pack-result"
	receiveResultCT   = "application/x-git-receive-pack-result"
	uploadAdvertCT    = "application/x-git-upload-pack-advertisement"
	receiveAdvertCT   = "application/x-git-receive-pack-advertisement"
)

var (
	flushPktBytes = []byte("0000")
	delimPktBytes = []byte("0001")
)

const maxPackCacheEntries = 32

type packEntry struct {
	rev    string
	pack   []byte
	framed []byte
}

type responseCache struct {
	mu       sync.Mutex
	rev      string
	packs    map[string]packEntry
	order    []string
	uploadV2 []byte
	receive  []byte
	lsRefs   map[string][]byte
	lsOrder  []string
}

var gitCache = &responseCache{
	packs:  make(map[string]packEntry),
	lsRefs: make(map[string][]byte),
}

var zlibWriterPool = sync.Pool{New: func() any {
	zw, err := zlib.NewWriterLevel(&bytes.Buffer{}, zlib.BestSpeed)
	if err != nil {
		panic(err)
	}
	return zw
}}

var zlibBufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func repoRev(repo *Repo) string {
	return strconv.Itoa(repo.ID) + ":" + strconv.Itoa(repo.Rev)
}

func (c *responseCache) resetLocked(rev string) {
	c.rev = rev
	c.packs = make(map[string]packEntry)
	c.order = c.order[:0]
	c.uploadV2 = nil
	c.receive = nil
	c.lsRefs = make(map[string][]byte)
	c.lsOrder = c.lsOrder[:0]
}

func (c *responseCache) syncLocked(rev string) {
	if c.rev != rev {
		c.resetLocked(rev)
	}
}

func appendPktLen(dst []byte, n int) {
	const digits = "0123456789abcdef"
	dst[0] = digits[(n>>12)&0xf]
	dst[1] = digits[(n>>8)&0xf]
	dst[2] = digits[(n>>4)&0xf]
	dst[3] = digits[n&0xf]
}

func PKT(text string) []byte {
	n := len(text) + 4
	if n > maxPktLine {
		panic("pkt-line too large")
	}
	out := make([]byte, n)
	appendPktLen(out, n)
	copy(out[4:], text)
	return out
}

func pktBytes(b []byte) []byte {
	n := len(b) + 4
	if n > maxPktLine {
		panic("pkt-line too large")
	}
	out := make([]byte, n)
	appendPktLen(out, n)
	copy(out[4:], b)
	return out
}

func writePktLine(out *bytes.Buffer, s string) {
	n := len(s) + 5
	if n > maxPktLine {
		panic("pkt-line too large")
	}
	var hdr [4]byte
	appendPktLen(hdr[:], n)
	out.Write(hdr[:])
	out.WriteString(s)
	out.WriteByte('\n')
}

func writePktLineParts(out *bytes.Buffer, parts ...string) {
	n := 5
	for _, p := range parts {
		n += len(p)
	}
	if n > maxPktLine {
		panic("pkt-line too large")
	}
	var hdr [4]byte
	appendPktLen(hdr[:], n)
	out.Write(hdr[:])
	for _, p := range parts {
		out.WriteString(p)
	}
	out.WriteByte('\n')
}

func writePktBytes(out *bytes.Buffer, b []byte) {
	n := len(b) + 4
	if n > maxPktLine {
		panic("pkt-line too large")
	}
	var hdr [4]byte
	appendPktLen(hdr[:], n)
	out.Write(hdr[:])
	out.Write(b)
}

func FlushPkt() []byte { return flushPktBytes }
func DelimPkt() []byte { return delimPktBytes }

type PacketKind int

const (
	pktData PacketKind = iota
	pktFlush
	pktDelim
	pktResponseEnd
)

type Packet struct {
	Kind    PacketKind
	Payload []byte
}

type DecodedPackets struct {
	Packets       []Packet
	ConsumedBytes int
}

func DecodePackets(buf []byte) DecodedPackets {
	var packets []Packet
	pos := 0
	for pos+4 <= len(buf) {
		hx := string(buf[pos : pos+4])
		if len(hx) != 4 || !isHex4(hx) {
			panic("malformed pkt-line")
		}
		l, _ := strconv.ParseUint(hx, 16, 16)
		length := int(l)
		pos += 4
		if length == 0 {
			packets = append(packets, Packet{Kind: pktFlush})
			break
		}
		if length == 1 {
			packets = append(packets, Packet{Kind: pktDelim})
			continue
		}
		if length == 2 {
			packets = append(packets, Packet{Kind: pktResponseEnd})
			continue
		}
		if length < 4 || pos+length-4 > len(buf) {
			panic("malformed pkt-line length")
		}
		pay := make([]byte, length-4)
		copy(pay, buf[pos:pos+length-4])
		packets = append(packets, Packet{Kind: pktData, Payload: pay})
		pos += length - 4
	}
	return DecodedPackets{Packets: packets, ConsumedBytes: pos}
}

func isHex4(s string) bool {
	for i := 0; i < 4; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func pktLineText(s string) []byte {
	n := len(s) + 5
	if n > maxPktLine {
		panic("pkt-line too large")
	}
	out := make([]byte, n)
	appendPktLen(out, n)
	copy(out[4:], s)
	out[n-1] = '\n'
	return out
}

func stripNL(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s[:len(s)-1]
	}
	return s
}

func firstOID(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func (a *App) sendGit(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-GitHub-Request-Id", a.nextRequestID())
	w.WriteHeader(status)
	if body != nil && status != http.StatusNoContent {
		_, _ = w.Write(body)
	}
}

type RefInfo struct {
	Name   string
	OID    string
	Peeled string
}

func listRefs(repo *Repo) []RefInfo {
	var heads, tags []RefInfo
	for name, oid := range repo.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			heads = append(heads, RefInfo{Name: name, OID: oid})
		} else if strings.HasPrefix(name, "refs/tags/") {
			tags = append(tags, RefInfo{Name: name, OID: oid, Peeled: peelTag(repo, oid)})
		}
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].Name < heads[j].Name })
	sort.Slice(tags, func(i, j int) bool { return tags[i].Name < tags[j].Name })
	return append(heads, tags...)
}

func peelTag(repo *Repo, oid string) string {
	cur := oid
	seen := map[string]bool{}
	for {
		obj := repo.GetObject(cur)
		if obj == nil {
			return ""
		}
		if obj.Type != ObjectTypeTag {
			return cur
		}
		if seen[cur] {
			return ""
		}
		seen[cur] = true
		m := tagObjectPattern.FindSubmatch(obj.Data)
		if len(m) < 2 {
			return ""
		}
		cur = string(m[1])
	}
}

func defaultRefName(repo *Repo) string {
	return "refs/heads/" + defaultBranch(repo)
}

func uploadCaps(repo *Repo) string {
	return "symref=HEAD:" + defaultRefName(repo) + " side-band-64k thin-pack ofs-delta shallow no-progress multi_ack_detailed agent=github-core-clone/1.0"
}

const receiveCaps = "report-status side-band-64k delete-refs ofs-delta agent=github-core-clone/1.0"

func uploadV2Advert(repo *Repo) []byte {
	rev := repoRev(repo)
	gitCache.mu.Lock()
	defer gitCache.mu.Unlock()
	gitCache.syncLocked(rev)
	if gitCache.uploadV2 != nil {
		return gitCache.uploadV2
	}
	var out bytes.Buffer
	out.Grow(160)
	writePktLine(&out, "version 2")
	writePktLine(&out, "agent=github-core-clone/1.0")
	writePktLine(&out, "ls-refs=unborn")
	writePktLine(&out, "fetch=shallow wait-for-done")
	writePktLine(&out, "server-option")
	writePktLine(&out, "object-format=sha1")
	out.Write(FlushPkt())
	gitCache.uploadV2 = out.Bytes()
	return gitCache.uploadV2
}

func uploadV0Advert(repo *Repo) []byte {
	refs := listRefs(repo)
	var out bytes.Buffer
	out.Write(pktLineText("# service=git-upload-pack"))
	out.Write(FlushPkt())
	if len(refs) == 0 {
		out.Write(pktLineText(zeroOID + " capabilities^{}\x00" + uploadCaps(repo)))
	} else {
		headOID, ok := repo.Refs[defaultRefName(repo)]
		if !ok {
			headOID = refs[0].OID
		}
		out.Write(pktLineText(headOID + " HEAD\x00" + uploadCaps(repo)))
		for _, r := range refs {
			out.Write(pktLineText(r.OID + " " + r.Name))
			if r.Peeled != "" {
				out.Write(pktLineText(r.Peeled + " " + r.Name + "^{}"))
			}
		}
	}
	out.Write(FlushPkt())
	return out.Bytes()
}

func receiveAdvert(repo *Repo) []byte {
	rev := repoRev(repo)
	gitCache.mu.Lock()
	defer gitCache.mu.Unlock()
	gitCache.syncLocked(rev)
	if gitCache.receive != nil {
		return gitCache.receive
	}
	refs := listRefs(repo)
	var out bytes.Buffer
	out.Grow(64 + len(refs)*72)
	writePktLine(&out, "# service=git-receive-pack")
	out.Write(FlushPkt())
	if len(refs) == 0 {
		writePktLine(&out, zeroOID+" capabilities^{}\x00"+receiveCaps)
	} else {
		for i, r := range refs {
			if i == 0 {
				writePktLine(&out, r.OID+" "+r.Name+"\x00"+receiveCaps)
			} else {
				writePktLine(&out, r.OID+" "+r.Name)
			}
		}
	}
	out.Write(FlushPkt())
	gitCache.receive = out.Bytes()
	return gitCache.receive
}

func collectReachableWithShallow(tips []string, shallow map[string]bool, lookup func(string) *GitObject) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string(nil), tips...)
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if sha == "" || seen[sha] {
			continue
		}
		obj := lookup(sha)
		if obj == nil {
			continue
		}
		seen[sha] = true
		switch obj.Type {
		case ObjectTypeCommit:
			c := ParseCommit(obj.Data)
			stack = append(stack, c.Tree)
			if !shallow[sha] {
				stack = append(stack, c.Parents...)
			}
		case ObjectTypeTree:
			for _, e := range ParseTree(obj.Data) {
				stack = append(stack, e.SHA)
			}
		case ObjectTypeTag:
			if o := TagObjectSHA(obj.Data); o != "" {
				stack = append(stack, o)
			}
		}
	}
	return seen
}

type Selection struct {
	OIDs       []string
	NewShallow []string
	Unshallow  []string
}

func selectObjects(repo *Repo, wants, haves, clientShallow []string, deepen *int) Selection {
	lookup := func(sha string) *GitObject { return repo.GetObject(sha) }
	hasDeepen := deepen != nil && *deepen >= 1
	var exclude map[string]bool
	if hasDeepen {
		cs := map[string]bool{}
		for _, s := range clientShallow {
			cs[s] = true
		}
		exclude = collectReachableWithShallow(haves, cs, lookup)
	} else {
		exclude = CollectReachable(haves, lookup)
	}
	included := map[string]bool{}
	newShallow := []string{}

	if !hasDeepen {
		for oid := range CollectReachable(wants, lookup) {
			if !exclude[oid] {
				included[oid] = true
			}
		}
	} else {
		d := *deepen
		var commitTips []string
		for _, w := range wants {
			obj := lookup(w)
			if obj == nil {
				continue
			}
			switch obj.Type {
			case ObjectTypeTag:
				included[w] = true
				p := peelTag(repo, w)
				if p != "" {
					commitTips = append(commitTips, p)
				}
			case ObjectTypeCommit:
				commitTips = append(commitTips, w)
			default:
				included[w] = true
			}
		}
		distance := map[string]int{}
		type qitem struct {
			oid string
			d   int
		}
		queue := make([]qitem, 0, len(commitTips))
		for _, oid := range commitTips {
			queue = append(queue, qitem{oid: oid, d: 0})
		}
		for qi := 0; qi < len(queue); qi++ {
			cur := queue[qi]
			if _, ok := distance[cur.oid]; ok {
				continue
			}
			obj := lookup(cur.oid)
			if obj == nil || obj.Type != ObjectTypeCommit {
				continue
			}
			distance[cur.oid] = cur.d
			if cur.d+1 >= d {
				continue
			}
			c := ParseCommit(obj.Data)
			for _, p := range c.Parents {
				queue = append(queue, qitem{oid: p, d: cur.d + 1})
			}
		}
		for oid, dd := range distance {
			obj := lookup(oid)
			if obj == nil {
				continue
			}
			c := ParseCommit(obj.Data)
			included[oid] = true
			for t := range CollectReachable([]string{c.Tree}, lookup) {
				included[t] = true
			}
			if dd == d-1 && len(c.Parents) > 0 {
				newShallow = append(newShallow, oid)
			}
		}
		for oid := range exclude {
			delete(included, oid)
		}
	}

	newSet := map[string]bool{}
	for _, s := range newShallow {
		newSet[s] = true
	}
	unshallow := []string{}
	seenU := map[string]bool{}
	for _, s := range clientShallow {
		if !newSet[s] && !seenU[s] {
			unshallow = append(unshallow, s)
			seenU[s] = true
		}
	}
	oids := make([]string, 0, len(included))
	for oid := range included {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	sort.Strings(newShallow)
	return Selection{OIDs: oids, NewShallow: newShallow, Unshallow: unshallow}
}

func typeCode(t ObjectType) int {
	switch t {
	case ObjectTypeCommit:
		return 1
	case ObjectTypeTree:
		return 2
	case ObjectTypeBlob:
		return 3
	case ObjectTypeTag:
		return 4
	}
	return 0
}

func encodeTypeSize(code, size int) []byte {
	var out []byte
	b := byte((code << 4) | (size & 0x0f))
	size >>= 4
	for size > 0 {
		out = append(out, b|0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, b)
	return out
}

func writeTypeSize(out *bytes.Buffer, code, size int) {
	var enc [10]byte
	b := byte((code << 4) | (size & 0x0f))
	size >>= 4
	n := 0
	for size > 0 {
		enc[n] = b | 0x80
		n++
		b = byte(size & 0x7f)
		size >>= 7
	}
	enc[n] = b
	out.Write(enc[:n+1])
}

func writePack(repo *Repo, oids []string) []byte {
	var head [12]byte
	copy(head[:4], "PACK")
	head[7] = 2
	n := uint32(len(oids))
	head[8] = byte(n >> 24)
	head[9] = byte(n >> 16)
	head[10] = byte(n >> 8)
	head[11] = byte(n)
	var pack bytes.Buffer
	pack.Grow(12 + len(oids)*64 + sha1.Size)
	pack.Write(head[:])
	for _, oid := range oids {
		obj := repo.GetObject(oid)
		if obj == nil {
			continue
		}
		writeTypeSize(&pack, typeCode(obj.Type), len(obj.Data))
		zb := zlibBufferPool.Get().(*bytes.Buffer)
		zb.Reset()
		zw := zlibWriterPool.Get().(*zlib.Writer)
		zw.Reset(zb)
		_, _ = zw.Write(obj.Data)
		_ = zw.Flush()
		_ = zw.Close()
		pack.Write(zb.Bytes())
		zw.Reset(io.Discard)
		zlibBufferPool.Put(zb)
		zlibWriterPool.Put(zw)
	}
	sum := sha1.Sum(pack.Bytes())
	pack.Write(sum[:])
	return pack.Bytes()
}

func sidebandData(pack []byte) []byte {
	var out bytes.Buffer
	chunks := (len(pack) + sidebandChunkSize - 1) / sidebandChunkSize
	out.Grow(len(pack) + chunks*8 + 8)
	for off := 0; off < len(pack); off += sidebandChunkSize {
		end := off + sidebandChunkSize
		if end > len(pack) {
			end = len(pack)
		}
		var hdr [5]byte
		appendPktLen(hdr[:4], 5+end-off)
		hdr[4] = 1
		out.Write(hdr[:])
		out.Write(pack[off:end])
	}
	return out.Bytes()
}

func lsRefsCacheKey(rev string, args []string) string {
	var key bytes.Buffer
	key.Grow(len(rev) + 16 + len(args)*16)
	key.WriteString(rev)
	for _, a := range args {
		key.WriteByte('|')
		key.WriteString(strconv.Itoa(len(a)))
		key.WriteByte(':')
		key.WriteString(a)
	}
	return key.String()
}

func buildLsRefs(repo *Repo, args []string) []byte {
	peelAsked, symrefs, unbornAsked := false, false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peelAsked = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unbornAsked = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-prefix "))
		}
	}
	wanted := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	headRef := defaultRefName(repo)
	tip, hasTip := repo.Refs[headRef]
	refs := listRefs(repo)
	var out bytes.Buffer
	out.Grow(64 + len(refs)*72)
	if wanted("HEAD") {
		if unbornAsked && !hasTip {
			writePktLineParts(&out, "unborn HEAD symref-target:", headRef)
		} else if hasTip {
			if symrefs {
				writePktLineParts(&out, tip, " HEAD symref-target:", headRef)
			} else {
				writePktLineParts(&out, tip, " HEAD")
			}
		}
	}
	for _, ref := range refs {
		if !wanted(ref.Name) {
			continue
		}
		if peelAsked && ref.Peeled != "" {
			writePktLineParts(&out, ref.OID, " ", ref.Name, " peeled:", ref.Peeled)
		} else {
			writePktLineParts(&out, ref.OID, " ", ref.Name)
		}
	}
	out.Write(FlushPkt())
	return out.Bytes()
}

func lsRefs(repo *Repo, args []string) []byte {
	rev := repoRev(repo)
	key := lsRefsCacheKey(rev, args)
	gitCache.mu.Lock()
	defer gitCache.mu.Unlock()
	gitCache.syncLocked(rev)
	if out, ok := gitCache.lsRefs[key]; ok {
		return out
	}
	out := buildLsRefs(repo, args)
	if len(gitCache.lsRefs) >= maxPackCacheEntries {
		gitCache.lsRefs = make(map[string][]byte)
		gitCache.lsOrder = gitCache.lsOrder[:0]
	}
	gitCache.lsRefs[key] = out
	gitCache.lsOrder = append(gitCache.lsOrder, key)
	return out
}

func readFetchArgs(args []string) (wants, haves, shallow []string, deepen *int) {
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			wants = append(wants, firstOID(arg[5:]))
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, firstOID(arg[5:]))
		case strings.HasPrefix(arg, "shallow "):
			shallow = append(shallow, firstOID(arg[8:]))
		case strings.HasPrefix(arg, "deepen "):
			n, err := strconv.Atoi(strings.TrimSpace(arg[7:]))
			if err == nil {
				deepen = &n
			}
		}
	}
	return
}

func fetchV2(repo *Repo, args []string) []byte {
	wants, haves, shallow, deepen := readFetchArgs(args)
	sel := selectObjects(repo, wants, haves, shallow, deepen)
	pack := writePack(repo, sel.OIDs)
	framed := sidebandData(pack)
	var out bytes.Buffer
	if len(sel.NewShallow) > 0 || len(sel.Unshallow) > 0 {
		out.Write(pktLineText("shallow-info"))
		for _, oid := range sel.Unshallow {
			out.Write(pktLineText("unshallow " + oid))
		}
		for _, oid := range sel.NewShallow {
			out.Write(pktLineText("shallow " + oid))
		}
		out.Write(DelimPkt())
	}
	out.Write(pktLineText("packfile"))
	out.Write(framed)
	out.Write(FlushPkt())
	return out.Bytes()
}

func uploadPack(repo *Repo, body []byte) []byte {
	decoded := DecodePackets(body)
	packets := decoded.Packets
	var first *Packet
	for i := range packets {
		if packets[i].Kind == pktData {
			first = &packets[i]
			break
		}
	}
	if first != nil && strings.HasPrefix(string(first.Payload), "command=") {
		command := ""
		inArgs := false
		var args []string
		for _, p := range packets {
			if p.Kind == pktDelim {
				inArgs = true
				continue
			}
			if p.Kind == pktFlush || p.Kind == pktResponseEnd {
				break
			}
			if p.Kind != pktData {
				continue
			}
			line := stripNL(string(p.Payload))
			if !inArgs {
				if strings.HasPrefix(line, "command=") {
					command = strings.TrimPrefix(line, "command=")
				}
			} else {
				args = append(args, line)
			}
		}
		switch command {
		case "ls-refs":
			return lsRefs(repo, args)
		case "fetch":
			return fetchV2(repo, args)
		default:
			return nil
		}
	}
	// v0
	var wants, haves, shallow []string
	var deepen *int
	section := 0
	for _, p := range packets {
		if p.Kind == pktFlush {
			section++
			continue
		}
		if p.Kind != pktData {
			continue
		}
		line := stripNL(string(p.Payload))
		if section == 0 {
			switch {
			case strings.HasPrefix(line, "want "):
				wants = append(wants, firstOID(line[5:]))
			case strings.HasPrefix(line, "shallow "):
				shallow = append(shallow, firstOID(line[8:]))
			case strings.HasPrefix(line, "deepen "):
				n, err := strconv.Atoi(strings.TrimSpace(line[7:]))
				if err == nil {
					deepen = &n
				}
			}
		} else if section == 1 && strings.HasPrefix(line, "have ") {
			haves = append(haves, firstOID(line[5:]))
		}
	}
	sel := selectObjects(repo, wants, haves, shallow, deepen)
	pack := writePack(repo, sel.OIDs)
	framed := sidebandData(pack)
	var out bytes.Buffer
	out.Write(pktLineText("NAK"))
	out.Write(framed)
	out.Write(FlushPkt())
	return out.Bytes()
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		value := 0
		shift := 0
		for {
			if pos >= len(delta) {
				return 0, errors.New("truncated delta")
			}
			b := delta[pos]
			pos++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta source size mismatch")
	}
	out := make([]byte, 0, tgtSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			offset := 0
			size := 0
			if op&0x01 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				offset |= int(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				offset |= int(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				offset |= int(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				offset |= int(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				size |= int(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				size |= int(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				size |= int(delta[pos]) << 16
				pos++
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[offset:offset+size]...)
		} else {
			if op == 0 {
				return nil, errors.New("invalid delta opcode")
			}
			n := int(op)
			if pos+n > len(delta) {
				return nil, errors.New("truncated delta")
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		}
	}
	if len(out) != tgtSize {
		return nil, errors.New("delta target size mismatch")
	}
	return out, nil
}

func readPack(repo *Repo, buf []byte) error {
	if len(buf) < 32 || string(buf[0:4]) != "PACK" {
		return errors.New("bad pack signature")
	}
	version := int(buf[4])<<24 | int(buf[5])<<16 | int(buf[6])<<8 | int(buf[7])
	if version != 2 && version != 3 {
		return errors.New("bad pack version")
	}
	count := int(buf[8])<<24 | int(buf[9])<<16 | int(buf[10])<<8 | int(buf[11])
	pos := 12
	// verify trailer sha1 if present
	if len(buf) >= 32 {
		payload := buf[:len(buf)-20]
		trailer := buf[len(buf)-20:]
		sum := sha1.Sum(payload)
		if bytes.Equal(sum[:], trailer) {
			// ok, strip not necessary as we read by count
		}
		// if mismatch, still try? TS verifies in write but read doesn't explicitly check except maybe not stated; better be permissive or check. TS readPack doesn't check trailer. skip check.
	}
	type objEntry struct {
		Type ObjectType
		Data []byte
	}
	byOffset := map[int]objEntry{}
	for i := 0; i < count; i++ {
		start := pos
		if pos >= len(buf) {
			return errors.New("truncated pack")
		}
		b := buf[pos]
		pos++
		typ := int((b >> 4) & 7)
		size := int(b & 0x0f)
		shift := 4
		for b&0x80 != 0 {
			if pos >= len(buf) {
				return errors.New("truncated pack")
			}
			b = buf[pos]
			pos++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		var base *objEntry
		var baseType ObjectType
		switch typ {
		case 6: // ofs-delta
			if pos >= len(buf) {
				return errors.New("truncated pack")
			}
			c := buf[pos]
			pos++
			off := int(c & 0x7f)
			for c&0x80 != 0 {
				if pos >= len(buf) {
					return errors.New("truncated pack")
				}
				c = buf[pos]
				pos++
				off = ((off + 1) << 7) | int(c&0x7f)
			}
			baseOff := start - off
			if be, ok := byOffset[baseOff]; ok {
				base = &be
			} else {
				return errors.New("bad ofs-delta base")
			}
		case 7: // ref-delta
			if pos+20 > len(buf) {
				return errors.New("truncated pack")
			}
			baseOID := hex.EncodeToString(buf[pos : pos+20])
			pos += 20
			bo := repo.GetObject(baseOID)
			if bo == nil {
				return errors.New("bad ref-delta base")
			}
			base = &objEntry{Type: bo.Type, Data: bo.Data}
		}
		if pos >= len(buf) {
			return errors.New("truncated pack")
		}
		zr, err := zlib.NewReader(bytes.NewReader(buf[pos:]))
		if err != nil {
			return err
		}
		inflated, err := io.ReadAll(zr)
		consumed := 0
		// zlib reader doesn't expose consumed easily; use a counting reader
		_ = zr.Close()
		// recompute with counting reader
		cr := &countReader{r: bytes.NewReader(buf[pos:])}
		zr2, err := zlib.NewReader(cr)
		if err != nil {
			return err
		}
		inflated, err = io.ReadAll(zr2)
		if err != nil {
			_ = zr2.Close()
			return err
		}
		_ = zr2.Close()
		consumed = cr.n
		pos += consumed
		var obj objEntry
		switch typ {
		case 1:
			obj = objEntry{Type: ObjectTypeCommit, Data: inflated}
		case 2:
			obj = objEntry{Type: ObjectTypeTree, Data: inflated}
		case 3:
			obj = objEntry{Type: ObjectTypeBlob, Data: inflated}
		case 4:
			obj = objEntry{Type: ObjectTypeTag, Data: inflated}
		case 6, 7:
			if base == nil {
				return errors.New("missing delta base")
			}
			data, err := applyDelta(base.Data, inflated)
			if err != nil {
				return err
			}
			obj = objEntry{Type: base.Type, Data: data}
			_ = baseType
		default:
			return errors.New("bad pack object type")
		}
		if typ >= 1 && typ <= 4 && len(obj.Data) != size {
			return errors.New("pack object size mismatch")
		}
		repo.PutObject(obj.Type, obj.Data)
		byOffset[start] = obj
	}
	_ = count
	return nil
}

type countReader struct {
	r *bytes.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// ReadByte lets compress/flate read the stream unbuffered, so n counts exactly the
// bytes the compressed object occupies in the pack.
func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

func isAncestorRepo(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	steps := 0
	for len(stack) > 0 && steps < 200000 {
		steps++
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == ancestor {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := repo.GetObject(cur)
		if obj == nil || obj.Type != ObjectTypeCommit {
			continue
		}
		c := ParseCommit(obj.Data)
		for _, p := range c.Parents {
			stack = append(stack, p)
		}
	}
	return false
}

func isAllZeroOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

func wrapSideband(inner []byte, useSideband bool) []byte {
	if !useSideband {
		return inner
	}
	var out bytes.Buffer
	for off := 0; off < len(inner); off += sidebandChunkSize {
		end := off + sidebandChunkSize
		if end > len(inner) {
			end = len(inner)
		}
		chunk := inner[off:end]
		payload := make([]byte, 1+len(chunk))
		payload[0] = 1
		copy(payload[1:], chunk)
		out.Write(pktBytes(payload))
	}
	out.Write(FlushPkt())
	return out.Bytes()
}

func receivePack(repo *Repo, body []byte) []byte {
	decoded := DecodePackets(body)
	packets := decoded.Packets
	consumed := decoded.ConsumedBytes
	type pushCmd struct {
		oldOID, newOID, ref, caps string
	}
	var commands []pushCmd
	for _, p := range packets {
		if p.Kind == pktFlush {
			break
		}
		if p.Kind != pktData {
			continue
		}
		line := stripNL(string(p.Payload))
		nul := strings.IndexByte(line, 0)
		head := line
		caps := ""
		if nul >= 0 {
			head = line[:nul]
			caps = line[nul+1:]
		}
		parts := strings.SplitN(head, " ", 3)
		if len(parts) < 3 {
			continue
		}
		commands = append(commands, pushCmd{oldOID: parts[0], newOID: parts[1], ref: parts[2], caps: caps})
	}
	useSideband := false
	for _, c := range commands {
		if strings.Contains(" "+c.caps+" ", " side-band-64k ") || strings.HasPrefix(c.caps, "side-band-64k ") || strings.HasSuffix(c.caps, " side-band-64k") || c.caps == "side-band-64k" {
			useSideband = true
			break
		}
	}
	packBuf := []byte{}
	if consumed < len(body) {
		packBuf = body[consumed:]
	}
	report := []string{}
	if len(packBuf) > 0 {
		err := readPack(repo, packBuf)
		if err != nil {
			msg := err.Error()
			msg = strings.ReplaceAll(msg, "\r", " ")
			msg = strings.ReplaceAll(msg, "\n", " ")
			if len(msg) > 100 {
				msg = msg[:100]
			}
			report = append(report, "unpack "+msg)
			var inner bytes.Buffer
			for _, l := range report {
				inner.Write(pktLineText(l))
			}
			inner.Write(FlushPkt())
			return wrapSideband(inner.Bytes(), useSideband)
		}
		report = append(report, "unpack ok")
	} else {
		report = append(report, "unpack ok")
	}

	for _, c := range commands {
		if isAllZeroOID(c.newOID) {
			if _, ok := repo.Refs[c.ref]; ok {
				delete(repo.Refs, c.ref)
				repo.Rev++
				report = append(report, "ok "+c.ref)
			} else {
				report = append(report, "ng "+c.ref+" delete-current-branch")
			}
			continue
		}
		if isAllZeroOID(c.oldOID) {
			if _, ok := repo.Refs[c.ref]; ok {
				report = append(report, "ng "+c.ref+" already exists")
				continue
			}
			repo.Refs[c.ref] = c.newOID
			repo.Rev++
			report = append(report, "ok "+c.ref)
			continue
		}
		current, ok := repo.Refs[c.ref]
		if !ok || current != c.oldOID {
			report = append(report, "ng "+c.ref+" fetch first")
			continue
		}
		if !strings.HasPrefix(c.ref, "refs/tags/") && !isAncestorRepo(repo, c.oldOID, c.newOID) {
			report = append(report, "ng "+c.ref+" non-fast-forward")
			continue
		}
		repo.Refs[c.ref] = c.newOID
		repo.Rev++
		report = append(report, "ok "+c.ref)
	}
	var inner bytes.Buffer
	for _, l := range report {
		inner.Write(pktLineText(l))
	}
	inner.Write(FlushPkt())
	return wrapSideband(inner.Bytes(), useSideband)
}

func (a *App) handleGitHTTP(w http.ResponseWriter, r *http.Request, method string, segs []string, body []byte, rawURL string) {
	if len(segs) < 2 || !strings.HasSuffix(segs[1], ".git") {
		a.sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	owner := segs[0]
	name := strings.TrimSuffix(segs[1], ".git")
	repo := a.Store.GetRepo(owner, name)
	if repo == nil {
		a.sendGit(w, http.StatusNotFound, "text/plain", []byte("Not Found"))
		return
	}
	rest := ""
	if len(segs) > 2 {
		rest = strings.Join(segs[2:], "/")
	}
	defer func() {
		if rec := recover(); rec != nil {
			msg := fmt.Sprint(rec)
			a.sendGit(w, http.StatusBadRequest, "text/plain", []byte(msg))
		}
	}()

	// parse query
	query := ""
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		query = rawURL[i+1:]
	}

	if method == http.MethodGet && rest == "info/refs" {
		service := queryParam(query, "service")
		if service == "git-upload-pack" {
			v2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
			if v2 {
				a.sendGit(w, http.StatusOK, uploadAdvertCT, uploadV2Advert(repo))
			} else {
				a.sendGit(w, http.StatusOK, uploadAdvertCT, uploadV0Advert(repo))
			}
			return
		}
		if service == "git-receive-pack" {
			a.sendGit(w, http.StatusOK, receiveAdvertCT, receiveAdvert(repo))
			return
		}
		a.sendGit(w, http.StatusNotFound, "text/plain", []byte("Not Found"))
		return
	}
	if method == http.MethodPost && rest == "git-upload-pack" {
		out := uploadPack(repo, body)
		if out == nil {
			a.sendGit(w, http.StatusBadRequest, "text/plain", []byte("unsupported command"))
			return
		}
		a.sendGit(w, http.StatusOK, uploadResultCT, out)
		return
	}
	if method == http.MethodPost && rest == "git-receive-pack" {
		out := receivePack(repo, body)
		a.sendGit(w, http.StatusOK, receiveResultCT, out)
		return
	}
	a.sendGit(w, http.StatusNotFound, "text/plain", []byte("Not Found"))
}
