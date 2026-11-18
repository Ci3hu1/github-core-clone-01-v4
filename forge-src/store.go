package main

// In-memory store: repos, objects, issues, pulls, labels, comments.
// One global RWMutex guards the store; reads take RLock, mutations Lock.
// A render never sees a half-applied mutation: mutations complete under
// the write lock before any response bytes are produced.

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Obj struct {
	typ  string // "blob" | "tree" | "commit" | "tag"
	data []byte
}

type Label struct {
	id    int
	name  string
	color string
}

type Comment struct {
	id      int
	body    string
	issueNo int
}

type Issue struct {
	id       int
	number   int
	title    string
	body     string
	state    string // "open" | "closed"
	labels   []string
	comments []*Comment
	cached   []byte // rendered REST JSON, rebuilt on mutation
}

type Pull struct {
	id             int
	number         int
	title          string
	body           string
	headRef        string
	baseRef        string
	headSha        string
	baseSha        string
	merged         bool
	mergeCommitSha string
	mergedAt       string
	closed         bool // closed without merge
}

type Repo struct {
	id          int
	name        string
	owner       string
	private     bool
	description string

	objects map[string]*Obj
	refs    map[string]string // full ref name -> oid
	// refOrder preserves creation order for deterministic listings.
	refOrder []string

	issues     []*Issue
	issuesByNo map[int]*Issue
	pulls      []*Pull
	pullsByNo  map[int]*Pull
	labels     map[string]*Label
	labelOrder []string
	nextNumber int // shared issue+PR number counter

	gen uint64 // bumps on every genuine mutation; drives caches

	packMu    sync.Mutex
	packCache map[string]*packEntry
}

type packEntry struct {
	pack      []byte
	shallow   []string
	unshallow []string
	done      chan struct{}
}

type Store struct {
	mu        sync.RWMutex
	repos     map[string]*Repo // "owner/name" -> repo
	reposList []*Repo          // creation order (deterministic listings)
	repoSeq   int
	globalID  int // numeric id counter for issues/comments/pulls/labels
}

func newStore() *Store {
	return &Store{repos: make(map[string]*Repo), repoSeq: 5000}
}

func (s *Store) nextGlobalID() int {
	s.globalID++
	return s.globalID
}

func (s *Store) createRepo(name string, private bool, description string) *Repo {
	s.repoSeq++
	r := &Repo{
		id:          s.repoSeq,
		name:        name,
		owner:       viewerLogin,
		private:     private,
		description: description,
		objects:     make(map[string]*Obj),
		refs:        make(map[string]string),
		issuesByNo:  make(map[int]*Issue),
		pullsByNo:   make(map[int]*Pull),
		labels:      make(map[string]*Label),
		nextNumber:  1,
		packCache:   make(map[string]*packEntry),
	}
	s.repos[viewerLogin+"/"+name] = r
	s.reposList = append(s.reposList, r)
	return r
}

func (s *Store) getRepo(owner, name string) *Repo {
	return s.repos[owner+"/"+name]
}

// bump records a genuine mutation: invalidates pack caches (they are keyed
// by gen, so old entries simply stop matching) and advances gen.
func (r *Repo) bump() { r.gen++ }

// --- git objects -----------------------------------------------------------

func (r *Repo) putObject(typ string, data []byte) string {
	oid := gitObjectID(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &Obj{typ: typ, data: data}
	}
	return oid
}

func (r *Repo) getObject(oid string) *Obj { return r.objects[oid] }

func (r *Repo) refTip(ref string) (string, bool) {
	oid, ok := r.refs[ref]
	return oid, ok
}

func (r *Repo) defaultTip() (string, bool) {
	return r.refTip("refs/heads/" + r.defaultBranch())
}

func (r *Repo) defaultBranch() string { return "main" }

func (r *Repo) setRef(ref, oid string) {
	if _, ok := r.refs[ref]; !ok {
		r.refOrder = append(r.refOrder, ref)
	}
	r.refs[ref] = oid
}

func (r *Repo) deleteRef(ref string) {
	delete(r.refs, ref)
	for i, n := range r.refOrder {
		if n == ref {
			r.refOrder = append(r.refOrder[:i], r.refOrder[i+1:]...)
			break
		}
	}
}

