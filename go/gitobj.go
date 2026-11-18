package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------- git object model ----------

const (
	ObjCommit   = 1
	ObjTree     = 2
	ObjBlob     = 3
	ObjTag      = 4
	ObjOfsDelta = 6
	ObjRefDelta = 7
)

const ZeroOID = "0000000000000000000000000000000000000000"

// Object is a single git object.
type Object struct {
	Type int
	Data []byte
}

// hashObject returns the SHA-1 id of an object. It hashes the header and the
// data in one streaming pass instead of copying the data behind a header.
func hashObject(typ int, data []byte) string {
	var typeName string
	switch typ {
	case ObjCommit:
		typeName = "commit"
	case ObjTree:
		typeName = "tree"
	case ObjBlob:
		typeName = "blob"
	case ObjTag:
		typeName = "tag"
	}
	h := sha1.New()
	h.Write([]byte(typeName))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------- tree ----------

type TreeEntry struct {
	Mode  string // "100644", "100755", "40000", "120000", "160000"
	Name  string
	OID   string
	IsDir bool
}

// compareTreeNames matches git's tree sorting: directories compare as name+"/".
func compareTreeNames(aName string, aDir bool, bName string, bDir bool) int {
	an, bn := aName, bName
	if aDir {
		an += "/"
	}
	if bDir {
		bn += "/"
	}
	return strings.Compare(an, bn)
}

// parseTree parses raw tree bytes into entries (in stored order).
func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := bytes.IndexByte(data[i:], 0)
		if nul < 0 || i+nul+20 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		oid := hex.EncodeToString(data[i : i+20])
		i += 20
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid, IsDir: mode == "40000"})
	}
	return out
}

// buildTree serializes entries (sorted git-style) into raw tree bytes.
func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return compareTreeNames(sorted[i].Name, sorted[i].IsDir, sorted[j].Name, sorted[j].IsDir) < 0
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		oidBytes := make([]byte, 20)
		hex.Decode(oidBytes, []byte(e.OID))
		buf.Write(oidBytes)
	}
	return buf.Bytes()
}

// hexToBytes converts a 40-hex oid to 20 bytes.
func hexToBytes(oid string) []byte {
	out := make([]byte, 20)
	hex.Decode(out, []byte(oid))
	return out
}

// ---------- commit / tag ----------

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string // raw "Name <email> ts tz"
	Committer string
	Message   string
	AuthorTS  int64
}

// parseCommit parses raw commit bytes without copying the whole object to a
// string: it scans the header lines in place.
func parseCommit(data []byte) CommitInfo {
	var c CommitInfo
	// headers end at blank line
	idx := bytes.Index(data, []byte("\n\n"))
	var head, msg []byte
	if idx >= 0 {
		head = data[:idx]
		msg = data[idx+2:]
	} else {
		head = data
	}
	c.Message = string(msg)
	for len(head) > 0 {
		var line []byte
		if nl := bytes.IndexByte(head, '\n'); nl >= 0 {
			line, head = head[:nl], head[nl+1:]
		} else {
			line, head = head, nil
		}
		switch {
		case bytes.HasPrefix(line, []byte("tree ")):
			c.Tree = strings.TrimSpace(string(line[5:]))
		case bytes.HasPrefix(line, []byte("parent ")):
			c.Parents = append(c.Parents, strings.TrimSpace(string(line[7:])))
		case bytes.HasPrefix(line, []byte("author ")):
			c.Author = strings.TrimSpace(string(line[7:]))
			c.AuthorTS = parseIdentTS(c.Author)
		case bytes.HasPrefix(line, []byte("committer ")):
			c.Committer = strings.TrimSpace(string(line[10:]))
		}
	}
	return c
}

