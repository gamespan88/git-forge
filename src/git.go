// The git object core of the forge, in Go: an in-memory object store with SHA-1 ids, tree and
// commit codecs, packfile reading (including ofs/ref deltas) and packfile writing. Standard
// library only (crypto/sha1 for ids, compress/zlib for the deflate streams inside packs).
//
// Objects are stored by their raw content: a commit is its header block plus message, a tree is
// its sequence of "<mode> <name>\0<20-byte id>" entries, a blob is the file bytes. The id of an
// object is sha1("<type> <length>\0" + content), exactly as git computes it, so ids the suite
// computes on its side match the ids this store computes here.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
)

const ZeroSHA = "0000000000000000000000000000000000000000"

type GitObject struct {
	Type string // "commit" | "tree" | "blob" | "tag"
	Data []byte
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func objectID(objType string, data []byte) string {
	header := []byte(fmt.Sprintf("%s %d\x00", objType, len(data)))
	h := sha1.New()
	h.Write(header)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// inflateAt inflates one zlib stream out of buf starting at offset, knowing its inflated size,
// and reports where the stream ends. zlib readers ignore trailing bytes, so "inflates to the
// expected size" is a monotone predicate of the slice length: the shortest slice that satisfies
// it ends exactly at the end of this stream. Probe exponentially, then binary-search the edge.
func inflateAt(buf []byte, offset int, expectedSize int) ([]byte, int, error) {
	remaining := len(buf) - offset
	tryLen := func(n int) []byte {
		r, err := zlib.NewReader(bytes.NewReader(buf[offset : offset+n]))
		if err != nil {
			return nil
		}
		out, err := io.ReadAll(io.LimitReader(r, int64(expectedSize)+1))
		_ = r.Close()
		if err != nil || len(out) != expectedSize {
			return nil
		}
		return out
	}
	lo := 0
	hi := expectedSize*6/10 + 16
	if hi > remaining {
		hi = remaining
	}
	if hi < 16 && remaining >= 16 {
		hi = 16
	}
	out := tryLen(hi)
	for out == nil {
		lo = hi
		if hi >= remaining {
			return nil, 0, fmt.Errorf("pack object does not inflate to its stated size")
		}
		hi *= 2
		if hi > remaining {
			hi = remaining
		}
		out = tryLen(hi)
	}
	for lo+1 < hi {
		mid := (lo + hi) >> 1
		if midOut := tryLen(mid); midOut == nil {
			lo = mid
		} else {
			hi = mid
			out = midOut
		}
	}
	return out, offset + hi, nil
}

// applyDelta applies a git delta (the pack ofs-delta / ref-delta format) to a base object.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() int {
		value := 0
		shift := uint(0)
		for {
			b := delta[pos]
			pos++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	sourceSize := readVarint()
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch: %d != %d", sourceSize, len(base))
	}
	targetSize := readVarint()
	var out bytes.Buffer
	for pos < len(delta) {
		op := delta[pos]
		pos++
		switch {
		case op&0x80 != 0:
			offset, size := 0, 0
			if op&0x01 != 0 {
				offset = int(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				offset |= int(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				offset |= int(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				offset |= int(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				size = int(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				size |= int(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				size |= int(delta[pos]) << 16
				pos++
			}
			if size == 0 {
				size = 0x10000
			}
			out.Write(base[offset : offset+size])
		case op != 0:
			out.Write(delta[pos : pos+int(op)])
			pos += int(op)
		default:
			return nil, fmt.Errorf("delta opcode 0 is reserved")
		}
	}
	if out.Len() != targetSize {
		return nil, fmt.Errorf("delta target size mismatch")
	}
	return out.Bytes(), nil
}

var packTypes = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
var packTypeNums = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

type ObjectStore struct {
	Objects map[string]*GitObject
}

func NewObjectStore() *ObjectStore {
	return &ObjectStore{Objects: make(map[string]*GitObject)}
}

func (s *ObjectStore) Has(sha string) bool {
	_, ok := s.Objects[sha]
	return ok
}

func (s *ObjectStore) Get(sha string) *GitObject {
	return s.Objects[sha]
}

func (s *ObjectStore) Write(objType string, data []byte) string {
	sha := objectID(objType, data)
	if _, ok := s.Objects[sha]; !ok {
		s.Objects[sha] = &GitObject{Type: objType, Data: data}
	}
	return sha
}

type pendingDelta struct {
	kind       string // "ofs" | "ref"
	baseOffset int
	baseSha    string
	atOffset   int
	delta      []byte
}

// IngestPack reads every object out of a packfile into this store. Deltas are resolved against
// objects already in the store and objects read earlier from the same pack, in whatever order
// they settle. Returns the ids written.
func (s *ObjectStore) IngestPack(pack []byte) ([]string, error) {
	if len(pack) < 12+20 || string(pack[0:4]) != "PACK" {
		return nil, fmt.Errorf("not a packfile")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	pos := 12
	var pending []*pendingDelta
	offsetToSha := make(map[int]string)
	var written []string
	place := func(objType string, data []byte, atOffset int) string {
		sha := s.Write(objType, data)
		offsetToSha[atOffset] = sha
		written = append(written, sha)
		return sha
	}
	for i := 0; i < count; i++ {
		start := pos
		b := pack[pos]
		pos++
		typeNum := int(b>>4) & 0x7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = pack[pos]
			pos++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		switch typeNum {
		case 6: // ofs-delta
			c := pack[pos]
			pos++
			baseOffset := int(c & 0x7f)
			for c&0x80 != 0 {
				c = pack[pos]
				pos++
				baseOffset = ((baseOffset + 1) << 7) | int(c&0x7f)
			}
			data, next, err := inflateAt(pack, pos, size)
			if err != nil {
				return nil, err
			}
			pos = next
			pending = append(pending, &pendingDelta{kind: "ofs", baseOffset: start - baseOffset, atOffset: start, delta: data})
		case 7: // ref-delta
			baseSha := hex.EncodeToString(pack[pos : pos+20])
			pos += 20
			data, next, err := inflateAt(pack, pos, size)
			if err != nil {
				return nil, err
			}
			pos = next
			pending = append(pending, &pendingDelta{kind: "ref", baseSha: baseSha, atOffset: start, delta: data})
		default:
			objType, ok := packTypes[typeNum]
			if !ok {
				return nil, fmt.Errorf("unknown pack object type %d", typeNum)
			}
			data, next, err := inflateAt(pack, pos, size)
			if err != nil {
				return nil, err
			}
			pos = next
			place(objType, data, start)
		}
	}
	for len(pending) > 0 {
		progressed := false
		for i := len(pending) - 1; i >= 0; i-- {
			item := pending[i]
			var baseSha string
			if item.kind == "ofs" {
				baseSha = offsetToSha[item.baseOffset]
			} else {
				baseSha = item.baseSha
			}
			if baseSha == "" {
				continue
			}
			base := s.Objects[baseSha]
			if base == nil {
				continue
			}
			data, err := applyDelta(base.Data, item.delta)
			if err != nil {
				return nil, err
			}
			place(base.Type, data, item.atOffset)
			pending = append(pending[:i], pending[i+1:]...)
			progressed = true
		}
		if !progressed {
			return nil, fmt.Errorf("%d pack delta(s) have no base in the store", len(pending))
		}
	}
	return written, nil
}

// BuildPack builds a packfile holding the given objects whole (no deltas), in the order given.
func (s *ObjectStore) BuildPack(shas []string) ([]byte, error) {
	var body bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	header[7] = 2
	header[8] = byte(len(shas) >> 24)
	header[9] = byte(len(shas) >> 16)
	header[10] = byte(len(shas) >> 8)
	header[11] = byte(len(shas))
	body.Write(header)
	for _, sha := range shas {
		obj := s.Objects[sha]
		if obj == nil {
			return nil, fmt.Errorf("cannot pack missing object %s", sha)
		}
		size := len(obj.Data)
		first := byte(packTypeNums[obj.Type]<<4) | byte(size&0x0f)
		size >>= 4
		b := first
		for size > 0 {
			body.WriteByte(b | 0x80)
			b = byte(size & 0x7f)
			size >>= 7
		}
		body.WriteByte(b)
		var zbuf bytes.Buffer
		zw := zlib.NewWriter(&zbuf)
		_, _ = zw.Write(obj.Data)
		_ = zw.Close()
		body.Write(zbuf.Bytes())
	}
	trailer := sha1.Sum(body.Bytes())
	body.Write(trailer[:])
	return body.Bytes(), nil
}

// ------------------------------------------------------------- trees and commits

type TreeEntry struct {
	Mode string // e.g. "100644", "100755", "40000" (git writes tree modes without the leading 0)
	Name string
	Sha  string
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	pos := 0
	for pos < len(data) {
		space := bytes.IndexByte(data[pos:], 0x20) + pos
		nul := bytes.IndexByte(data[space:], 0x00) + space
		entries = append(entries, TreeEntry{
			Mode: string(data[pos:space]),
			Name: string(data[space+1 : nul]),
			Sha:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		pos = nul + 21
	}
	return entries
}

func serializeTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	// Git orders tree entries by name, with directories compared as if they ended in "/".
	sort.SliceStable(sorted, func(i, j int) bool {
		ka, kb := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			ka += "/"
		}
		if sorted[j].Mode == "40000" {
			kb += "/"
		}
		return ka < kb
	})
	var out bytes.Buffer
	for _, e := range sorted {
		out.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.Sha)
		out.Write(raw)
	}
	return out.Bytes()
}

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) CommitInfo {
	text := string(data)
	head, message := text, ""
	if split := indexOf(text, "\n\n"); split >= 0 {
		head, message = text[:split], text[split+2:]
	}
	info := CommitInfo{Message: message}
	for _, line := range splitLines(head) {
		switch {
		case hasPrefix(line, "tree "):
			info.Tree = trimSpace(line[5:])
		case hasPrefix(line, "parent "):
			info.Parents = append(info.Parents, trimSpace(line[7:]))
		case hasPrefix(line, "author "):
			info.Author = line[7:]
		case hasPrefix(line, "committer "):
			info.Committer = line[10:]
		}
	}
	return info
}

type TagInfo struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) TagInfo {
	text := string(data)
	head, message := text, ""
	if split := indexOf(text, "\n\n"); split >= 0 {
		head, message = text[:split], text[split+2:]
	}
	headers := make(map[string]string)
	for _, line := range splitLines(head) {
		if space := indexOf(line, " "); space > 0 {
			headers[line[:space]] = line[space+1:]
		}
	}
	return TagInfo{
		Object:  headers["object"],
		Type:    headers["type"],
		Tag:     headers["tag"],
		Tagger:  headers["tagger"],
		Message: message,
	}
}

func serializeCommit(info CommitInfo) []byte {
	var out bytes.Buffer
	out.WriteString("tree " + info.Tree + "\n")
	for _, parent := range info.Parents {
		out.WriteString("parent " + parent + "\n")
	}
	out.WriteString("author " + info.Author + "\n")
	out.WriteString("committer " + info.Committer + "\n")
	out.WriteString("\n" + info.Message)
	return out.Bytes()
}

// ident formats an ident line the way git does: "Name <email> <unix-seconds> <tz>".
func ident(name, email string, whenSec int64) string {
	return fmt.Sprintf("%s <%s> %d +0000", name, email, whenSec)
}

// ancestors returns every ancestor of tip, tip first, following all parents.
func ancestors(store *ObjectStore, tip string) []string {
	seen := make(map[string]bool)
	var order []string
	queue := []string{tip}
	for len(queue) > 0 {
		sha := queue[0]
		queue = queue[1:]
		if seen[sha] || sha == ZeroSHA {
			continue
		}
		obj := store.Get(sha)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		seen[sha] = true
		order = append(order, sha)
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	return order
}

// isAncestor reports whether maybeAncestor is tip or one of its ancestors. Depth-first with
// early exit: the common question ("is the ref's current tip an ancestor of the proposed new
// tip?") is answered by the first parent step; only a true non-ancestor pays for the whole walk.
func isAncestor(store *ObjectStore, maybeAncestor, tip string) bool {
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[sha] || sha == ZeroSHA {
			continue
		}
		seen[sha] = true
		obj := store.Get(sha)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		if sha == maybeAncestor {
			return true
		}
		stack = append(stack, parseCommit(obj.Data).Parents...)
	}
	return false
}

// reachableObjects returns all objects reachable from the given commit tips: the commits, their
// trees and blobs, and their history.
func reachableObjects(store *ObjectStore, tips []string) []string {
	out := make(map[string]bool)
	var visitTree func(sha string)
	visitTree = func(sha string) {
		if out[sha] {
			return
		}
		obj := store.Get(sha)
		if obj == nil {
			return
		}
		out[sha] = true
		if obj.Type == "tree" {
			for _, entry := range parseTree(obj.Data) {
				visitTree(entry.Sha)
			}
		}
	}
	for _, tip := range tips {
		for _, commitSha := range ancestors(store, tip) {
			out[commitSha] = true
			if commit := store.Get(commitSha); commit != nil {
				visitTree(parseCommit(commit.Data).Tree)
			}
		}
	}
	result := make([]string, 0, len(out))
	for sha := range out {
		result = append(result, sha)
	}
	return result
}

// ------------------------------------------------------------- small string helpers

func indexOf(s, sub string) int {
	return bytes.Index([]byte(s), []byte(sub))
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func trimSpace(s string) string {
	return string(bytes.TrimSpace([]byte(s)))
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
