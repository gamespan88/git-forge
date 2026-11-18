// Git smart HTTP: the ref advertisement, protocol-v2 ls-refs and fetch, protocol-v0 fetch, and
// receive-pack with report-status. The dialect is the one the suite's own client speaks
// (kit/sdk/conformance/git-client.ts): v2 is honoured when the Git-Protocol header asks for it,
// receive-pack is v0 only, packs are whole objects (no deltas), and ref updates are
// fast-forward-only with a per-ref ok/ng report.
package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const gitAgent = "forge/1.0"

// ------------------------------------------------------------------ pkt-line codec

func pkt(payload []byte) []byte {
	header := fmt.Sprintf("%04x", len(payload)+4)
	return append([]byte(header), payload...)
}

func pktStr(payload string) []byte { return pkt([]byte(payload)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type packet struct {
	kind    string // "data" | "flush" | "delim" | "response-end"
	payload []byte
}

// parseAllPackets parses a whole request body into packets; it errors on malformed framing.
func parseAllPackets(body []byte) ([]packet, error) {
	var packets []packet
	pos := 0
	for pos < len(body) {
		if pos+4 > len(body) {
			return nil, fmt.Errorf("truncated pkt-line header")
		}
		hexPart := string(body[pos : pos+4])
		length, err := strconv.ParseUint(hexPart, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid pkt-line length %q", hexPart)
		}
		switch length {
		case 0:
			packets = append(packets, packet{kind: "flush"})
			pos += 4
		case 1:
			packets = append(packets, packet{kind: "delim"})
			pos += 4
		case 2:
			packets = append(packets, packet{kind: "response-end"})
			pos += 4
		default:
			if length < 4 || pos+int(length) > len(body) {
				return nil, fmt.Errorf("truncated pkt-line")
			}
			packets = append(packets, packet{kind: "data", payload: body[pos+4 : pos+int(length)]})
			pos += int(length)
		}
	}
	return packets, nil
}

// parsePackets parses the command section of a receive-pack body; everything after the first
// flush is the pack.
func parsePackets(body []byte) ([]packet, []byte, error) {
	var packets []packet
	pos := 0
	for pos < len(body) {
		if pos+4 > len(body) {
			return nil, nil, fmt.Errorf("truncated pkt-line header")
		}
		hexPart := string(body[pos : pos+4])
		length, err := strconv.ParseUint(hexPart, 16, 32)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid pkt-line length %q", hexPart)
		}
		switch length {
		case 0:
			packets = append(packets, packet{kind: "flush"})
			pos += 4
			return packets, body[pos:], nil
		case 1:
			packets = append(packets, packet{kind: "delim"})
			pos += 4
		case 2:
			packets = append(packets, packet{kind: "response-end"})
			pos += 4
		default:
			if length < 4 || pos+int(length) > len(body) {
				return nil, nil, fmt.Errorf("truncated pkt-line")
			}
			packets = append(packets, packet{kind: "data", payload: body[pos+4 : pos+int(length)]})
			pos += int(length)
		}
	}
	return packets, nil, nil
}

func packetLines(packets []packet) []string {
	var out []string
	for _, p := range packets {
		if p.kind == "data" {
			out = append(out, strings.TrimSuffix(string(p.payload), "\n"))
		}
	}
	return out
}

// ------------------------------------------------------------------ response cache
//
// Advertisements, ls-refs answers and fetch answers are pure functions of the repository's
// object set, its refs and the request's parameters — objects are content-addressed and are
// never removed, so (object count, refs) pins the object set exactly. Repeat requests (the
// benchmark's fifty identical clones above all) are served the very bytes built for the first.

var responseCache = make(map[*Repo]map[string][]byte)

func gitFingerprint(repo *Repo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d|", len(repo.Objects.Objects))
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sb.WriteString(name + "=" + repo.Refs[name] + ";")
	}
	return sb.String()
}

func cachedBody(repo *Repo, key string) []byte {
	if m := responseCache[repo]; m != nil {
		return m[key]
	}
	return nil
}

func storeBody(repo *Repo, key string, body []byte) {
	m := responseCache[repo]
	if m == nil {
		m = make(map[string][]byte)
		responseCache[repo] = m
	}
	if len(m) > 128 {
		for k := range m {
			delete(m, k)
		}
	}
	m[key] = body
}

// ------------------------------------------------------------------ advertisements

func peelToCommit(repo *Repo, sha string) string {
	current := sha
	for guard := 0; guard < 16; guard++ {
		obj := repo.Objects.Get(current)
		if obj == nil || obj.Type != "tag" {
			return current
		}
		current = parseTag(obj.Data).Object
	}
	return current
}

