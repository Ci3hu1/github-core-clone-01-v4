package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestGitObjectIDBlob(t *testing.T) {
	// `printf hello | git hash-object --stdin` == ce013625030ba8dba906f756967f9e9ca394464a
	if got := gitObjectID("blob", []byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("blob oid = %s", got)
	}
}

func TestZlibDeflateInflateRoundTrip(t *testing.T) {
	data := []byte("hello world, hello world, hello world, hello world")
	z := zlibDeflate(data)
	out, end, err := inflateZlibAt(z, 0, len(data))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("inflate mismatch")
	}
	if end != len(z) {
		t.Fatalf("stream end %d, want %d", end, len(z))
	}
}

func TestApplyDelta(t *testing.T) {
	// base "abcdef" -> target "abcXYZdef"
	// delta: src=6, tgt=9, copy(0,3), insert "XYZ", copy(3,3)
	delta := []byte{6, 9, 0x90, 0x03, 0x03, 'X', 'Y', 'Z', 0x91, 0x03, 0x03}
	out, err := applyDelta([]byte("abcdef"), delta)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "abcXYZdef" {
		t.Fatalf("delta result %q", out)
	}
}

func TestPackRoundTrip(t *testing.T) {
	r := &Repo{objects: make(map[string]*Obj), refs: make(map[string]string)}
	blob := r.putObject("blob", []byte("hello pack\n"))
	tree := r.putObject("tree", serializeTree([]treeEntry{{mode: "100644", name: "a.txt", oid: blob}}))
	commit := r.putObject("commit", []byte("tree "+tree+"\nauthor a <a@a> 1735689600 +0000\ncommitter a <a@a> 1735689600 +0000\n\nmsg\n"))
	pack := buildPack(r, []string{commit, tree, blob})
	got, err := readPack(pack, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d objects, want 3", len(got))
	}
	for _, oid := range []string{commit, tree, blob} {
		if got[oid] == nil {
			t.Fatalf("missing object %s", oid)
		}
	}
	if got[blob].typ != "blob" || string(got[blob].data) != "hello pack\n" {
		t.Fatalf("blob mismatch")
	}
}

func TestPackDeltaRoundTrip(t *testing.T) {
	// Build a pack with a REF_DELTA entry by hand: base blob + delta blob.
	r := &Repo{objects: make(map[string]*Obj), refs: make(map[string]string)}
	base := []byte(strings.Repeat("abcdefgh", 10))
	baseOID := r.putObject("blob", base)
	// delta: src=len(base), tgt=len(base)+3, copy all, insert "XYZ"
	delta := []byte{}
	// varints
	delta = append(delta, byte(len(base)), byte(len(base)+3))
	// copy 0..80: opcode 0x80|0x10|0x01 (size byte0 + offset byte0), offset 0, size 80
	delta = append(delta, 0x91, 0x00, 0x50)
	delta = append(delta, 0x03, 'X', 'Y', 'Z')
	deltaData := applyDeltaMust(t, base, delta)
	_ = deltaData
	// Write pack manually: base object + ref-delta object.
	var pack []byte
	pack = append(pack, "PACK"...)
	pack = append(pack, 0, 0, 0, 2, 0, 0, 0, 2)
	pack = append(pack, packEntryHeader(3, len(base))...)
	pack = append(pack, zlibDeflate(base)...)
	// ref-delta entry: header type 7, size = len(delta)
	pack = append(pack, packEntryHeader(7, len(delta))...)
	raw, _ := hexDecode(baseOID)
	pack = append(pack, raw...)
	pack = append(pack, zlibDeflate(delta)...)
	sum := sha1Sum(pack)
	pack = append(pack, sum[:]...)
	got, err := readPack(pack, r)
	if err != nil {
		t.Fatal(err)
	}
	want := gitObjectID("blob", deltaData)
	if got[want] == nil {
		t.Fatalf("delta object %s not resolved", want)
	}
	if string(got[want].data) != string(deltaData) {
		t.Fatalf("delta content mismatch")
	}
}

