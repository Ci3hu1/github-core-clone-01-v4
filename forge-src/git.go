package main

// Git smart HTTP: pkt-line framing, pack read/write (plain objects only on
// the wire out; OFS/REF deltas resolved on the way in), upload-pack v2/v0
// and receive-pack v0, advertisements.

import (
	"bytes"
	"compress/flate"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"sort"
	"strconv"
	"strings"
)

// --- pkt-line ---------------------------------------------------------------

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	var b []byte
	b = append(b, []byte(fmt.Sprintf("%04x", n))...)
	b = append(b, payload...)
	return b
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type pkt struct {
	kind    string // "data" | "flush" | "delim"
	payload []byte
}

func parsePktLines(body []byte) ([]pkt, error) {
	var out []pkt
	i := 0
	for i < len(body) {
		if i+4 > len(body) {
			return nil, errors.New("truncated pkt-line")
		}
		hdr := string(body[i : i+4])
		if hdr == "0000" {
			out = append(out, pkt{kind: "flush"})
			i += 4
			continue
		}
		if hdr == "0001" {
			out = append(out, pkt{kind: "delim"})
			i += 4
			continue
		}
		n, err := strconv.ParseInt(hdr, 16, 32)
		if err != nil || n < 4 || n > 65520 {
			return nil, errors.New("bad pkt-line length")
		}
		if i+int(n) > len(body) {
			return nil, errors.New("truncated pkt-line payload")
		}
		out = append(out, pkt{kind: "data", payload: body[i+4 : i+int(n)]})
		i += int(n)
	}
	return out, nil
}

// --- zlib inflate with exact stream-end detection --------------------------

// countByteReader feeds bytes to flate one at a time (flate does not
// over-read a ByteReader beyond its small bit buffer) and records the
// position, so the stream end can be pinned exactly.
type countByteReader struct {
	b   []byte
	pos int
}

func (r *countByteReader) ReadByte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	c := r.b[r.pos]
	r.pos++
	return c, nil
}