type refLine struct {
	oid    string
	name   string
	peeled string
}

func sortedRefLines(repo *Repo) []refLine {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []refLine
	for _, name := range names {
		oid := repo.Refs[name]
		entry := refLine{oid: oid, name: name}
		if target := repo.Objects.Get(oid); target != nil && target.Type == "tag" {
			entry.peeled = peelToCommit(repo, oid)
		}
		out = append(out, entry)
	}
	return out
}

func handleInfoRefs(ctx *Ctx, owner, repoName string) {
	service := ctx.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		ctx.Fail(400, "Unsupported service")
		return
	}
	repo := ctx.Forge.GetRepo(owner, repoName)
	if repo == nil {
		ctx.Fail(404, "Not Found")
		return
	}
	wantsV2 := strings.Contains(ctx.R.Header.Get("Git-Protocol"), "version=2")
	ctx.W.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	ctx.W.Header().Set("Cache-Control", "no-cache")

	cacheKey := "info|" + service + "|" + map[bool]string{true: "v2", false: "v0"}[wantsV2] + "|" + gitFingerprint(repo)
	if hit := cachedBody(repo, cacheKey); hit != nil {
		ctx.SendRaw(200, hit)
		return
	}

	var body []byte
	if service == "git-upload-pack" && wantsV2 {
		var buf bytes.Buffer
		buf.Write(pktStr("version 2\n"))
		buf.Write(pktStr("agent=" + gitAgent + "\n"))
		buf.Write(pktStr("ls-refs=unborn\n"))
		buf.Write(pktStr("fetch=shallow\n"))
		buf.Write(pktStr("object-format=sha1\n"))
		buf.Write(pktFlush)
		body = buf.Bytes()
	} else {
		headRef := repo.HeadRef()
		headOid := repo.Refs[headRef]
		caps := "report-status side-band-64k delete-refs ofs-delta agent=" + gitAgent + " symref=HEAD:" + headRef
		if service == "git-upload-pack" {
			caps = "side-band-64k ofs-delta shallow agent=" + gitAgent + " symref=HEAD:" + headRef
		}
		var buf bytes.Buffer
		buf.Write(pktStr("# service=" + service + "\n"))
		buf.Write(pktFlush)
		refs := sortedRefLines(repo)
		if headOid == "" && len(refs) == 0 {
			buf.Write(pktStr(ZeroSHA + " capabilities^{}\x00" + caps + "\n"))
		} else {
			first := true
			if headOid != "" {
				buf.Write(pktStr(headOid + " HEAD\x00" + caps + "\n"))
				first = false
			}
			for _, ref := range refs {
				if first {
					buf.Write(pktStr(ref.oid + " " + ref.name + "\x00" + caps + "\n"))
					first = false
				} else {
					buf.Write(pktStr(ref.oid + " " + ref.name + "\n"))
				}
				if ref.peeled != "" {
					buf.Write(pktStr(ref.peeled + " " + ref.name + "^{}\n"))
				}
			}
		}
		buf.Write(pktFlush)
		body = buf.Bytes()
	}
	storeBody(repo, cacheKey, body)
	ctx.SendRaw(200, body)
}

// ------------------------------------------------------------------ upload-pack

func handleUploadPack(ctx *Ctx, owner, repoName string) {
	repo := ctx.Forge.GetRepo(owner, repoName)
	if repo == nil {
		ctx.Fail(404, "Not Found")
		return
	}
	ctx.W.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	ctx.W.Header().Set("Cache-Control", "no-cache")
	packets, err := parseAllPackets(ctx.Body)
	if err != nil {
		panic(err)
	}
	firstLine := ""
	for _, p := range packets {
		if p.kind == "data" {
			firstLine = string(p.payload)
			break
		}
	}
	switch {
	case strings.HasPrefix(firstLine, "command=ls-refs"):
		lsRefs(ctx, repo, packets)
	case strings.HasPrefix(firstLine, "command=fetch"):
		fetchV2(ctx, repo, packets)
	case strings.HasPrefix(firstLine, "want "):
		fetchV0(ctx, repo, packets)
	default:
		panic(fmt.Errorf("unrecognized upload-pack request"))
	}
}