func applyDeltaMust(t *testing.T, base, delta []byte) []byte {
	t.Helper()
	out, err := applyDelta(base, delta)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func hexDecode(s string) ([]byte, bool) {
	b := make([]byte, 20)
	for i := 0; i < 20; i++ {
		hi := hexVal(s[2*i])
		lo := hexVal(s[2*i+1])
		if hi < 0 || lo < 0 {
			return nil, false
		}
		b[i] = byte(hi<<4 | lo)
	}
	return b, true
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}

func TestJSONParser(t *testing.T) {
	v, err := parseJSON([]byte(`{"a":[1,2.5,"x\ny"],"b":{"c":true,"d":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	m := asObj(v)
	if asInt(asArr(m["a"])[0], 0) != 1 {
		t.Fatalf("a[0] wrong")
	}
	if asStr(asArr(m["a"])[2]) != "x\ny" {
		t.Fatalf("string escape wrong")
	}
	if !asBool(asObj(m["b"])["c"], false) {
		t.Fatalf("bool wrong")
	}
	if _, err := parseJSON([]byte(`{"name":`)); err == nil {
		t.Fatalf("expected parse error for truncated JSON")
	}
}

func TestStoreContentsRoundTrip(t *testing.T) {
	s := newStore()
	r := s.createRepo("demo", false, "")
	blob := r.putObject("blob", []byte("one\n"))
	root, _ := r.setFileInTree("", []string{"config", "token.css"}, blob)
	flat := r.flattenTree(root)
	if flat["config/token.css"] != blob {
		t.Fatalf("flatten mismatch")
	}
	commit := r.makeCommit(root, nil, "init")
	r.setRef("refs/heads/main", commit)
	if got := r.treeOIDAt("main"); got != root {
		t.Fatalf("treeOIDAt = %s, want %s", got, root)
	}
	// update the file
	blob2 := r.putObject("blob", []byte("two\n"))
	root2, changed := r.setFileInTree(root, []string{"config", "token.css"}, blob2)
	if !changed || root2 == root {
		t.Fatalf("update did not change tree")
	}
	if flat2 := r.flattenTree(root2); flat2["config/token.css"] != blob2 {
		t.Fatalf("updated flatten mismatch")
	}
	// delete the file
	root3, changed := r.setFileInTree(root2, []string{"config", "token.css"}, "")
	if !changed {
		t.Fatalf("delete did not change tree")
	}
	if flat3 := r.flattenTree(root3); len(flat3) != 0 {
		t.Fatalf("delete left files: %v", flat3)
	}
}

func TestPlanFetch(t *testing.T) {
	r := &Repo{objects: make(map[string]*Obj), refs: make(map[string]string)}
	mkCommit := func(parent, msg string) string {
		blob := r.putObject("blob", []byte(msg+"\n"))
		tree := r.putObject("tree", serializeTree([]treeEntry{{mode: "100644", name: "f.txt", oid: blob}}))
		var parents []string
		if parent != "" {
			parents = []string{parent}
		}
		return r.makeCommit(tree, parents, msg)
	}
	c1 := mkCommit("", "first")
	c2 := mkCommit(c1, "second")
	c3 := mkCommit(c2, "third")
	// Full fetch of tip with nothing held.
	send, sh, unsh := r.planFetch([]string{c3}, nil, nil, 0)
	if len(send) == 0 || sh != nil || unsh != nil {
		t.Fatalf("full fetch: send=%d shallow=%v unshallow=%v", len(send), sh, unsh)
	}
	// Incremental: want c3, have c2 -> only c3's objects.
	send, _, _ = r.planFetch([]string{c3}, []string{c2}, nil, 0)
	if !send[c3] || send[c2] {
		t.Fatalf("incremental fetch wrong: c3=%v c2=%v", send[c3], send[c2])
	}
	// Nothing new: want c3, have c3 -> empty.
	send, _, _ = r.planFetch([]string{c3}, []string{c3}, nil, 0)
	if len(send) != 0 {
		t.Fatalf("nothing-new fetch sent %d objects", len(send))
	}
	// Shallow depth 1: want c3 -> shallow [c3].
	send, sh, _ = r.planFetch([]string{c3}, nil, nil, 1)
	if !send[c3] || send[c2] {
		t.Fatalf("depth-1 fetch wrong")
	}
	if len(sh) != 1 || sh[0] != c3 {
		t.Fatalf("depth-1 shallow = %v, want [%s]", sh, c3)
	}
	// Deepen 2 from shallow c3: want c3, have c3, shallow [c3] -> send c2, shallow [c2], unshallow [c3].
	send, sh, unsh = r.planFetch([]string{c3}, []string{c3}, []string{c3}, 2)
	if !send[c2] || send[c3] {
		t.Fatalf("deepen fetch wrong: c2=%v c3=%v", send[c2], send[c3])
	}
	if len(sh) != 1 || sh[0] != c2 {
		t.Fatalf("deepen shallow = %v, want [%s]", sh, c2)
	}
	if len(unsh) != 1 || unsh[0] != c3 {
		t.Fatalf("deepen unshallow = %v, want [%s]", unsh, c3)
	}
}

func TestPktLine(t *testing.T) {
	p := pktLineStr("hello\n")
	if string(p) != "000ahello\n" {
		t.Fatalf("pkt-line = %q", p)
	}
	pkts, err := parsePktLines(append(p, pktFlush...))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 2 || pkts[0].kind != "data" || string(pkts[0].payload) != "hello\n" || pkts[1].kind != "flush" {
		t.Fatalf("parse mismatch: %+v", pkts)
	}
	if _, err := parsePktLines([]byte("zzzznot-a-pkt-line")); err == nil {
		t.Fatalf("expected error for malformed pkt-line")
	}
}