func (r *countByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// inflateZlibAt inflates the zlib stream starting at buf[start] whose
// uncompressed length is expectedLen. Returns the data and the offset just
// past the stream (including its 4-byte Adler-32 trailer).
func inflateZlibAt(buf []byte, start, expectedLen int) ([]byte, int, error) {
	if start+2 > len(buf) {
		return nil, 0, errors.New("short zlib stream")
	}
	if buf[start]&0x0f != 8 || (int(buf[start])<<8|int(buf[start+1]))%31 != 0 {
		return nil, 0, errors.New("bad zlib header")
	}
	br := &countByteReader{b: buf, pos: start + 2}
	fr := flate.NewReader(br)
	data, err := io.ReadAll(fr)
	fr.Close()
	if err != nil {
		return nil, 0, err
	}
	if len(data) != expectedLen {
		return nil, 0, fmt.Errorf("inflate size %d, want %d", len(data), expectedLen)
	}
	adler := adler32.Checksum(data)
	var adlerBytes [4]byte
	binary.BigEndian.PutUint32(adlerBytes[:], adler)
	// The stream end is at most a few bytes behind br.pos (flate's bit
	// buffer). Scan candidates upwards and verify by re-inflating: the
	// first candidate whose trailer is the Adler-32 and whose
	// re-inflation reproduces the data is the true end (any shorter
	// slice is a truncated deflate stream and cannot verify).
	hi := br.pos
	if hi > len(buf) {
		hi = len(buf)
	}
	lo := start + 2
	if hi-lo > 64 {
		lo = hi - 64
	}
	for cand := lo; cand <= hi; cand++ {
		if cand+4 > len(buf) {
			continue
		}
		if !bytes.Equal(buf[cand:cand+4], adlerBytes[:]) {
			continue
		}
		// Re-inflate verification.
		vr := flate.NewReader(bytes.NewReader(buf[start+2 : cand]))
		vdata, verr := io.ReadAll(vr)
		vr.Close()
		if verr == nil && bytes.Equal(vdata, data) {
			return data, cand + 4, nil
		}
	}
	return nil, 0, errors.New("zlib stream end not found")
}

// --- delta -----------------------------------------------------------------

func applyDelta(base, delta []byte) ([]byte, error) {
	i := 0
	readVarint := func() (int, error) {
		v := 0
		shift := 0
		for {
			if i >= len(delta) {
				return 0, errors.New("short delta varint")
			}
			c := delta[i]
			i++
			v |= int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out []byte
	for i < len(delta) {
		op := delta[i]
		i++
		if op&0x80 != 0 {
			// copy from base
			off, size := 0, 0
			for b := 0; b < 4; b++ {
				if op&(1<<uint(b)) != 0 {
					if i >= len(delta) {
						return nil, errors.New("short delta copy")
					}
					off |= int(delta[i]) << (8 * uint(b))
					i++
				}
			}
			for b := 0; b < 3; b++ {
				if op&(0x10<<uint(b)) != 0 {
					if i >= len(delta) {
						return nil, errors.New("short delta copy")
					}
					size |= int(delta[i]) << (8 * uint(b))
					i++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			n := int(op)
			if i+n > len(delta) {
				return nil, errors.New("short delta insert")
			}
			out = append(out, delta[i:i+n]...)
			i += n
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if len(out) != tgtSize {
		return nil, errors.New("delta target size mismatch")
	}
	return out, nil
}

// --- pack read ---------------------------------------------------------------

var packTypeName = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// readPack parses a packfile. Returns every object it defines (deltas
// resolved). Bases for REF_DELTA not in the pack are looked up in the repo.
func readPack(buf []byte, r *Repo) (map[string]*Obj, error) {
	if len(buf) < 12 || string(buf[:4]) != "PACK" {
		return nil, errors.New("bad pack header")
	}
	version := binary.BigEndian.Uint32(buf[4:8])
	if version != 2 && version != 3 {
		return nil, errors.New("unsupported pack version")
	}
	count := int(binary.BigEndian.Uint32(buf[8:12]))
	out := make(map[string]*Obj)
	byOffset := make(map[int]string) // pack offset -> oid
	pos := 12
	for n := 0; n < count; n++ {
		entryStart := pos
		if pos >= len(buf) {
			return nil, errors.New("truncated pack entry")
		}
		// varint type/size header
		b := buf[pos]
		pos++
		typ := int(b>>4) & 7
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= len(buf) {
				return nil, errors.New("truncated pack size")
			}
			b = buf[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		var objType string
		var data []byte
		switch typ {
		case 1, 2, 3, 4:
			objType = packTypeName[typ]
			d, end, err := inflateZlibAt(buf, pos, int(size))
			if err != nil {
				return nil, err
			}
			pos = end
			data = d
		case 6: // OFS_DELTA
			if pos >= len(buf) {
				return nil, errors.New("truncated ofs-delta")
			}
			c := buf[pos]
			pos++
			off := int64(c & 0x7f)
			for c&0x80 != 0 {
				if pos >= len(buf) {
					return nil, errors.New("truncated ofs-delta")
				}
				c = buf[pos]
				pos++
				off = ((off + 1) << 7) | int64(c&0x7f)
			}
			baseOff := entryStart - int(off)
			baseOID, ok := byOffset[baseOff]
			if !ok {
				return nil, errors.New("ofs-delta base not found")
			}
			base := out[baseOID]
			if base == nil {
				return nil, errors.New("ofs-delta base not resolved")
			}
			d, end, err := inflateZlibAt(buf, pos, int(size))
			if err != nil {
				return nil, err
			}
			pos = end
			data, err = applyDelta(base.data, d)
			if err != nil {
				return nil, err
			}
			objType = base.typ
		case 7: // REF_DELTA
			if pos+20 > len(buf) {
				return nil, errors.New("truncated ref-delta")
			}
			baseOID := hex.EncodeToString(buf[pos : pos+20])
			pos += 20
			var base *Obj
			if o, ok := out[baseOID]; ok {
				base = o
			} else if r != nil {
				base = r.objects[baseOID]
			}
			if base == nil {
				return nil, errors.New("ref-delta base not found")
			}
			d, end, err := inflateZlibAt(buf, pos, int(size))
			if err != nil {
				return nil, err
			}
			pos = end
			data, err = applyDelta(base.data, d)
			if err != nil {
				return nil, err
			}
			objType = base.typ
		default:
			return nil, fmt.Errorf("unknown pack type %d", typ)
		}
		oid := gitObjectID(objType, data)
		out[oid] = &Obj{typ: objType, data: data}
		byOffset[entryStart] = oid
	}
	return out, nil
}

// --- pack write --------------------------------------------------------------

// zlibDeflate returns a zlib stream (header 0x78 0x01, BestSpeed) for data.
func zlibDeflate(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	w.Write(data)
	w.Close()
	out := make([]byte, 0, buf.Len()+6)
	out = append(out, 0x78, 0x01)
	out = append(out, buf.Bytes()...)
	var a [4]byte
	binary.BigEndian.PutUint32(a[:], adler32.Checksum(data))
	out = append(out, a[:]...)
	return out
}

func packEntryHeader(typ int, size int) []byte {
	var out []byte
	b0 := byte((typ << 4) | (size & 0x0f))
	size >>= 4
	if size > 0 {
		b0 |= 0x80
	}
	out = append(out, b0)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

var packTypeID = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

// buildPack writes a version-2 pack of plain objects for the given oids
// (sorted for determinism), with a SHA-1 trailer.
func buildPack(r *Repo, oids []string) []byte {
	sort.Strings(oids)
	present := oids[:0]
	for _, oid := range oids {
		if r.objects[oid] != nil {
			present = append(present, oid)
		}
	}
	oids = present
	var body []byte
	body = append(body, "PACK"...)
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(oids)))
	body = append(body, hdr[:]...)
	for _, oid := range oids {
		o := r.objects[oid]
		body = append(body, packEntryHeader(packTypeID[o.typ], len(o.data))...)
		body = append(body, zlibDeflate(o.data)...)
	}
	sum := sha1Sum(body)
	return append(body, sum[:]...)
}

func sha1Sum(b []byte) [20]byte {
	return sha1.Sum(b)
}

// --- advertisements -----------------------------------------------------------

func (r *Repo) refLinesV0(service string) [][]byte {
	// Returns pkt-lines for a v0 advertisement (after preamble+flush).
	var lines [][]byte
	capsUpload := "side-band-64k thin-pack ofs-delta multi_ack shallow agent=" + agentName + " symref=HEAD:refs/heads/main"
	capsReceive := "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=" + agentName + " symref=HEAD:refs/heads/main"
	caps := capsUpload
	if service == "git-receive-pack" {
		caps = capsReceive
	}
	// Collect refs sorted: HEAD first, then refs sorted by name.
	type refEnt struct {
		oid, name string
	}
	var refs []refEnt
	for _, name := range r.refOrder {
		refs = append(refs, refEnt{r.refs[name], name})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	headTip, headOK := r.defaultTip()
	// peeled tag targets
	peeled := map[string]string{}
	for _, e := range refs {
		if strings.HasPrefix(e.name, "refs/tags/") {
			if o := r.objects[e.oid]; o != nil && o.typ == "tag" {
				if tgt := parseTagTarget(o.data); tgt != "" {
					peeled[e.name] = tgt
				}
			}
		}
	}
	first := true
	emit := func(oid, name string) {
		line := oid + " " + name
		if first {
			line += "\x00" + caps
			first = false
		}
		lines = append(lines, pktLineStr(line+"\n"))
	}
	if headOK {
		emit(headTip, "HEAD")
	} else if len(refs) == 0 {
		emit(zeroOID, "capabilities^{}")
	} else {
		// No default-branch tip but other refs exist: still emit a HEAD
		// line is wrong; emit capabilities pseudo-ref first instead.
		emit(zeroOID, "capabilities^{}")
	}
	for _, e := range refs {
		emit(e.oid, e.name)
		if p, ok := peeled[e.name]; ok {
			lines = append(lines, pktLineStr(p+" "+e.name+"^{}\n"))
		}
	}
	return lines
}

func parseTagTarget(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "object ") {
			return line[7:]
		}
	}
	return ""
}

func advertisementV2() []byte {
	var b []byte
	b = append(b, pktLineStr("version 2\n")...)
	b = append(b, pktLineStr("ls-refs=unborn\n")...)
	b = append(b, pktLineStr("fetch=shallow\n")...)
	b = append(b, pktLineStr("object-format=sha1\n")...)
	b = append(b, pktLineStr("agent="+agentName+"\n")...)
	b = append(b, pktFlush...)
	return b
}

// lsRefsV2 answers a v2 ls-refs request body.
func (r *Repo) lsRefsV2(body []byte) ([]byte, error) {
	pkts, err := parsePktLines(body)
	if err != nil {
		return nil, err
	}
	// ls-refs may carry several ref-prefix arguments; a ref matches when
	// it matches ANY of them (git clone sends HEAD, refs/heads/, refs/tags/).
	var prefixes []string
	for _, p := range pkts {
		if p.kind != "data" {
			continue
		}
		line := string(p.payload)
		if strings.HasPrefix(line, "ref-prefix ") {
			prefixes = append(prefixes, strings.TrimSuffix(line[len("ref-prefix "):], "\n"))
		}
	}
	matches := func(name string) bool {
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
	var out []byte
	headTip, headOK := r.defaultTip()
	// HEAD line (only when it matches a prefix, or no prefix).
	if matches("HEAD") {
		if headOK {
			out = append(out, pktLineStr(headTip+" HEAD symref-target:refs/heads/main\n")...)
		} else {
			out = append(out, pktLineStr("unborn HEAD symref-target:refs/heads/main\n")...)
		}
	}
	// Refs sorted.
	names := make([]string, 0, len(r.refs))
	for n := range r.refs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if !matches(name) {
			continue
		}
		oid := r.refs[name]
		line := oid + " " + name
		if strings.HasPrefix(name, "refs/tags/") {
			if o := r.objects[oid]; o != nil && o.typ == "tag" {
				if tgt := parseTagTarget(o.data); tgt != "" {
					line += " peeled:" + tgt
				}
			}
		}
		out = append(out, pktLineStr(line+"\n")...)
	}
	out = append(out, pktFlush...)
	return out, nil
}

// --- upload-pack ---------------------------------------------------------------

// fetchPack returns the cached (or freshly built) pack for this fetch,
// with single-flight: concurrent identical fetches share one build.
func (r *Repo) fetchPack(wants, haves, shallow []string, depth int) (pack []byte, shallowOut, unshallowOut []string) {
	keyParts := []string{"g" + strconv.FormatUint(r.gen, 10), "d" + strconv.Itoa(depth)}
	for _, w := range append(append([]string{}, wants...), haves...) {
		keyParts = append(keyParts, w)
	}
	for _, s := range shallow {
		keyParts = append(keyParts, "s"+s)
	}
	sort.Strings(keyParts[2:])
	key := strings.Join(keyParts, "|")
	r.packMu.Lock()
	if ent, ok := r.packCache[key]; ok {
		r.packMu.Unlock()
		<-ent.done
		return ent.pack, ent.shallow, ent.unshallow
	}
	ent := &packEntry{done: make(chan struct{})}
	r.packCache[key] = ent
	r.packMu.Unlock()

	send, sh, unsh := r.planFetch(wants, haves, shallow, depth)
	var oids []string
	for oid := range send {
		oids = append(oids, oid)
	}
	ent.pack = buildPack(r, oids)
	ent.shallow = sh
	ent.unshallow = unsh
	close(ent.done)
	return ent.pack, sh, unsh
}

func sidebandPackets(pack []byte) []byte {
	var out []byte
	const maxChunk = 65515
	for len(pack) > 0 {
		n := len(pack)
		if n > maxChunk {
			n = maxChunk
		}
		chunk := make([]byte, 0, n+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, pack[:n]...)
		out = append(out, pktLine(chunk)...)
		pack = pack[n:]
	}
	return out
}

// uploadPackV2 handles a v2 fetch request body.
func (r *Repo) uploadPackV2(body []byte) ([]byte, error) {
	pkts, err := parsePktLines(body)
	if err != nil {
		return nil, err
	}
	var command string
	var wants, haves, shallow []string
	depth := 0
	inArgs := false
	for _, p := range pkts {
		switch p.kind {
		case "delim":
			inArgs = true
		case "flush":
			inArgs = false
		case "data":
			line := strings.TrimSuffix(string(p.payload), "\n")
			if !inArgs {
				if strings.HasPrefix(line, "command=") {
					command = line[len("command="):]
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "want "):
				wants = append(wants, line[5:])
			case strings.HasPrefix(line, "have "):
				haves = append(haves, line[5:])
			case strings.HasPrefix(line, "shallow "):
				shallow = append(shallow, line[8:])
			case strings.HasPrefix(line, "deepen "):
				depth, _ = strconv.Atoi(line[7:])
			}
		}
	}
	if command == "ls-refs" {
		return r.lsRefsV2(body)
	}
	if command != "fetch" {
		return nil, errors.New("unknown v2 command")
	}
	pack, sh, unsh := r.fetchPack(wants, haves, shallow, depth)
	var out []byte
	// The acknowledgments section is only sent when the client sent
	// "have" lines: a client that sent no haves (e.g. a fresh clone)
	// expects the response to start with shallow-info/packfile, and
	// errors on a stray acknowledgments section.
	if len(haves) > 0 {
		out = append(out, pktLineStr("acknowledgments\n")...)
		out = append(out, pktLineStr("NAK\n")...)
		out = append(out, pktDelim...)
	}
	if len(sh) > 0 || len(unsh) > 0 {
		out = append(out, pktLineStr("shallow-info\n")...)
		for _, s := range sh {
			out = append(out, pktLineStr("shallow "+s+"\n")...)
		}
		for _, s := range unsh {
			out = append(out, pktLineStr("unshallow "+s+"\n")...)
		}
		out = append(out, pktDelim...)
	}
	out = append(out, pktLineStr("packfile\n")...)
	out = append(out, sidebandPackets(pack)...)
	out = append(out, pktFlush...)
	return out, nil
}

// uploadPackV0 handles a v0 fetch request body.
func (r *Repo) uploadPackV0(body []byte) ([]byte, error) {
	pkts, err := parsePktLines(body)
	if err != nil {
		return nil, err
	}
	var wants, haves, shallow []string
	depth := 0
	for _, p := range pkts {
		switch p.kind {
		case "flush":
			// section boundary; want/have lines are distinct anyway
		case "data":
			line := strings.TrimSuffix(string(p.payload), "\n")
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			switch fields[0] {
			case "want":
				if len(fields) >= 2 {
					wants = append(wants, fields[1])
				}
			case "have":
				if len(fields) >= 2 {
					haves = append(haves, fields[1])
				}
			case "shallow":
				if len(fields) >= 2 {
					shallow = append(shallow, fields[1])
				}
			case "deepen":
				if len(fields) >= 2 {
					depth, _ = strconv.Atoi(fields[1])
				}
			}
		}
	}
	pack, sh, unsh := r.fetchPack(wants, haves, shallow, depth)
	var out []byte
	out = append(out, pktLineStr("NAK\n")...)
	for _, s := range sh {
		out = append(out, pktLineStr("shallow "+s+"\n")...)
	}
	for _, s := range unsh {
		out = append(out, pktLineStr("unshallow "+s+"\n")...)
	}
	out = append(out, sidebandPackets(pack)...)
	out = append(out, pktFlush...)
	return out, nil
}

// --- receive-pack ---------------------------------------------------------------

// receivePack handles a v0 receive-pack POST. Returns the framed report.
func (r *Repo) receivePack(body []byte) ([]byte, error) {
	type cmd struct {
		old, new, ref string
	}
	var cmds []cmd
	useSideband := false
	// Parse pkt-lines only up to the first flush; everything after it is
	// the raw pack (its bytes are not pkt-lines and must not be parsed).
	off := 0
	packStart := -1
	for off < len(body) {
		if off+4 > len(body) {
			return nil, errors.New("truncated pkt-line")
		}
		hdr := string(body[off : off+4])
		if hdr == "0000" {
			packStart = off + 4
			break
		}
		n, err := strconv.ParseInt(hdr, 16, 32)
		if err != nil || n < 4 || n > 65520 {
			return nil, errors.New("bad pkt-line length")
		}
		if off+int(n) > len(body) {
			return nil, errors.New("truncated pkt-line payload")
		}
		rawLine := string(body[off+4 : off+int(n)])
		if idx := strings.IndexByte(rawLine, 0); idx >= 0 {
			for _, cap := range strings.Fields(rawLine[idx+1:]) {
				if cap == "side-band-64k" {
					useSideband = true
				}
			}
			rawLine = rawLine[:idx]
		}
		line := strings.TrimSuffix(rawLine, "\n")
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, errors.New("bad receive-pack command")
		}
		cmds = append(cmds, cmd{f[0], f[1], f[2]})
		off += int(n)
	}
	if packStart < 0 {
		return nil, errors.New("receive-pack missing pack")
	}
	packBytes := body[packStart:]
	var report strings.Builder
	unpackOK := true
	if len(cmds) > 0 || len(packBytes) > 0 {
		// An empty pack (delete-only push) has no pack bytes; that is fine.
		if len(packBytes) > 0 {
			objs, err := readPack(packBytes, r)
			if err != nil {
				unpackOK = false
				report.WriteString("unpack " + err.Error() + "\n")
			} else {
				for oid, o := range objs {
					if _, ok := r.objects[oid]; !ok {
						r.objects[oid] = o
					}
				}
				report.WriteString("unpack ok\n")
			}
		} else {
			report.WriteString("unpack ok\n")
		}
	} else {
		report.WriteString("unpack ok\n")
	}
	if unpackOK {
		for _, c := range cmds {
			ok, reason := r.applyPushRef(c.old, c.new, c.ref)
			if ok {
				report.WriteString("ok " + c.ref + "\n")
			} else {
				report.WriteString("ng " + c.ref + " " + reason + "\n")
			}
		}
	}
	// The report-status stream is pkt-lined, one pkt-line per line, ending
	// with a flush pkt-line (pack-protocol.txt). When the client asked for
	// side-band-64k, that stream rides inside sideband band 1; otherwise
	// it is sent bare. Each line must be its own pkt-line: packing the
	// whole report into one pkt-line makes the unpack line read as
	// "ok\nok <ref>" and fail.
	var reportStream []byte
	for _, line := range strings.SplitAfter(report.String(), "\n") {
		if line == "" {
			continue
		}
		reportStream = append(reportStream, pktLineStr(line)...)
	}
	reportStream = append(reportStream, pktFlush...)
	var outer []byte
	if useSideband {
		chunked := reportStream
		for len(chunked) > 0 {
			n := len(chunked)
			if n > 65515 {
				n = 65515
			}
			chunk := make([]byte, 0, n+1)
			chunk = append(chunk, 1)
			chunk = append(chunk, chunked[:n]...)
			outer = append(outer, pktLine(chunk)...)
			chunked = chunked[n:]
		}
	} else {
		outer = append(outer, reportStream...)
	}
	outer = append(outer, pktFlush...)
	return outer, nil
}

// applyPushRef applies one ref update. Returns ok and refusal reason.
func (r *Repo) applyPushRef(old, new, ref string) (bool, string) {
	cur, exists := r.refs[ref]
	isDelete := new == zeroOID
	if isDelete {
		if !exists {
			return false, "non-fast-forward"
		}
		if old != zeroOID && old != cur {
			return false, "non-fast-forward"
		}
		r.deleteRef(ref)
		r.bump()
		return true, ""
	}
	// new object must exist and be a commit (or tag for refs/tags).
	obj := r.objects[new]
	if obj == nil {
		return false, "non-fast-forward"
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		if obj.typ != "commit" && obj.typ != "tag" {
			return false, "non-fast-forward"
		}
		if exists && old != cur {
			return false, "non-fast-forward"
		}
		if exists && old == cur {
			// Re-pointing an existing tag to a different object: refuse
			// unless it is the same object.
			if new == cur {
				return true, ""
			}
			return false, "non-fast-forward"
		}
		r.setRef(ref, new)
		r.bump()
		return true, ""
	}
	if obj.typ != "commit" {
		return false, "non-fast-forward"
	}
	if !exists {
		// create
		if old != zeroOID {
			return false, "non-fast-forward"
		}
		r.setRef(ref, new)
		r.bump()
		return true, ""
	}
	// update: old must match tip, new must descend from old.
	if old != cur {
		return false, "non-fast-forward"
	}
	if !r.isAncestor(old, new) {
		return false, "non-fast-forward"
	}
	r.setRef(ref, new)
	r.bump()
	return true, ""
}
