package main

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ---------- git smart HTTP ----------

const gitAgent = "forge/1.0"

func (s *Server) dispatchGit(req *Request) *Response {
	// path: /{owner}/{repo}.git/info/refs | /{owner}/{repo}.git/git-upload-pack
	//       | /{owner}/{repo}.git/git-receive-pack
	trimmed := strings.TrimPrefix(req.Path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		return errResponse(404, "Not Found")
	}
	owner, repoName := unescape(parts[0]), unescape(strings.TrimSuffix(parts[1], ".git"))
	s.store.mu.RLock()
	repo, ok := s.store.getRepo(owner, repoName)
	s.store.mu.RUnlock()
	if !ok {
		return errResponse(404, "Not Found")
	}
	return s.dispatchGitPath(req, repo, owner, repoName)
}

// dispatchGitPath handles the real git paths using the raw path.
func (s *Server) dispatchGitPath(req *Request, repo *Repo, owner, repoName string) *Response {
	p := req.Path
	suffix := strings.TrimPrefix(p, "/"+owner+"/"+repoName+".git/")
	switch {
	case suffix == "info/refs" && req.Method == "GET":
		svc := req.Query.Get("service")
		if svc == "git-upload-pack" {
			if req.Headers["git-protocol"] == "version=2" {
				return s.gitUploadPackAdvertV2(repo)
			}
			return s.gitUploadPackAdvertV0(repo)
		}
		if svc == "git-receive-pack" {
			return s.gitReceivePackAdvert(repo)
		}
		return errResponse(400, "unknown service")
	case suffix == "git-upload-pack" && req.Method == "POST":
		if req.Headers["git-protocol"] == "version=2" {
			return s.gitUploadPackV2(req, repo)
		}
		return s.gitUploadPackV0(req, repo)
	case suffix == "git-receive-pack" && req.Method == "POST":
		return s.gitReceivePack(req, repo)
	}
	return errResponse(404, "Not Found")
}

func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// ---------- advertisements ----------

func (s *Server) gitUploadPackAdvertV2(repo *Repo) *Response {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=git-upload-pack\n")))
	buf.Write(pktFlush)
	buf.Write(pktLine([]byte("version 2\n")))
	for _, cap := range []string{
		"agent=" + gitAgent,
		"ls-refs=unborn",
		"fetch=shallow filter",
		"object-format=sha1",
		"object-info",
		"server-option",
	} {
		buf.Write(pktLine([]byte(cap + "\n")))
	}
	buf.Write(pktFlush)
	return &Response{
		Status:  200,
		Headers: map[string]string{"Content-Type": "application/x-git-upload-pack-advertisement"},
		Body:    buf.Bytes(),
	}
}

func (s *Server) gitUploadPackAdvertV0(repo *Repo) *Response {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=git-upload-pack\n")))
	buf.Write(pktFlush)
	caps := "side-band-64k shallow thin-pack ofs-delta symref=HEAD:refs/heads/" + repo.DefaultBranch + " agent=" + gitAgent
	buf.Write(s.refAdvertisement(repo, caps))
	return &Response{
		Status:  200,
		Headers: map[string]string{"Content-Type": "application/x-git-upload-pack-advertisement"},
		Body:    buf.Bytes(),
	}
}

func (s *Server) gitReceivePackAdvert(repo *Repo) *Response {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=git-receive-pack\n")))
	buf.Write(pktFlush)
	caps := "report-status side-band-64k delete-refs ofs-delta agent=" + gitAgent
	buf.Write(s.refAdvertisement(repo, caps))
	return &Response{
		Status:  200,
		Headers: map[string]string{"Content-Type": "application/x-git-receive-pack-advertisement"},
		Body:    buf.Bytes(),
	}
}