// parseIdentTS extracts the epoch seconds from an ident line.
func parseIdentTS(ident string) int64 {
	fields := strings.Fields(ident)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// identNameEmail splits "Name <email> ts tz" into name and email.
func identNameEmail(ident string) (string, string) {
	lt := strings.Index(ident, "<")
	gt := strings.Index(ident, ">")
	if lt < 0 || gt < 0 || gt < lt {
		return ident, ""
	}
	return strings.TrimSpace(ident[:lt]), ident[lt+1 : gt]
}

// buildCommit serializes a commit. message gets a trailing newline if missing.
func buildCommit(tree string, parents []string, author, committer, message string) []byte {
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	var sb strings.Builder
	sb.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + author + "\n")
	sb.WriteString("committer " + committer + "\n")
	sb.WriteString("\n")
	sb.WriteString(message)
	return []byte(sb.String())
}

// buildTag serializes an annotated tag object.
func buildTag(object, typ, tag, tagger, message string) []byte {
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	var sb strings.Builder
	sb.WriteString("object " + object + "\n")
	sb.WriteString("type " + typ + "\n")
	sb.WriteString("tag " + tag + "\n")
	sb.WriteString("tagger " + tagger + "\n")
	sb.WriteString("\n")
	sb.WriteString(message)
	return []byte(sb.String())
}

// ---------- pkt-line ----------

// pktLine encodes one pkt-line (4 hex length + payload).
func pktLine(payload []byte) []byte {
	total := len(payload) + 4
	out := make([]byte, 4, total)
	copy(out, fmt.Sprintf("%04x", total))
	return append(out, payload...)
}

// pktFlush is the flush packet.
var pktFlush = []byte("0000")

// pktDelim is the delim packet.
var pktDelim = []byte("0001")

// pktResponseEnd is the response-end packet.
var pktResponseEnd = []byte("0002")

// pktReader reads pkt-lines from a byte stream.
type pktReader struct {
	data []byte
	pos  int
}

// readLine returns (payload, kind) where kind is "line", "flush", "delim",
// "response-end", or "" on error/EOF.
func (r *pktReader) readLine() ([]byte, string) {
	if r.pos+4 > len(r.data) {
		return nil, ""
	}
	hdr := string(r.data[r.pos : r.pos+4])
	n, err := strconv.ParseInt(hdr, 16, 32)
	if err != nil || n < 0 {
		return nil, ""
	}
	if n == 0 {
		r.pos += 4
		return nil, "flush"
	}
	if n == 1 {
		r.pos += 4
		return nil, "delim"
	}
	if n == 2 {
		r.pos += 4
		return nil, "response-end"
	}
	if r.pos+int(n) > len(r.data) {
		return nil, ""
	}
	payload := r.data[r.pos+4 : r.pos+int(n)]
	r.pos += int(n)
	return payload, "line"
}

// readLinesUntilFlush collects payload lines until flush/EOF.
func (r *pktReader) readLinesUntilFlush() [][]byte {
	var out [][]byte
	for {
		payload, kind := r.readLine()
		if kind == "flush" || kind == "" {
			return out
		}
		if kind == "line" {
			out = append(out, payload)
		}
	}
}

// ---------- pack ----------