// branchNames returns sorted branch short names.
func (r *Repo) branchNames() []string {
	var out []string
	for name := range r.refs {
		if strings.HasPrefix(name, "refs/heads/") {
			out = append(out, name[len("refs/heads/"):])
		}
	}
	sort.Strings(out)
	return out
}

// --- commits ---------------------------------------------------------------

type commitInfo struct {
	tree    string
	parents []string
	message string
}

func parseCommit(data []byte) commitInfo {
	var c commitInfo
	s := string(data)
	// headers end at blank line
	rest := s
	for {
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			return c
		}
		line := rest[:nl]
		rest = rest[nl+1:]
		if line == "" {
			c.message = rest
			return c
		}
		if strings.HasPrefix(line, "tree ") {
			c.tree = line[5:]
		} else if strings.HasPrefix(line, "parent ") {
			c.parents = append(c.parents, line[7:])
		}
	}
}

func (r *Repo) makeCommit(treeOID string, parents []string, message string) string {
	var b strings.Builder
	b.WriteString("tree ")
	b.WriteString(treeOID)
	b.WriteString("\n")
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	b.WriteString("author ada iversen <adaiversen@example.invalid> ")
	b.WriteString(strconv.Itoa(fixedEpochSec))
	b.WriteString(" +0000\n")
	b.WriteString("committer ada iversen <adaiversen@example.invalid> ")
	b.WriteString(strconv.Itoa(fixedEpochSec))
	b.WriteString(" +0000\n\n")
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	b.WriteString(message)
	return r.putObject("commit", []byte(b.String()))
}

// listCommits returns oids reachable from tip, newest first (DFS pre-order).
func (r *Repo) listCommits(tip string) []string {
	var out []string
	seen := make(map[string]bool)
	var walk func(oid string)
	walk = func(oid string) {
		if oid == "" || seen[oid] {
			return
		}
		seen[oid] = true
		o := r.objects[oid]
		if o == nil || o.typ != "commit" {
			return
		}
		out = append(out, oid)
		c := parseCommit(o.data)
		for _, p := range c.parents {
			walk(p)
		}
	}
	walk(tip)
	return out
}

// ancestors returns the set of commit oids reachable from tip (incl. tip).
func (r *Repo) ancestors(tip string) map[string]bool {
	set := make(map[string]bool)
	var walk func(oid string)
	walk = func(oid string) {
		if oid == "" || set[oid] {
			return
		}
		o := r.objects[oid]
		if o == nil || o.typ != "commit" {
			return
		}
		set[oid] = true
		for _, p := range parseCommit(o.data).parents {
			walk(p)
		}
	}
	walk(tip)
	return set
}

func (r *Repo) isAncestor(anc, desc string) bool {
	if anc == desc {
		return true
	}
	return r.ancestors(desc)[anc]
}

// mergeBase returns the newest common ancestor of a and b, or "".
func (r *Repo) mergeBase(a, b string) string {
	aSet := r.ancestors(a)
	var found string
	var walk func(oid string)
	walk = func(oid string) {
		if found != "" || oid == "" {
			return
		}
		o := r.objects[oid]
		if o == nil || o.typ != "commit" {
			return
		}
		if aSet[oid] {
			found = oid
			return
		}
		for _, p := range parseCommit(o.data).parents {
			walk(p)
			if found != "" {
				return
			}
		}
	}
	walk(b)
	return found
}

// --- trees -----------------------------------------------------------------