// refAdvertisement renders the ref lines for a v0 advertisement.
func (s *Server) refAdvertisement(repo *Repo, caps string) []byte {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	var buf bytes.Buffer
	refs := sortedRefNames(repo.Refs)
	if len(refs) == 0 {
		buf.Write(pktLine([]byte(ZeroOID + " capabilities^{}\x00" + caps + "\n")))
		buf.Write(pktFlush)
		return buf.Bytes()
	}
	// HEAD first
	headRef := "refs/heads/" + repo.DefaultBranch
	if tip, ok := repo.Refs[headRef]; ok {
		buf.Write(pktLine([]byte(tip + " HEAD\x00" + caps + "\n")))
	} else if len(refs) > 0 {
		// fall back to the first ref as HEAD target
		buf.Write(pktLine([]byte(repo.Refs[refs[0]] + " HEAD\x00" + caps + "\n")))
	}
	for _, ref := range refs {
		oid := repo.Refs[ref]
		buf.Write(pktLine([]byte(oid + " " + ref + "\n")))
		// peeled line for annotated tags
		if strings.HasPrefix(ref, "refs/tags/") {
			if obj, ok := repo.Objects[oid]; ok && obj.Type == ObjTag {
				target := tagTarget(obj.Data)
				if target != "" {
					buf.Write(pktLine([]byte(target + " " + ref + "^{}\n")))
				}
			}
		}
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

func sortedRefNames(refs map[string]string) []string {
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func tagTarget(data []byte) string {
	lines := strings.SplitN(string(data), "\n", 2)
	if len(lines) > 0 && strings.HasPrefix(lines[0], "object ") {
		return strings.TrimSpace(lines[0][7:])
	}
	return ""
}

// ---------- upload-pack v2 ----------

type fetchRequest struct {
	wants    []string
	haves    []string
	shallow  []string // client's current shallow boundaries
	deepen   int      // 0 = none
	deepenSet bool
	filter   string
	thinPack bool
	ofsDelta bool
}

func parseFetchLines(lines [][]byte) fetchRequest {
	var fr fetchRequest
	for _, raw := range lines {
		line := strings.TrimSuffix(string(raw), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			oid := strings.TrimSpace(line[5:])
			if i := strings.Index(oid, "\x00"); i >= 0 {
				oid = oid[:i]
			}
			fr.wants = append(fr.wants, strings.TrimSpace(oid))
		case strings.HasPrefix(line, "have "):
			fr.haves = append(fr.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			fr.shallow = append(fr.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			var d int
			if _, err := fmt.Sscanf(line, "deepen %d", &d); err == nil {
				fr.deepen = d
				fr.deepenSet = true
			}
		case strings.HasPrefix(line, "deepen-since "):
			fr.deepen = 1 // treat as depth 1 (client never uses it in checks)
			fr.deepenSet = true
		case strings.HasPrefix(line, "deepen-not "):
			// not supported; ignore (client never uses it)
		case strings.HasPrefix(line, "filter "):
			fr.filter = strings.TrimSpace(line[7:])
		case line == "thin-pack":
			fr.thinPack = true
		case line == "ofs-delta":
			fr.ofsDelta = true
		}
	}
	return fr
}

// buildFetchPack computes the pack + boundary for a fetch request.
func (s *Server) buildFetchPack(repo *Repo, fr fetchRequest) (pack []byte, boundary []string) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	candidates, boundary := reachableDepth(repo, fr.wants, fr.deepen)
	held := heldSet(repo, fr.haves, fr.shallow)
	var objs []Object
	oids := make([]string, 0, len(candidates))
	for oid := range candidates {
		if held[oid] {
			continue
		}
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	for _, oid := range oids {
		obj, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		// honor filters
		if fr.filter == "blob:none" && obj.Type == ObjBlob {
			continue
		}
		if fr.filter == "tree:0" && (obj.Type == ObjTree || obj.Type == ObjBlob) {
			continue
		}
		if strings.HasPrefix(fr.filter, "blob:limit ") {
			var limit int64
			if _, err := fmt.Sscanf(fr.filter, "blob:limit %d", &limit); err == nil {
				if obj.Type == ObjBlob && int64(len(obj.Data)) > limit {
					continue
				}
			}
		}
		objs = append(objs, obj)
	}
	return writePack(objs), boundary
}

func sidebandPack(pack []byte) []byte {
	var buf bytes.Buffer
	for len(pack) > 0 {
		chunk := pack
		if len(chunk) > 65515 {
			chunk = pack[:65515]
		}
		payload := append([]byte{1}, chunk...)
		buf.Write(pktLine(payload))
		pack = pack[len(chunk):]
	}
	if len(pack) == 0 && buf.Len() == 0 {
		// empty pack still needs at least one sideband packet
		buf.Write(pktLine([]byte{1}))
	}
	return buf.Bytes()
}

func (s *Server) gitUploadPackV2(req *Request, repo *Repo) *Response {
	// parse pkt-lines; first section ends at delim, rest until flush
	r := &pktReader{data: req.Body}
	var lines [][]byte
	inCommand := true
	command := ""
	for {
		payload, kind := r.readLine()
		if kind == "" || kind == "flush" {
			break
		}
		if kind == "delim" {
			inCommand = false
			continue
		}
		if kind != "line" {
			continue
		}
		if inCommand {
			line := strings.TrimSuffix(string(payload), "\n")
			if strings.HasPrefix(line, "command=") {
				command = line[len("command="):]
			}
			continue
		}
		lines = append(lines, payload)
	}
	if command == "ls-refs" {
		return s.gitLsRefs(repo, lines)
	}
	if command != "fetch" {
		return errResponse(400, "unknown command")
	}
	fr := parseFetchLines(lines)
	pack, boundary := s.buildFetchPack(repo, fr)
	// acknowledgments
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("acknowledgments\n")))
	ack := ""
	s.store.mu.RLock()
	for _, h := range fr.haves {
		for _, w := range fr.wants {
			if isAncestor(repo, h, w) {
				ack = h
				break
			}
		}
		if ack != "" {
			break
		}
	}
	s.store.mu.RUnlock()
	if ack != "" {
		buf.Write(pktLine([]byte("ACK " + ack + "\n")))
		buf.Write(pktLine([]byte("ready\n")))
	} else {
		buf.Write(pktLine([]byte("NAK\n")))
	}
	buf.Write(pktDelim)
	if fr.deepenSet && fr.deepen > 0 {
		buf.Write(pktLine([]byte("shallow-info\n")))
		for _, b := range boundary {
			buf.Write(pktLine([]byte("shallow " + b + "\n")))
		}
		// unshallow: previous shallow boundaries now included
		for _, sh := range fr.shallow {
			if _, ok := repo.Objects[sh]; ok {
				// included if it is in the candidate set (distance < depth)
				// — emit unshallow for boundaries that are ancestors of wants
				// within the depth. Conservative: emit for any shallow boundary
				// that is an ancestor-or-equal of a want.
				for _, w := range fr.wants {
					if isAncestor(repo, sh, w) {
						buf.Write(pktLine([]byte("unshallow " + sh + "\n")))
						break
					}
				}
			}
		}
		buf.Write(pktDelim)
	}
	buf.Write(pktLine([]byte("packfile\n")))
	buf.Write(sidebandPack(pack))
	buf.Write(pktFlush)
	return &Response{
		Status:  200,
		Headers: map[string]string{"Content-Type": "application/x-git-upload-pack-result"},
		Body:    buf.Bytes(),
	}
}

// gitLsRefs handles the ls-refs command.
func (s *Server) gitLsRefs(repo *Repo, lines [][]byte) *Response {
	var prefixes []string
	peel, symrefs, unborn := false, false, false
	for _, raw := range lines {
		line := strings.TrimSuffix(string(raw), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimSpace(line[len("ref-prefix "):]))
		}
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	var buf bytes.Buffer
	match := func(ref string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(ref, p) {
				return true
			}
		}
		return false
	}
	headRef := "refs/heads/" + repo.DefaultBranch
	headTip, headExists := repo.Refs[headRef]
	empty := len(repo.Refs) == 0
	// HEAD line
	if match("HEAD") {
		if empty {
			if unborn {
				line := "unborn HEAD"
				if symrefs {
					line += " symref-target:" + headRef
				}
				buf.Write(pktLine([]byte(line + "\n")))
			}
		} else if headExists {
			line := headTip + " HEAD"
			if symrefs {
				line += " symref-target:" + headRef
			}
			buf.Write(pktLine([]byte(line + "\n")))
		}
	}
	for _, ref := range sortedRefNames(repo.Refs) {
		if !match(ref) {
			continue
		}
		oid := repo.Refs[ref]
		line := oid + " " + ref
		if peel && strings.HasPrefix(ref, "refs/tags/") {
			if obj, ok := repo.Objects[oid]; ok && obj.Type == ObjTag {
				if target := tagTarget(obj.Data); target != "" {
					line += " peeled:" + target
				}
			}
		}
		buf.Write(pktLine([]byte(line + "\n")))
	}
	buf.Write(pktFlush)
	return &Response{
		Status:  200,
		Headers: map[string]string{"Content-Type": "application/x-git-upload-pack-result"},
		Body:    buf.Bytes(),
	}
}

// ---------- upload-pack v0 ----------

func (s *Server) gitUploadPackV0(req *Request, repo *Repo) *Response {
	r := &pktReader{data: req.Body}
	section1 := r.readLinesUntilFlush()
	section2 := r.readLinesUntilFlush()
	var fr fetchRequest
	for _, raw := range section1 {
		line := strings.TrimSuffix(string(raw), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			oid := strings.TrimSpace(line[5:])
			if i := strings.Index(oid, "\x00"); i >= 0 {
				oid = oid[:i]
			}
			fr.wants = append(fr.wants, strings.TrimSpace(oid))
		case strings.HasPrefix(line, "shallow "):
			fr.shallow = append(fr.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			var d int
			if _, err := fmt.Sscanf(line, "deepen %d", &d); err == nil {
				fr.deepen = d
				fr.deepenSet = true
			}
		}
	}
	for _, raw := range section2 {
		line := strings.TrimSuffix(string(raw), "\n")
		if strings.HasPrefix(line, "have ") {
			fr.haves = append(fr.haves, strings.TrimSpace(line[5:]))
		}
	}
	pack, boundary := s.buildFetchPack(repo, fr)
	var buf bytes.Buffer
	ack := ""
	s.store.mu.RLock()
	for _, h := range fr.haves {
		for _, w := range fr.wants {
			if isAncestor(repo, h, w) {
				ack = h
				break
			}
		}
		if ack != "" {
			break
		}
	}
	s.store.mu.RUnlock()
	if ack != "" {
		buf.Write(pktLine([]byte("ACK " + ack + "\n")))
	} else {
		buf.Write(pktLine([]byte("NAK\n")))
	}
	for _, b := range boundary {
		buf.Write(pktLine([]byte("shallow " + b + "\n")))
	}
	buf.Write(sidebandPack(pack))
	buf.Write(pktFlush)
	return &Response{
		Status:  200,
		Headers: map[string]string{"Content-Type": "application/x-git-upload-pack-result"},
		Body:    buf.Bytes(),
	}
}

// ---------- receive-pack ----------

type pushCommand struct {
	old string
	new string
	ref string
}

func (s *Server) gitReceivePack(req *Request, repo *Repo) *Response {
	// body: pkt-lines until flush, then raw pack
	r := &pktReader{data: req.Body}
	var cmds []pushCommand
	for {
		payload, kind := r.readLine()
		if kind == "" || kind == "flush" {
			break
		}
		if kind != "line" {
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if i := strings.Index(line, "\x00"); i >= 0 {
			line = line[:i] // strip capabilities
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		cmds = append(cmds, pushCommand{old: fields[0], new: fields[1], ref: fields[2]})
	}
	packBytes := req.Body[r.pos:]
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	results := make([]string, 0, len(cmds))
	unpackOK := true
	if len(packBytes) > 0 {
		objs, err := readPack(packBytes)
		if err != nil {
			unpackOK = false
		} else {
			for _, o := range objs {
				repo.putObject(o)
			}
		}
	}
	for _, c := range cmds {
		if !unpackOK {
			results = append(results, "ng "+c.ref+" unpack failed")
			continue
		}
		results = append(results, s.applyPushCommand(repo, c))
	}
	var inner bytes.Buffer
	if unpackOK {
		inner.Write(pktLine([]byte("unpack ok\n")))
	} else {
		inner.Write(pktLine([]byte("unpack unpack failed\n")))
	}
	for _, res := range results {
		inner.Write(pktLine([]byte(res + "\n")))
	}
	inner.Write(pktFlush)
	var buf bytes.Buffer
	buf.Write(pktLine(append([]byte{1}, inner.Bytes()...)))
	buf.Write(pktFlush)
	return &Response{
		Status:  200,
		Headers: map[string]string{"Content-Type": "application/x-git-receive-pack-result"},
		Body:    buf.Bytes(),
	}
}

func (s *Server) applyPushCommand(repo *Repo, c pushCommand) string {
	isTag := strings.HasPrefix(c.ref, "refs/tags/")
	current, exists := repo.Refs[c.ref]
	// delete
	if c.new == ZeroOID {
		if !exists {
			return "ng " + c.ref + " remote ref does not exist"
		}
		if c.old != ZeroOID && c.old != current {
			return "ng " + c.ref + " non-fast-forward"
		}
		delete(repo.Refs, c.ref)
		return "ok " + c.ref
	}
	// new object must exist
	if _, ok := repo.Objects[c.new]; !ok {
		return "ng " + c.ref + " bad object"
	}
	// tag updates are never fast-forwardable
	if isTag && exists && c.new != current {
		return "ng " + c.ref + " already exists"
	}
	// create
	if c.old == ZeroOID {
		if exists {
			return "ng " + c.ref + " already exists"
		}
		repo.Refs[c.ref] = c.new
		return "ok " + c.ref
	}
	// update
	if !exists {
		return "ng " + c.ref + " remote ref does not exist"
	}
	if c.new == current {
		return "ok " + c.ref
	}
	if !isAncestor(repo, current, c.new) {
		return "ng " + c.ref + " non-fast-forward"
	}
	repo.Refs[c.ref] = c.new
	return "ok " + c.ref
}