// writePackObjectHeader encodes a pack entry header (type + size varint).
func writePackObjectHeader(typ int, size int) []byte {
	var out []byte
	remaining := size
	first := (typ << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	out = append(out, byte(first))
	return out
}

// countingReader tracks bytes consumed through Read and ReadByte.
type countingReader struct {
	r *bytes.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// zlibReaderPool reuses zlib readers across pack entries: a fresh reader pays
// setup cost per stream, which dominates inflating small objects.
var zlibReaderPool sync.Pool

// readPack parses a pack file. Returns objects in pack order.
// Verifies: magic, version 2, declared count, per-object inflated size,
// and the 20-byte trailer checksum.
func readPack(data []byte) ([]Object, error) {
	if len(data) < 12+20 {
		return nil, fmt.Errorf("pack too short")
	}
	if string(data[0:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack magic")
	}
	ver := binary.BigEndian.Uint32(data[4:8])
	if ver != 2 {
		return nil, fmt.Errorf("unsupported pack version %d", ver)
	}
	count := binary.BigEndian.Uint32(data[8:12])
	pos := 12
	var objs []Object
	for i := uint32(0); i < count; i++ {
		if pos >= len(data)-20 {
			return nil, fmt.Errorf("pack truncated at object %d", i)
		}
		// header varint
		b0 := data[pos]
		pos++
		typ := int((b0 >> 4) & 0x7)
		size := int64(b0 & 0x0f)
		shift := uint(4)
		for b0&0x80 != 0 {
			if pos >= len(data)-20 {
				return nil, fmt.Errorf("pack truncated in header")
			}
			b0 = data[pos]
			pos++
			size |= int64(b0&0x7f) << shift
			shift += 7
		}
		var baseOID string
		switch typ {
		case ObjOfsDelta:
			// ofs-delta: negative offset varint
			if pos >= len(data)-20 {
				return nil, fmt.Errorf("pack truncated in ofs-delta")
			}
			b := data[pos]
			pos++
			off := int64(b & 0x7f)
			for b&0x80 != 0 {
				b = data[pos]
				pos++
				off = ((off + 1) << 7) | int64(b&0x7f)
			}
			// base object is at pos-1-off (relative to entry start); resolve later
			_ = off
			return nil, fmt.Errorf("ofs-delta packs not supported")
		case ObjRefDelta:
			if pos+20 > len(data)-20 {
				return nil, fmt.Errorf("pack truncated in ref-delta")
			}
			baseOID = fmt.Sprintf("%x", data[pos:pos+20])
			pos += 20
			_ = baseOID
			return nil, fmt.Errorf("ref-delta packs not supported")
		}
		// zlib stream at pos; count consumed bytes
		cr := &countingReader{r: bytes.NewReader(data[pos:])}
		var zr io.ReadCloser
		var zerr error
		if v := zlibReaderPool.Get(); v != nil {
			zr = v.(io.ReadCloser)
			zerr = zr.(zlib.Resetter).Reset(cr, nil)
			if zerr != nil {
				zr, zerr = zlib.NewReader(cr)
			}
		} else {
			zr, zerr = zlib.NewReader(cr)
		}
		if zerr != nil {
			return nil, fmt.Errorf("zlib open: %v", zerr)
		}
		inflated, err := io.ReadAll(zr)
		zr.Close()
		zlibReaderPool.Put(zr)
		if err != nil {
			return nil, fmt.Errorf("zlib inflate: %v", err)
		}
		if int64(len(inflated)) != size {
			return nil, fmt.Errorf("object size mismatch: declared %d got %d", size, len(inflated))
		}
		pos += int(cr.n)
		objs = append(objs, Object{Type: typ, Data: inflated})
	}
	if len(objs) != int(count) {
		return nil, fmt.Errorf("pack count mismatch")
	}
	// trailer checksum
	want := data[len(data)-20:]
	got := sha1.Sum(data[:len(data)-20])
	if !bytes.Equal(want, got[:]) {
		return nil, fmt.Errorf("pack trailer checksum mismatch")
	}
	if pos != len(data)-20 {
		return nil, fmt.Errorf("pack has stray bytes")
	}
	return objs, nil
}

// zlibWriterPool reuses BestSpeed zlib writers across pack entries: a fresh
// writer allocates large deflate tables per stream, which dominates the cost
// of compressing small objects. Any zlib level is valid in a pack.
var zlibWriterPool = sync.Pool{New: func() interface{} {
	w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
	return w
}}

// writePack serializes objects into a pack file (plain entries, no deltas).
func writePack(objs []Object) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objs)))
	buf.Write(hdr[:])
	for _, o := range objs {
		buf.Write(writePackObjectHeader(o.Type, len(o.Data)))
		zw := zlibWriterPool.Get().(*zlib.Writer)
		zw.Reset(&buf)
		zw.Write(o.Data)
		zw.Close()
		zlibWriterPool.Put(zw)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// ---------- reachability ----------

// objStore is the interface the reachability walk needs.
type objStore interface {
	getObject(oid string) (Object, bool)
}

// walkCommitParents yields the parents of a commit object.
func walkCommitParents(obj Object) []string {
	if obj.Type != ObjCommit {
		return nil
	}
	return parseCommit(obj.Data).Parents
}

// reachableSet returns the full closure of roots (commits, tags, trees, blobs).
func reachableSet(st objStore, roots []string) map[string]bool {
	seen := map[string]bool{}
	var stack []string
	for _, r := range roots {
		if r != "" && r != ZeroOID {
			stack = append(stack, r)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj, ok := st.getObject(oid)
		if !ok {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case ObjCommit:
			c := parseCommit(obj.Data)
			if c.Tree != "" {
				stack = append(stack, c.Tree)
			}
			for _, p := range c.Parents {
				stack = append(stack, p)
			}
		case ObjTag:
			// tag object: first line "object <oid>"
			lines := strings.SplitN(string(obj.Data), "\n", 2)
			if len(lines) > 0 && strings.HasPrefix(lines[0], "object ") {
				stack = append(stack, strings.TrimSpace(lines[0][7:]))
			}
		case ObjTree:
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.OID)
			}
		}
	}
	return seen
}