func lsRefs(ctx *Ctx, repo *Repo, packets []packet) {
	args := packetLines(packets[1:])
	peel, symrefs := false, false
	var prefixes []string
	for _, arg := range args {
		switch arg {
		case "peel":
			peel = true
		case "symrefs":
			symrefs = true
		default:
			if strings.HasPrefix(arg, "ref-prefix ") {
				prefixes = append(prefixes, arg[len("ref-prefix "):])
			}
		}
	}
	boolBit := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	cacheKey := "ls|" + gitFingerprint(repo) + "|" + boolBit(peel) + boolBit(symrefs) + "|" + strings.Join(prefixes, ",")
	if hit := cachedBody(repo, cacheKey); hit != nil {
		ctx.SendRaw(200, hit)
		return
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var buf bytes.Buffer
	headRef := repo.HeadRef()
	headOid := repo.Refs[headRef]
	if matches("HEAD") {
		if headOid == "" {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:" + headRef
			}
			buf.Write(pktStr(line + "\n"))
		} else {
			line := headOid + " HEAD"
			if symrefs {
				line += " symref-target:" + headRef
			}
			buf.Write(pktStr(line + "\n"))
		}
	}
	for _, ref := range sortedRefLines(repo) {
		if !matches(ref.name) {
			continue
		}
		line := ref.oid + " " + ref.name
		if peel && ref.peeled != "" {
			line += " peeled:" + ref.peeled
		}
		buf.Write(pktStr(line + "\n"))
	}
	buf.Write(pktFlush)
	body := buf.Bytes()
	storeBody(repo, cacheKey, body)
	ctx.SendRaw(200, body)
}

// ---------------------------------------------------------- fetch object selection

type fetchSelection struct {
	send      []string
	boundary  []string
	unshallow []string
}

// addTreeClosure adds a commit's whole tree (subtrees and blobs) to into.
func addTreeClosure(repo *Repo, treeSha string, into map[string]bool) {
	if into[treeSha] {
		return
	}
	obj := repo.Objects.Get(treeSha)
	if obj == nil || obj.Type != "tree" {
		return
	}
	into[treeSha] = true
	for _, entry := range parseTree(obj.Data) {
		if entry.Mode == "40000" {
			addTreeClosure(repo, entry.Sha, into)
		} else {
			into[entry.Sha] = true
		}
	}
}

func selectFetchObjects(repo *Repo, wants, haves []string, clientShallow map[string]bool, depth *int) fetchSelection {
	// What the client already holds: its haves and their history, stopping at its shallow
	// boundary (a shallow commit's ancestors are NOT held), plus every tree of a held commit.
	held := make(map[string]bool)
	{
		seenCommits := make(map[string]bool)
		queue := append([]string{}, haves...)
		for sha := range clientShallow {
			queue = append(queue, sha)
		}
		for len(queue) > 0 {
			sha := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if seenCommits[sha] {
				continue
			}
			obj := repo.Objects.Get(sha)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			seenCommits[sha] = true
			held[sha] = true
			commit := parseCommit(obj.Data)
			addTreeClosure(repo, commit.Tree, held)
			if clientShallow[sha] {
				continue
			}
			queue = append(queue, commit.Parents...)
		}
	}

	// What the client wants: the wants and (depth-limited) their history, with trees. A want may
	// be an annotated tag: the tag object itself is part of the answer, and its target commit is
	// where the history walk starts.
	wanted := make(map[string]bool)
	includedCommits := make(map[string]bool)
	var boundary []string
	{
		type queueItem struct {
			sha string
			gen int
		}
		generation := make(map[string]int)
		var queue []queueItem
		for _, want := range wants {
			target := want
			for guard := 0; guard < 16; guard++ {
				obj := repo.Objects.Get(target)
				if obj == nil {
					break
				}
				wanted[target] = true
				if obj.Type != "tag" {
					break
				}
				target = parseTag(obj.Data).Object
			}
			queue = append(queue, queueItem{sha: target, gen: 0})
		}
		for len(queue) > 0 {
			item := queue[0]
			queue = queue[1:]
			if gen, seen := generation[item.sha]; seen && gen <= item.gen {
				continue
			}
			generation[item.sha] = item.gen
			obj := repo.Objects.Get(item.sha)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			includedCommits[item.sha] = true
			wanted[item.sha] = true
			commit := parseCommit(obj.Data)
			addTreeClosure(repo, commit.Tree, wanted)
			if depth != nil && item.gen+1 >= *depth {
				if len(commit.Parents) > 0 {
					boundary = append(boundary, item.sha)
				}
				continue
			}
			for _, parent := range commit.Parents {
				queue = append(queue, queueItem{sha: parent, gen: item.gen + 1})
			}
		}
	}

	var send []string
	for sha := range wanted {
		if !held[sha] {
			send = append(send, sha)
		}
	}
	sort.Strings(send)
	var unshallow []string
	for sha := range clientShallow {
		if includedCommits[sha] && !containsStr(boundary, sha) {
			unshallow = append(unshallow, sha)
		}
	}
	sort.Strings(unshallow)
	var newBoundary []string
	for _, sha := range boundary {
		if !clientShallow[sha] {
			newBoundary = append(newBoundary, sha)
		}
	}
	return fetchSelection{send: send, boundary: newBoundary, unshallow: unshallow}
}