type treeEntry struct {
	mode string
	name string
	oid  string
	dir  bool
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	i := 0
	for i < len(data) {
		sp := indexByte(data, ' ', i)
		if sp < 0 {
			break
		}
		mode := string(data[i:sp])
		nul := indexByte(data, 0, sp+1)
		if nul < 0 || nul+21 > len(data) {
			break
		}
		name := string(data[sp+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, treeEntry{mode: mode, name: name, oid: oid, dir: mode == "40000"})
		i = nul + 21
	}
	return out
}

func indexByte(b []byte, c byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func sortTreeEntries(es []treeEntry) {
	sort.Slice(es, func(i, j int) bool {
		ki := es[i].name
		if es[i].dir {
			ki += "/"
		}
		kj := es[j].name
		if es[j].dir {
			kj += "/"
		}
		return ki < kj
	})
}

func serializeTree(es []treeEntry) []byte {
	sortTreeEntries(es)
	var b []byte
	for _, e := range es {
		b = append(b, e.mode...)
		b = append(b, ' ')
		b = append(b, e.name...)
		b = append(b, 0)
		raw, _ := hex.DecodeString(e.oid)
		b = append(b, raw...)
	}
	return b
}

// flattenTree returns path -> blob oid for every file under treeOID.
func (r *Repo) flattenTree(treeOID string) map[string]string {
	out := make(map[string]string)
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		o := r.objects[treeOID]
		if o == nil || o.typ != "tree" {
			return
		}
		for _, e := range parseTree(o.data) {
			p := prefix + e.name
			if e.dir {
				walk(e.oid, p+"/")
			} else {
				out[p] = e.oid
			}
		}
	}
	walk(treeOID, "")
	return out
}

// treeOIDAt resolves a ref/oid to its root tree oid, or "".
func (r *Repo) treeOIDAt(refOrOID string) string {
	if refOrOID == "" {
		refOrOID = "main"
	}
	oid := refOrOID
	if t, ok := r.refs["refs/heads/"+refOrOID]; ok {
		oid = t
	} else if t, ok := r.refs[refOrOID]; ok {
		oid = t
	}
	o := r.objects[oid]
	if o == nil {
		return ""
	}
	if o.typ == "tree" {
		return oid
	}
	if o.typ != "commit" {
		return ""
	}
	return parseCommit(o.data).tree
}

// setFileInTree returns the new root tree oid after setting path to blobOID
// (or deleting it when blobOID == ""). changed=false when nothing changed.
func (r *Repo) setFileInTree(treeOID string, parts []string, blobOID string) (string, bool) {
	var rec func(treeOID string, depth int) (string, bool)
	rec = func(treeOID string, depth int) (string, bool) {
		var entries []treeEntry
		if treeOID != "" {
			if o := r.objects[treeOID]; o != nil && o.typ == "tree" {
				entries = parseTree(o.data)
			}
		}
		name := parts[depth]
		last := depth == len(parts)-1
		found := -1
		for i, e := range entries {
			if e.name == name {
				found = i
				break
			}
		}
		if last {
			if blobOID == "" {
				if found < 0 || entries[found].dir {
					return treeOID, false
				}
				entries = append(entries[:found], entries[found+1:]...)
				return r.putObject("tree", serializeTree(entries)), true
			}
			if found >= 0 && !entries[found].dir && entries[found].oid == blobOID {
				return treeOID, false
			}
			if found >= 0 {
				entries[found] = treeEntry{mode: "100644", name: name, oid: blobOID}
			} else {
				entries = append(entries, treeEntry{mode: "100644", name: name, oid: blobOID})
			}
			return r.putObject("tree", serializeTree(entries)), true
		}
		// directory level
		var subOID string
		if found >= 0 {
			if !entries[found].dir {
				return treeOID, false
			}
			subOID = entries[found].oid
		}
		newSub, changed := rec(subOID, depth+1)
		if !changed {
			return treeOID, false
		}
		if found >= 0 {
			entries[found].oid = newSub
		} else {
			entries = append(entries, treeEntry{mode: "40000", name: name, oid: newSub, dir: true})
		}
		return r.putObject("tree", serializeTree(entries)), true
	}
	return rec(treeOID, 0)
}

// --- reachability for fetches ----------------------------------------------

// reachableObjects returns every object oid reachable from the given commits
// (commits, their trees recursively, and blobs).
func (r *Repo) reachableObjects(commits map[string]bool) map[string]bool {
	out := make(map[string]bool)
	var walkTree func(treeOID string)
	walkTree = func(treeOID string) {
		if treeOID == "" || out[treeOID] {
			return
		}
		o := r.objects[treeOID]
		if o == nil || o.typ != "tree" {
			return
		}
		out[treeOID] = true
		for _, e := range parseTree(o.data) {
			if e.dir {
				walkTree(e.oid)
			} else {
				out[e.oid] = true
			}
		}
	}
	for oid := range commits {
		if out[oid] {
			continue
		}
		o := r.objects[oid]
		if o == nil || o.typ != "commit" {
			continue
		}
		out[oid] = true
		walkTree(parseCommit(o.data).tree)
	}
	return out
}

// planFetch computes the object set for an upload-pack fetch.
// Returns send object oids, new shallow boundaries, and unshallowed oids.
func (r *Repo) planFetch(wants, haves, shallow []string, depth int) (send map[string]bool, shallowOut, unshallowOut []string) {
	// Depth-limited walk from wants (BFS, min distance).
	// A wanted tag object is sent itself, and its target commit seeds
	// the walk (a clone of a repo with an annotated tag must receive
	// the tag object, not just its target commit).
	dist := make(map[string]int)
	queue := []string{}
	extra := make(map[string]bool)
	for _, w := range wants {
		if _, ok := dist[w]; !ok {
			o := r.objects[w]
			if o == nil {
				continue
			}
			if o.typ == "tag" {
				extra[w] = true
				if tgt := parseTagTarget(o.data); tgt != "" {
					if _, ok := dist[tgt]; !ok {
						if to := r.objects[tgt]; to != nil && to.typ == "commit" {
							dist[tgt] = 0
							queue = append(queue, tgt)
						}
					}
				}
				continue
			}
			if o.typ == "commit" {
				dist[w] = 0
				queue = append(queue, w)
			}
		}
	}
	for qi := 0; qi < len(queue); qi++ {
		c := queue[qi]
		d := dist[c]
		if depth > 0 && d >= depth-1 {
			continue
		}
		o := r.objects[c]
		if o == nil {
			continue
		}
		for _, p := range parseCommit(o.data).parents {
			if _, ok := dist[p]; !ok {
				if po := r.objects[p]; po != nil && po.typ == "commit" {
					dist[p] = d + 1
					queue = append(queue, p)
				}
			}
		}
	}
	// Exclusion set: ancestors of haves, but never descending past a
	// shallow-listed commit (its parents are still wanted on deepen).
	shallowSet := make(map[string]bool)
	for _, s := range shallow {
		shallowSet[s] = true
	}
	excl := make(map[string]bool)
	var walkExcl func(oid string)
	walkExcl = func(oid string) {
		if oid == "" || excl[oid] {
			return
		}
		o := r.objects[oid]
		if o == nil || o.typ != "commit" {
			return
		}
		excl[oid] = true
		if shallowSet[oid] {
			return
		}
		for _, p := range parseCommit(o.data).parents {
			walkExcl(p)
		}
	}
	for _, h := range haves {
		walkExcl(h)
	}
	sendCommits := make(map[string]bool)
	for oid := range dist {
		if !excl[oid] {
			sendCommits[oid] = true
		}
	}
	if depth > 0 {
		for oid := range sendCommits {
			if dist[oid] == depth-1 {
				if o := r.objects[oid]; o != nil && len(parseCommit(o.data).parents) > 0 {
					shallowOut = append(shallowOut, oid)
				}
			}
		}
		for _, s := range shallow {
			if _, ok := dist[s]; ok {
				if o := r.objects[s]; o != nil {
					for _, p := range parseCommit(o.data).parents {
						if _, ok := dist[p]; ok {
							unshallowOut = append(unshallowOut, s)
							break
						}
					}
				}
			}
		}
	}
	sort.Strings(shallowOut)
	sort.Strings(unshallowOut)
	haveObjects := r.reachableObjects(excl)
	sendObjects := r.reachableObjects(sendCommits)
	send = make(map[string]bool)
	for oid := range sendObjects {
		if !haveObjects[oid] {
			send[oid] = true
		}
	}
	for oid := range extra {
		send[oid] = true
	}
	return send, shallowOut, unshallowOut
}

// --- diffs -------------------------------------------------------------------

type fileChange struct {
	path      string
	status    string // added | modified | removed
	additions int
	deletions int
}

// diffTrees compares the flattened trees of base and head commits.
func (r *Repo) diffTrees(baseTree, headTree string) []fileChange {
	base := map[string]string{}
	if baseTree != "" {
		base = r.flattenTree(baseTree)
	}
	head := map[string]string{}
	if headTree != "" {
		head = r.flattenTree(headTree)
	}
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var list []string
	for p := range paths {
		list = append(list, p)
	}
	sort.Strings(list)
	var out []fileChange
	for _, p := range list {
		b, inBase := base[p]
		h, inHead := head[p]
		if inBase && !inHead {
			out = append(out, fileChange{path: p, status: "removed", deletions: lineCount(r, b), additions: 0})
		} else if !inBase && inHead {
			out = append(out, fileChange{path: p, status: "added", additions: lineCount(r, h), deletions: 0})
		} else if b != h {
			a, d := lineDiff(r, b, h)
			out = append(out, fileChange{path: p, status: "modified", additions: a, deletions: d})
		}
	}
	return out
}

func lineCount(r *Repo, blobOID string) int {
	o := r.objects[blobOID]
	if o == nil {
		return 0
	}
	if len(o.data) == 0 {
		return 0
	}
	n := 0
	for _, c := range o.data {
		if c == '\n' {
			n++
		}
	}
	if o.data[len(o.data)-1] != '\n' {
		n++
	}
	return n
}

// lineDiff counts added/removed lines by multiset difference.
func lineDiff(r *Repo, oldOID, newOID string) (add, del int) {
	oldLines := map[string]int{}
	if o := r.objects[oldOID]; o != nil {
		for _, l := range strings.Split(string(o.data), "\n") {
			oldLines[l]++
		}
	}
	if o := r.objects[newOID]; o != nil {
		for _, l := range strings.Split(string(o.data), "\n") {
			if oldLines[l] > 0 {
				oldLines[l]--
			} else {
				add++
			}
		}
	}
	for _, n := range oldLines {
		del += n
	}
	return add, del
}

// liveBaseTip returns the current tip of the PR's base branch. A PR's
// recorded baseSha is the tip at creation time; after another PR merges,
// the branch has moved, and merge decisions must use the live tip.
func (r *Repo) liveBaseTip(pr *Pull) string {
	if tip, ok := r.refs["refs/heads/"+pr.baseRef]; ok {
		return tip
	}
	return pr.baseSha
}

// mergePR performs a squash merge of pr onto its base branch's current tip.
// Returns (mergeCommitSha, conflict).
func (r *Repo) mergePR(pr *Pull) (string, bool) {
	baseTip := r.liveBaseTip(pr)
	if r.mergeConflict(baseTip, pr.headSha) {
		return "", true
	}
	tree := r.squashMergeTree(baseTip, pr.headSha)
	msg := "Merge pull request #" + strconv.Itoa(pr.number) + " from " + r.owner + "/" + pr.headRef + "\n\n" + pr.title
	commitOID := r.makeCommit(tree, []string{baseTip}, msg)
	r.setRef("refs/heads/"+pr.baseRef, commitOID)
	pr.merged = true
	pr.mergeCommitSha = commitOID
	pr.mergedAt = fixedTimeISO
	pr.baseSha = commitOID
	r.bump()
	return commitOID, false
}

// mergeConflict reports whether merging head into base would conflict,
// using merge-base as the common ancestor.
func (r *Repo) mergeConflict(baseTip, headTip string) bool {
	mb := r.mergeBase(baseTip, headTip)
	if mb == "" {
		return false
	}
	mbTree := ""
	if o := r.objects[mb]; o != nil {
		mbTree = parseCommit(o.data).tree
	}
	baseTree, headTree := "", ""
	if o := r.objects[baseTip]; o != nil {
		baseTree = parseCommit(o.data).tree
	}
	if o := r.objects[headTip]; o != nil {
		headTree = parseCommit(o.data).tree
	}
	mbFlat := r.flattenTree(mbTree)
	baseFlat := r.flattenTree(baseTree)
	headFlat := r.flattenTree(headTree)
	paths := make(map[string]bool)
	for p := range mbFlat {
		paths[p] = true
	}
	for p := range headFlat {
		paths[p] = true
	}
	for p := range paths {
		mbV, inMb := mbFlat[p]
		headV, inHead := headFlat[p]
		baseV, inBase := baseFlat[p]
		headChanged := !inMb && inHead || inMb && (!inHead || headV != mbV)
		if !headChanged {
			continue
		}
		baseChanged := !inMb && inBase || inMb && (!inBase || baseV != mbV)
		if !baseChanged {
			continue
		}
		// Both sides changed the path; conflict unless they agree.
		if inHead != inBase || (inHead && headV != baseV) {
			return true
		}
	}
	return false
}

// squashMergeTree applies head's changes (vs merge-base) onto base's tree.
func (r *Repo) squashMergeTree(baseTip, headTip string) string {
	mb := r.mergeBase(baseTip, headTip)
	mbTree := ""
	if mb != "" {
		if o := r.objects[mb]; o != nil {
			mbTree = parseCommit(o.data).tree
		}
	}
	baseTree, headTree := "", ""
	if o := r.objects[baseTip]; o != nil {
		baseTree = parseCommit(o.data).tree
	}
	if o := r.objects[headTip]; o != nil {
		headTree = parseCommit(o.data).tree
	}
	mbFlat := r.flattenTree(mbTree)
	headFlat := r.flattenTree(headTree)
	result := r.flattenTree(baseTree)
	// Apply head-vs-mb changes onto base's tree.
	paths := make(map[string]bool)
	for p := range mbFlat {
		paths[p] = true
	}
	for p := range headFlat {
		paths[p] = true
	}
	for p := range paths {
		mbV, inMb := mbFlat[p]
		headV, inHead := headFlat[p]
		if inMb && inHead && mbV == headV {
			continue
		}
		if !inMb && !inHead {
			continue
		}
		if inHead {
			result[p] = headV
		} else {
			delete(result, p)
		}
	}
	// Rebuild nested trees from the flat map.
	return r.buildTreeFromFlat(result)
}

func (r *Repo) buildTreeFromFlat(flat map[string]string) string {
	// Insert paths one by one into an empty tree.
	root := r.putObject("tree", serializeTree(nil))
	paths := make([]string, 0, len(flat))
	for p := range flat {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		parts := strings.Split(p, "/")
		newRoot, _ := r.setFileInTree(root, parts, flat[p])
		root = newRoot
	}
	return root
}

// --- JSON renderers ----------------------------------------------------------

func userJSON(login string, id int) string {
	return `{"login":` + jsonString(login) + `,"id":` + strconv.Itoa(id) + `,"avatar_url":"https://example.invalid/avatar/` + login + `","type":"User","site_admin":false}`
}

func (r *Repo) repoJSON() []byte {
	var b []byte
	b = append(b, '{')
	b = append(b, `"id":`...)
	b = strconv.AppendInt(b, int64(r.id), 10)
	b = append(b, `,"node_id":`...)
	escapeJSONString(&b, globalID("Repository", strconv.Itoa(r.id)))
	b = append(b, `,"name":`...)
	escapeJSONString(&b, r.name)
	b = append(b, `,"full_name":`...)
	escapeJSONString(&b, r.owner+"/"+r.name)
	b = append(b, `,"owner":`...)
	b = append(b, userJSON(r.owner, viewerID)...)
	b = append(b, `,"private":`...)
	if r.private {
		b = append(b, "true"...)
	} else {
		b = append(b, "false"...)
	}
	b = append(b, `,"description":`...)
	if r.description == "" {
		b = append(b, "null"...)
	} else {
		escapeJSONString(&b, r.description)
	}
	b = append(b, `,"default_branch":"main","created_at":"`+fixedTimeISO+`","updated_at":"`+fixedTimeISO+`","pushed_at":"`+fixedTimeISO+`","homepage":null,"language":null,"forks_count":0,"stargazers_count":0,"watchers_count":0,"open_issues_count":`...)
	open := 0
	for _, is := range r.issues {
		if is.state == "open" {
			open++
		}
	}
	b = strconv.AppendInt(b, int64(open), 10)
	b = append(b, `,"has_issues":true,"has_projects":true,"has_wiki":true,"archived":false,"disabled":false,"visibility":`...)
	if r.private {
		escapeJSONString(&b, "private")
	} else {
		escapeJSONString(&b, "public")
	}
	b = append(b, `,"html_url":"https://example.invalid/`+r.owner+`/`+r.name+`","url":"https://example.invalid/api/v3/repos/`+r.owner+`/`+r.name+`","clone_url":"https://example.invalid/`+r.owner+`/`+r.name+`.git","git_url":"git://example.invalid/`+r.owner+`/`+r.name+`.git","ssh_url":"git@example.invalid:`+r.owner+`/`+r.name+`.git"}`...)
	return b
}

func (r *Repo) renderIssue(is *Issue) {
	var b []byte
	b = append(b, '{')
	b = append(b, `"id":`...)
	b = strconv.AppendInt(b, int64(is.id), 10)
	b = append(b, `,"node_id":`...)
	escapeJSONString(&b, globalID("Issue", strconv.Itoa(r.id)+":"+strconv.Itoa(is.number)))
	b = append(b, `,"number":`...)
	b = strconv.AppendInt(b, int64(is.number), 10)
	b = append(b, `,"title":`...)
	escapeJSONString(&b, is.title)
	b = append(b, `,"body":`...)
	escapeJSONString(&b, is.body)
	b = append(b, `,"state":`...)
	escapeJSONString(&b, is.state)
	b = append(b, `,"labels":[`...)
	for i, name := range is.labels {
		if i > 0 {
			b = append(b, ',')
		}
		lb := r.labels[name]
		id, color := 0, "ededed"
		if lb != nil {
			id, color = lb.id, lb.color
		}
		b = append(b, `{"id":`...)
		b = strconv.AppendInt(b, int64(id), 10)
		b = append(b, `,"node_id":`...)
		escapeJSONString(&b, globalID("Label", strconv.Itoa(id)))
		b = append(b, `,"name":`...)
		escapeJSONString(&b, name)
		b = append(b, `,"color":`...)
		escapeJSONString(&b, color)
		b = append(b, `,"description":"","default":false}`...)
	}
	b = append(b, `],"user":`...)
	b = append(b, userJSON(viewerLogin, viewerID)...)
	b = append(b, `,"assignee":null,"locked":false,"comments":`...)
	b = strconv.AppendInt(b, int64(len(is.comments)), 10)
	b = append(b, `,"created_at":"`+fixedTimeISO+`","updated_at":"`+fixedTimeISO+`","closed_at":`...)
	if is.state == "closed" {
		b = append(b, '"')
		b = append(b, fixedTimeISO...)
		b = append(b, '"')
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"html_url":"https://example.invalid/`+r.owner+`/`+r.name+`/issues/`...)
	b = strconv.AppendInt(b, int64(is.number), 10)
	b = append(b, `","url":"https://example.invalid/api/v3/repos/`+r.owner+`/`+r.name+`/issues/`...)
	b = strconv.AppendInt(b, int64(is.number), 10)
	b = append(b, `","repository_url":"https://example.invalid/api/v3/repos/`+r.owner+`/`+r.name+`","author_association":"OWNER"}`...)
	is.cached = b
}

func (r *Repo) renderPull(pr *Pull) []byte {
	var b []byte
	b = append(b, '{')
	b = append(b, `"id":`...)
	b = strconv.AppendInt(b, int64(pr.id), 10)
	b = append(b, `,"node_id":`...)
	escapeJSONString(&b, globalID("PullRequest", strconv.Itoa(r.id)+":"+strconv.Itoa(pr.number)))
	b = append(b, `,"number":`...)
	b = strconv.AppendInt(b, int64(pr.number), 10)
	b = append(b, `,"state":`...)
	if pr.merged || pr.closed {
		b = append(b, `"closed"`...)
	} else {
		b = append(b, `"open"`...)
	}
	b = append(b, `,"title":`...)
	escapeJSONString(&b, pr.title)
	b = append(b, `,"body":`...)
	escapeJSONString(&b, pr.body)
	b = append(b, `,"user":`...)
	b = append(b, userJSON(viewerLogin, viewerID)...)
	b = append(b, `,"head":{"label":"`+r.owner+`:`+pr.headRef+`","ref":`...)
	escapeJSONString(&b, pr.headRef)
	b = append(b, `,"sha":`...)
	escapeJSONString(&b, pr.headSha)
	b = append(b, `,"user":`...)
	b = append(b, userJSON(viewerLogin, viewerID)...)
	b = append(b, `,"repo":`...)
	b = append(b, r.repoJSON()...)
	b = append(b, `},"base":{"label":"`+r.owner+`:`+pr.baseRef+`","ref":`...)
	escapeJSONString(&b, pr.baseRef)
	b = append(b, `,"sha":`...)
	escapeJSONString(&b, pr.baseSha)
	b = append(b, `,"user":`...)
	b = append(b, userJSON(viewerLogin, viewerID)...)
	b = append(b, `,"repo":`...)
	b = append(b, r.repoJSON()...)
	b = append(b, `},"merged":`...)
	if pr.merged {
		b = append(b, "true"...)
	} else {
		b = append(b, "false"...)
	}
	b = append(b, `,"merged_at":`...)
	if pr.merged {
		b = append(b, '"')
		b = append(b, pr.mergedAt...)
		b = append(b, '"')
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"merge_commit_sha":`...)
	if pr.mergeCommitSha == "" {
		b = append(b, "null"...)
	} else {
		escapeJSONString(&b, pr.mergeCommitSha)
	}
	b = append(b, `,"mergeable":`...)
	if r.mergeConflict(pr.baseSha, pr.headSha) {
		b = append(b, "false"...)
	} else {
		b = append(b, "true"...)
	}
	b = append(b, `,"mergeable_state":`...)
	if r.mergeConflict(pr.baseSha, pr.headSha) {
		b = append(b, `"dirty"`...)
	} else {
		b = append(b, `"clean"`...)
	}
	b = append(b, `,"draft":false,"created_at":"`+fixedTimeISO+`","updated_at":"`+fixedTimeISO+`","closed_at":`...)
	if pr.merged || pr.closed {
		b = append(b, '"')
		b = append(b, fixedTimeISO...)
		b = append(b, '"')
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"html_url":"https://example.invalid/`+r.owner+`/`+r.name+`/pull/`...)
	b = strconv.AppendInt(b, int64(pr.number), 10)
	b = append(b, `","url":"https://example.invalid/api/v3/repos/`+r.owner+`/`+r.name+`/pulls/`...)
	b = strconv.AppendInt(b, int64(pr.number), 10)
	b = append(b, `"}`...)
	return b
}

func (r *Repo) renderComment(c *Comment, issueNo int) []byte {
	var b []byte
	b = append(b, '{')
	b = append(b, `"id":`...)
	b = strconv.AppendInt(b, int64(c.id), 10)
	b = append(b, `,"node_id":`...)
	escapeJSONString(&b, globalID("IssueComment", strconv.Itoa(r.id)+":"+strconv.Itoa(issueNo)+":"+strconv.Itoa(c.id)))
	b = append(b, `,"body":`...)
	escapeJSONString(&b, c.body)
	b = append(b, `,"user":`...)
	b = append(b, userJSON(viewerLogin, viewerID)...)
	b = append(b, `,"created_at":"`+fixedTimeISO+`","updated_at":"`+fixedTimeISO+`","html_url":"https://example.invalid/`+r.owner+`/`+r.name+`/issues/`...)
	b = strconv.AppendInt(b, int64(issueNo), 10)
	b = append(b, `#issuecomment-`...)
	b = strconv.AppendInt(b, int64(c.id), 10)
	b = append(b, `","url":"https://example.invalid/api/v3/repos/`+r.owner+`/`+r.name+`/issues/comments/`...)
	b = strconv.AppendInt(b, int64(c.id), 10)
	b = append(b, `","issue_url":"https://example.invalid/api/v3/repos/`+r.owner+`/`+r.name+`/issues/`...)
	b = strconv.AppendInt(b, int64(issueNo), 10)
	b = append(b, `"}`...)
	return b
}

func (s *Store) sha1Hex(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}