// reachableDepth computes the candidate set for a fetch with an optional depth
// limit. depth 0 means unlimited. It returns (objects, boundaryCommits):
//   - objects: all objects to send (commits at distance < depth when limited,
//     plus their full tree/blob closure; tag objects and their targets).
//   - boundary: commits at distance depth-1 (the "shallow" lines), empty when
//     unlimited.
func reachableDepth(st objStore, wants []string, depth int) (map[string]bool, []string) {
	out := map[string]bool{}
	var boundary []string
	// commit distances
	dist := map[string]int{}
	type item struct {
		oid string
		d   int
	}
	var queue []item
	for _, w := range wants {
		if w == "" || w == ZeroOID {
			continue
		}
		obj, ok := st.getObject(w)
		if !ok {
			continue
		}
		if obj.Type == ObjTag {
			// tag itself is included; its target is distance 0
			out[w] = true
			lines := strings.SplitN(string(obj.Data), "\n", 2)
			if len(lines) > 0 && strings.HasPrefix(lines[0], "object ") {
				t := strings.TrimSpace(lines[0][7:])
				if _, ok := st.getObject(t); ok {
					queue = append(queue, item{t, 0})
				}
			}
			continue
		}
		queue = append(queue, item{w, 0})
	}
	// BFS over commits by distance
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if prev, seen := dist[it.oid]; seen && prev <= it.d {
			continue
		}
		dist[it.oid] = it.d
		obj, ok := st.getObject(it.oid)
		if !ok {
			continue
		}
		if obj.Type != ObjCommit {
			// non-commit want (tree/blob): include closure directly
			for o := range reachableSet(st, []string{it.oid}) {
				out[o] = true
			}
			continue
		}
		if depth > 0 && it.d >= depth {
			continue // beyond depth: not included, not a boundary
		}
		out[it.oid] = true
		c := parseCommit(obj.Data)
		// full tree closure of included commits
		if c.Tree != "" {
			for o := range reachableSet(st, []string{c.Tree}) {
				out[o] = true
			}
		}
		if depth > 0 && it.d == depth-1 {
			boundary = append(boundary, it.oid)
			continue // parents are beyond depth
		}
		for _, p := range c.Parents {
			queue = append(queue, item{p, it.d + 1})
		}
	}
	sort.Strings(boundary)
	return out, boundary
}

// heldSet computes the objects a client already holds, given its have oids
// and its shallow boundary list: the walk stops descending past a shallow
// boundary commit (the client has the boundary + its tree, not its parents).
func heldSet(st objStore, haves []string, shallow []string) map[string]bool {
	shallowSet := map[string]bool{}
	for _, s := range shallow {
		shallowSet[s] = true
	}
	seen := map[string]bool{}
	var stack []string
	for _, h := range haves {
		if h != "" && h != ZeroOID {
			stack = append(stack, h)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj, ok := st.getObject(oid)
		if !ok {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case ObjCommit:
			c := parseCommit(obj.Data)
			if c.Tree != "" {
				for o := range reachableSet(st, []string{c.Tree}) {
					seen[o] = true
				}
			}
			if shallowSet[oid] {
				continue // do not descend past a shallow boundary
			}
			for _, p := range c.Parents {
				stack = append(stack, p)
			}
		case ObjTag:
			lines := strings.SplitN(string(obj.Data), "\n", 2)
			if len(lines) > 0 && strings.HasPrefix(lines[0], "object ") {
				stack = append(stack, strings.TrimSpace(lines[0][7:]))
			}
		case ObjTree:
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.OID)
			}
		}
	}
	return seen
}