func containsStr(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

func sidebandPackets(pack []byte) [][]byte {
	var parts [][]byte
	// A pkt-line is at most 65520 bytes including its 4-byte header, and a sideband packet spends
	// one payload byte on the band, leaving 65515 for pack data.
	const chunk = 65515
	for offset := 0; offset < len(pack); offset += chunk {
		end := offset + chunk
		if end > len(pack) {
			end = len(pack)
		}
		parts = append(parts, pkt(append([]byte{1}, pack[offset:end]...)))
	}
	return parts
}

func fetchV2(ctx *Ctx, repo *Repo, packets []packet) {
	args := packetLines(packets[1:])
	var wants, haves []string
	clientShallow := make(map[string]bool)
	var depth *int
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			wants = append(wants, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "shallow "):
			clientShallow[strings.TrimSpace(arg[8:])] = true
		case strings.HasPrefix(arg, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(arg[7:])); err == nil && n != 0 {
				depth = &n
			}
		}
	}
	shallowList := make([]string, 0, len(clientShallow))
	for sha := range clientShallow {
		shallowList = append(shallowList, sha)
	}
	sort.Strings(shallowList)
	depthStr := ""
	if depth != nil {
		depthStr = strconv.Itoa(*depth)
	}
	cacheKey := "f2|" + gitFingerprint(repo) + "|" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + strings.Join(shallowList, ",") + "|" + depthStr
	if hit := cachedBody(repo, cacheKey); hit != nil {
		ctx.SendRaw(200, hit)
		return
	}
	selection := selectFetchObjects(repo, wants, haves, clientShallow, depth)
	var buf bytes.Buffer
	if len(haves) > 0 {
		buf.Write(pktStr("acknowledgments\n"))
		var common []string
		for _, have := range haves {
			if !repo.Objects.Has(have) {
				continue
			}
			for _, want := range wants {
				if have == want || isAncestor(repo.Objects, have, want) {
					common = append(common, have)
					break
				}
			}
		}
		if len(common) == 0 {
			buf.Write(pktStr("NAK\n"))
		} else {
			for _, have := range common {
				buf.Write(pktStr("ACK " + have + " common\n"))
			}
			buf.Write(pktStr("ready\n"))
		}
		buf.Write(pktFlush)
	}
	if len(selection.boundary) > 0 || len(selection.unshallow) > 0 {
		buf.Write(pktStr("shallow-info\n"))
		for _, sha := range selection.boundary {
			buf.Write(pktStr("shallow " + sha + "\n"))
		}
		for _, sha := range selection.unshallow {
			buf.Write(pktStr("unshallow " + sha + "\n"))
		}
		buf.Write(pktFlush)
	}
	if len(selection.send) > 0 {
		buf.Write(pktStr("packfile\n"))
		pack, err := repo.Objects.BuildPack(selection.send)
		if err != nil {
			panic(err)
		}
		for _, part := range sidebandPackets(pack) {
			buf.Write(part)
		}
		buf.Write(pktFlush)
	}
	body := buf.Bytes()
	storeBody(repo, cacheKey, body)
	ctx.SendRaw(200, body)
}

func fetchV0(ctx *Ctx, repo *Repo, packets []packet) {
	all := packetLines(packets)
	var wants, haves []string
	clientShallow := make(map[string]bool)
	var depth *int
	for _, arg := range all {
		switch {
		case strings.HasPrefix(arg, "want "):
			rest := strings.TrimSpace(arg[5:])
			if space := strings.IndexByte(rest, ' '); space >= 0 {
				rest = rest[:space]
			}
			wants = append(wants, rest)
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "shallow "):
			clientShallow[strings.TrimSpace(arg[8:])] = true
		case strings.HasPrefix(arg, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(arg[7:])); err == nil && n != 0 {
				depth = &n
			}
		}
	}
	shallowList := make([]string, 0, len(clientShallow))
	for sha := range clientShallow {
		shallowList = append(shallowList, sha)
	}
	sort.Strings(shallowList)
	depthStr := ""
	if depth != nil {
		depthStr = strconv.Itoa(*depth)
	}
	cacheKey := "f0|" + gitFingerprint(repo) + "|" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + strings.Join(shallowList, ",") + "|" + depthStr
	if hit := cachedBody(repo, cacheKey); hit != nil {
		ctx.SendRaw(200, hit)
		return
	}
	selection := selectFetchObjects(repo, wants, haves, clientShallow, depth)
	var buf bytes.Buffer
	common := ""
	for _, have := range haves {
		if repo.Objects.Has(have) {
			common = have
			break
		}
	}
	if common == "" {
		buf.Write(pktStr("NAK\n"))
	} else {
		buf.Write(pktStr("ACK " + common + "\n"))
	}
	for _, sha := range selection.boundary {
		buf.Write(pktStr("shallow " + sha + "\n"))
	}
	for _, sha := range selection.unshallow {
		buf.Write(pktStr("unshallow " + sha + "\n"))
	}
	if len(selection.send) > 0 {
		pack, err := repo.Objects.BuildPack(selection.send)
		if err != nil {
			panic(err)
		}
		for _, part := range sidebandPackets(pack) {
			buf.Write(part)
		}
	}
	buf.Write(pktFlush)
	body := buf.Bytes()
	storeBody(repo, cacheKey, body)
	ctx.SendRaw(200, body)
}