// isAncestor reports whether anc is anc-or-equal of desc (commit ancestry).
func isAncestor(st objStore, anc, desc string) bool {
	if anc == desc {
		return true
	}
	seen := map[string]bool{}
	stack := []string{desc}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := st.getObject(oid)
		if !ok || obj.Type != ObjCommit {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if p == anc {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

// mergeBase returns the nearest common ancestor of a and b (min distA+distB).
func mergeBase(st objStore, a, b string) string {
	if a == "" || b == "" || a == ZeroOID || b == ZeroOID {
		return ""
	}
	// BFS distances from a
	distA := map[string]int{}
	type item struct {
		oid string
		d   int
	}
	queue := []item{{a, 0}}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if _, seen := distA[it.oid]; seen {
			continue
		}
		distA[it.oid] = it.d
		obj, ok := st.getObject(it.oid)
		if !ok || obj.Type != ObjCommit {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			queue = append(queue, item{p, it.d + 1})
		}
	}
	// BFS from b, tracking best common
	best := ""
	bestScore := -1
	distB := map[string]int{}
	queue = []item{{b, 0}}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if _, seen := distB[it.oid]; seen {
			continue
		}
		distB[it.oid] = it.d
		if da, ok := distA[it.oid]; ok {
			score := da + it.d
			if bestScore < 0 || score < bestScore {
				bestScore = score
				best = it.oid
			}
		}
		obj, ok := st.getObject(it.oid)
		if !ok || obj.Type != ObjCommit {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			queue = append(queue, item{p, it.d + 1})
		}
	}
	return best
}

// ---------- flat tree helpers ----------

// FileEntry is a file in a flattened tree.
type FileEntry struct {
	Mode string
	OID  string
}

// flattenTree returns path -> file for all blobs under a tree (recursive).
func flattenTree(st objStore, treeOID string) map[string]FileEntry {
	out := map[string]FileEntry{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj, ok := st.getObject(oid)
		if !ok || obj.Type != ObjTree {
			return
		}
		for _, e := range parseTree(obj.Data) {
			path := prefix + e.Name
			if e.IsDir {
				walk(e.OID, path+"/")
			} else {
				out[path] = FileEntry{Mode: e.Mode, OID: e.OID}
			}
		}
	}
	walk(treeOID, "")
	return out
}

// buildTreeFromFiles builds nested tree objects from a flat path map and
// stores them via put. Returns the root tree oid.
func buildTreeFromFiles(r *Repo, files map[string]FileEntry) string {
	// group by directory
	type dirNode struct {
		files map[string]FileEntry // name -> file (leaf in this dir)
		dirs  map[string]*dirNode
	}
	root := &dirNode{files: map[string]FileEntry{}, dirs: map[string]*dirNode{}}
	for path, fe := range files {
		parts := strings.Split(path, "/")
		node := root
		for i := 0; i < len(parts)-1; i++ {
			if node.dirs[parts[i]] == nil {
				node.dirs[parts[i]] = &dirNode{files: map[string]FileEntry{}, dirs: map[string]*dirNode{}}
			}
			node = node.dirs[parts[i]]
		}
		node.files[parts[len(parts)-1]] = fe
	}
	var build func(n *dirNode) string
	build = func(n *dirNode) string {
		var entries []TreeEntry
		for name, fe := range n.files {
			entries = append(entries, TreeEntry{Mode: fe.Mode, Name: name, OID: fe.OID})
		}
		dirNames := make([]string, 0, len(n.dirs))
		for name := range n.dirs {
			dirNames = append(dirNames, name)
		}
		sort.Strings(dirNames)
		for _, name := range dirNames {
			sub := build(n.dirs[name])
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: sub, IsDir: true})
		}
		data := buildTree(entries)
		oid := hashObject(ObjTree, data)
		r.putObject(Object{Type: ObjTree, Data: data})
		return oid
	}
	return build(root)
}