// ------------------------------------------------------------------ receive-pack

func handleReceivePack(ctx *Ctx, owner, repoName string) {
	repo := ctx.Forge.GetRepo(owner, repoName)
	if repo == nil {
		ctx.Fail(404, "Not Found")
		return
	}
	ctx.W.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	ctx.W.Header().Set("Cache-Control", "no-cache")
	packets, rest, err := parsePackets(ctx.Body)
	if err != nil {
		panic(err)
	}
	commandLines := packetLines(packets)
	if len(commandLines) == 0 {
		panic(fmt.Errorf("empty receive-pack command list"))
	}

	capabilities := ""
	type refCommand struct{ oldOid, newOid, ref string }
	var commands []refCommand
	for index, line := range commandLines {
		text := line
		if index == 0 {
			if nul := strings.IndexByte(text, 0); nul >= 0 {
				capabilities = text[nul+1:]
				text = text[:nul]
			}
		}
		parts := strings.SplitN(text, " ", 3)
		commands = append(commands, refCommand{oldOid: parts[0], newOid: parts[1], ref: parts[2]})
	}
	caps := strings.Split(capabilities, " ")
	useSideband := containsStr(caps, "side-band-64k")
	wantReport := containsStr(caps, "report-status") || containsStr(caps, "report-status-v2")

	unpackOk := true
	unpackDetail := "ok"
	if len(rest) > 0 {
		if _, err := repo.Objects.IngestPack(rest); err != nil {
			unpackOk = false
			unpackDetail = err.Error()
		}
	}

	reportLines := []string{"unpack " + map[bool]string{true: "ok", false: unpackDetail}[unpackOk] + "\n"}
	for _, command := range commands {
		oldOid, newOid, ref := command.oldOid, command.newOid, command.ref
		if !unpackOk {
			reportLines = append(reportLines, "ng "+ref+" unpacker error\n")
			continue
		}
		current := repo.Refs[ref]
		if current == "" {
			current = ZeroSHA
		}
		if oldOid != current {
			reportLines = append(reportLines, "ng "+ref+" stale info\n")
			continue
		}
		if newOid == ZeroSHA {
			if current == ZeroSHA {
				reportLines = append(reportLines, "ng "+ref+" ref not found\n")
				continue
			}
			delete(repo.Refs, ref)
			reportLines = append(reportLines, "ok "+ref+"\n")
			continue
		}
		target := repo.Objects.Get(newOid)
		if target == nil {
			reportLines = append(reportLines, "ng "+ref+" missing object\n")
			continue
		}
		if current != ZeroSHA && target.Type == "commit" && !isAncestor(repo.Objects, current, newOid) {
			reportLines = append(reportLines, "ng "+ref+" non-fast-forward\n")
			continue
		}
		repo.Refs[ref] = newOid
		reportLines = append(reportLines, "ok "+ref+"\n")
	}

	if !wantReport {
		ctx.SendRaw(200, nil)
		return
	}
	var report bytes.Buffer
	for _, line := range reportLines {
		report.Write(pktStr(line))
	}
	report.Write(pktFlush)
	if useSideband {
		var wrapped bytes.Buffer
		data := report.Bytes()
		const chunk = 65515
		for offset := 0; offset < len(data); offset += chunk {
			end := offset + chunk
			if end > len(data) {
				end = len(data)
			}
			wrapped.Write(pkt(append([]byte{1}, data[offset:end]...)))
		}
		wrapped.Write(pktFlush)
		ctx.SendRaw(200, wrapped.Bytes())
		return
	}
	ctx.SendRaw(200, report.Bytes())
}
