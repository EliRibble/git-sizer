package sizes

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var NotYetKnown = errors.New("the size of an object is not yet known")

type SizeScanner struct {
	repo *Repository

	// The (recursive) size of trees whose sizes have been computed so
	// far.
	treeSizes map[Oid]TreeSize

	// The size of blobs whose sizes have been looked up so far.
	blobSizes map[Oid]BlobSize

	// The size of commits whose sizes have been looked up so far.
	commitSizes map[Oid]CommitSize

	// The size of tags whose sizes have been looked up so far.
	tagSizes map[Oid]TagSize

	// Statistics about the overall history size:
	HistorySize HistorySize
}

func NewSizeScanner(repo *Repository) (*SizeScanner, error) {
	scanner := &SizeScanner{
		repo:        repo,
		treeSizes:   make(map[Oid]TreeSize),
		blobSizes:   make(map[Oid]BlobSize),
		commitSizes: make(map[Oid]CommitSize),
		tagSizes:    make(map[Oid]TagSize),
	}
	err := scanner.preload()
	if err != nil {
		return nil, err
	}
	return scanner, nil
}

// Prime the blobs.
func (scanner *SizeScanner) preload() error {
	iter, in, err := scanner.repo.NewObjectIter("--all", "--topo-order")
	if err != nil {
		return err
	}
	in.Close()
	defer iter.Close()

	commitObjectSizes := make(map[Oid]Count32)

	for {
		oid, objectType, objectSize, err := iter.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		switch objectType {
		case "blob":
			blobSize := BlobSize{objectSize}
			scanner.recordBlob(oid, blobSize)
		case "tree":
		case "commit":
			commitObjectSizes[oid] = objectSize
		case "tag":
		default:
			panic(fmt.Sprintf("object %v has unknown type", oid))
		}
	}

	commitIter, in, err := scanner.repo.NewCommitIter("--reverse", "--topo-order", "--all")
	if err != nil {
		return err
	}
	in.Close()
	defer commitIter.Close()

	var toDo ToDoList
	for {
		oid, commit, err := commitIter.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		p := pendingCommit{
			oid: oid,
		}
		objectSize, ok := commitObjectSizes[oid]
		if ok {
			commit.Size = objectSize
			p.commit = &commit
		} else {
			fmt.Fprintf(os.Stderr, "warning: size of commit %s not found in cache", oid)
		}
		commitSize, _, parentCount, err := p.Queue(scanner, &toDo)
		if err != nil {
			if err != NotYetKnown {
				return err
			}
			err = scanner.fill(&toDo)
			if err != nil {
				return err
			}
		} else {
			scanner.recordCommit(oid, commitSize, objectSize, parentCount)
		}
	}

	return nil
}

// Scan all of the references in `repo` that match `filter`.
func ScanRepositoryUsingScanner(repo *Repository, filter ReferenceFilter) (HistorySize, error) {
	scanner, err := NewSizeScanner(repo)
	if err != nil {
		return HistorySize{}, err
	}

	done := make(chan interface{})
	defer close(done)

	iter, err := repo.NewReferenceIter()
	if err != nil {
		return HistorySize{}, err
	}

	for {
		ref, ok, err := iter.Next()
		if err != nil {
			return HistorySize{}, err
		}
		if !ok {
			break
		}
		if !filter(ref) {
			continue
		}
		_, err = scanner.ReferenceSize(ref)
		if err != nil {
			return HistorySize{}, err
		}
	}

	return scanner.HistorySize, nil
}

func (scanner *SizeScanner) TypedObjectSize(
	spec string, oid Oid, objectType ObjectType, objectSize Count32,
) (Size, error) {
	switch objectType {
	case "blob":
		blobSize := BlobSize{objectSize}
		scanner.recordBlob(oid, blobSize)
		return blobSize, nil
	case "tree":
		treeSize, err := scanner.TreeSize(oid)
		return treeSize, err
	case "commit":
		commitSize, err := scanner.CommitSize(oid)
		return commitSize, err
	case "tag":
		tagSize, err := scanner.TagSize(oid)
		return tagSize, err
	default:
		panic(fmt.Sprintf("object %v has unknown type", oid))
	}
}

func (scanner *SizeScanner) ObjectSize(spec string) (Oid, ObjectType, Size, error) {
	oid, objectType, objectSize, err := scanner.repo.ReadHeader(spec)
	if err != nil {
		return Oid{}, "missing", nil, err
	}

	size, err := scanner.TypedObjectSize(spec, oid, objectType, objectSize)
	return oid, objectType, size, err
}

func (scanner *SizeScanner) ReferenceSize(ref Reference) (Size, error) {
	size, err := scanner.TypedObjectSize(ref.Refname, ref.Oid, ref.ObjectType, ref.ObjectSize)
	if err != nil {
		return nil, err
	}
	scanner.recordReference(ref)
	return size, err
}

func (scanner *SizeScanner) OidObjectSize(oid Oid) (ObjectType, Size, error) {
	_, objectType, size, error := scanner.ObjectSize(oid.String())
	return objectType, size, error
}

func (scanner *SizeScanner) BlobSize(oid Oid) (BlobSize, error) {
	size, ok := scanner.blobSizes[oid]
	if !ok {
		_, objectType, objectSize, err := scanner.repo.ReadHeader(oid.String())
		if err != nil {
			return BlobSize{}, err
		}
		if objectType != "blob" {
			return BlobSize{}, fmt.Errorf("object %s is a %s, not a blob", oid, objectType)
		}
		size = BlobSize{objectSize}
		scanner.recordBlob(oid, size)
	}
	return size, nil
}

func (scanner *SizeScanner) pendingTree(oid Oid) (*pendingTree, error) {
	tree, err := scanner.repo.ReadTree(oid)
	if err != nil {
		return nil, err
	}

	var entries []TreeEntry
	iter := tree.Iter()
	for {
		entry, entryOk, err := iter.NextEntry()
		if err != nil {
			return nil, err
		}
		if !entryOk {
			break
		}
		entries = append(entries, entry)
	}

	return &pendingTree{
		oid:              oid,
		objectSize:       NewCount32(uint64(len(tree.data))),
		treeSize:         TreeSize{ExpandedTreeCount: 1},
		remainingEntries: entries,
	}, nil
}

func (scanner *SizeScanner) TreeSize(oid Oid) (TreeSize, error) {
	s, ok := scanner.treeSizes[oid]
	if ok {
		return s, nil
	}

	var toDo ToDoList
	p, err := scanner.pendingTree(oid)
	if err != nil {
		return TreeSize{}, err
	}
	toDo.Push(p)
	err = scanner.fill(&toDo)
	if err != nil {
		return TreeSize{}, err
	}

	// Now the size should be in the cache:
	s, ok = scanner.treeSizes[oid]
	if !ok {
		panic("queueTree() didn't fill tree")
	}
	return s, nil
}

func (scanner *SizeScanner) CommitSize(oid Oid) (CommitSize, error) {
	s, ok := scanner.commitSizes[oid]
	if ok {
		return s, nil
	}

	var toDo ToDoList
	toDo.Push(&pendingCommit{oid: oid})
	err := scanner.fill(&toDo)
	if err != nil {
		return CommitSize{}, err
	}

	// Now the size should be in the cache:
	s, ok = scanner.commitSizes[oid]
	if !ok {
		panic("fill() didn't fill commit")
	}
	return s, nil
}

func (scanner *SizeScanner) TagSize(oid Oid) (TagSize, error) {
	s, ok := scanner.tagSizes[oid]
	if ok {
		return s, nil
	}

	var toDo ToDoList
	toDo.Push(&pendingTag{oid: oid})
	err := scanner.fill(&toDo)
	if err != nil {
		return TagSize{}, err
	}

	// Now the size should be in the cache:
	s, ok = scanner.tagSizes[oid]
	if !ok {
		panic("fill() didn't fill tag")
	}
	return s, nil
}

func (scanner *SizeScanner) recordReference(ref Reference) {
	scanner.HistorySize.recordReference(ref)
}

func (scanner *SizeScanner) recordBlob(oid Oid, blobSize BlobSize) {
	scanner.blobSizes[oid] = blobSize
	scanner.HistorySize.recordBlob(blobSize)
}

func (scanner *SizeScanner) recordTree(oid Oid, treeSize TreeSize, size Count32, treeEntries Count32) {
	scanner.treeSizes[oid] = treeSize
	scanner.HistorySize.recordTree(treeSize, size, treeEntries)
}

func (scanner *SizeScanner) recordCommit(oid Oid, commitSize CommitSize, size Count32, parentCount Count32) {
	scanner.commitSizes[oid] = commitSize
	scanner.HistorySize.recordCommit(commitSize, size, parentCount)
}

func (scanner *SizeScanner) recordTag(oid Oid, tagSize TagSize, size Count32) {
	scanner.tagSizes[oid] = tagSize
	scanner.HistorySize.recordTag(tagSize, size)
}

type pendingTree struct {
	oid              Oid
	objectSize       Count32
	entryCount       Count32
	treeSize         TreeSize
	remainingEntries []TreeEntry
}

// Compute and return the size of the tree in `p` if we already know
// the size of its constituents. If the constituents' sizes are not
// yet known but believed to be computable, add any unknown
// constituents to `scanner.toDo` and return a `NotYetKnown` error. If
// another error occurrs while looking up an object, return that
// error. The tree in `p` is not already in the cache.
func (p *pendingTree) Queue(
	scanner *SizeScanner, toDo *ToDoList,
) (TreeSize, Count32, Count32, error) {
	var subtasks ToDoList

	size := &p.treeSize

	// First accumulate all of the sizes (including maximum depth) for
	// all descendants:
	var dst int = 0
	for src, entry := range p.remainingEntries {
		switch {
		case entry.Filemode&0170000 == 0040000:
			// Tree
			subsize, subok := scanner.treeSizes[entry.Oid]
			if subok {
				size.addDescendent(entry.Name, subsize)
				p.entryCount.Increment(1)
			} else {
				// Schedule this one to be computed:
				p2, err := scanner.pendingTree(entry.Oid)
				if err != nil {
					return TreeSize{}, 0, 0, err
				}
				subtasks.Push(p2)
				if dst < src {
					p.remainingEntries[dst] = p.remainingEntries[src]
				}
				dst++
			}

		case entry.Filemode&0170000 == 0160000:
			// Commit
			size.addSubmodule(entry.Name)
			p.entryCount.Increment(1)

		case entry.Filemode&0170000 == 0120000:
			// Symlink
			size.addLink(entry.Name)
			p.entryCount.Increment(1)

		default:
			// Blob
			blobSize, blobOk := scanner.blobSizes[entry.Oid]
			if !blobOk {
				var err error
				blobSize, err = scanner.BlobSize(entry.Oid)
				if err != nil {
					return TreeSize{}, 0, 0, err
				}
			}
			size.addBlob(entry.Name, blobSize)
			p.entryCount.Increment(1)
		}
	}

	if dst > 0 {
		p.remainingEntries = p.remainingEntries[:dst]
		toDo.Push(p)
		toDo.PushAll(subtasks)
		return TreeSize{}, 0, 0, NotYetKnown
	}

	// Now add one to the depth and to the tree count to account for
	// this tree itself:
	size.MaxPathDepth.Increment(1)
	return *size, p.objectSize, p.entryCount, nil
}

func (p *pendingTree) Run(scanner *SizeScanner, toDo *ToDoList) error {
	// See if the object's size has been computed since it was
	// enqueued. This can happen if it is used in multiple places in
	// the ancestry graph.
	_, ok := scanner.treeSizes[p.oid]
	if ok {
		return nil
	}

	treeSize, size, treeEntries, err := p.Queue(scanner, toDo)
	if err != nil {
		if err != NotYetKnown {
			return err
		}
		return nil
	}
	scanner.recordTree(p.oid, treeSize, size, treeEntries)
	return nil
}

type pendingCommit struct {
	oid    Oid
	commit *Commit
}

// Compute and return the size of the commit in `p` if we already know
// the size of its constituents. If the constituents' sizes are not
// yet known but believed to be computable, add any unknown
// constituents to `scanner.toDo` and return a `NotYetKnown` error. If
// another error occurrs while looking up an object, return that
// error. The commit in `p` is not already in the cache.
func (p *pendingCommit) Queue(
	scanner *SizeScanner, toDo *ToDoList,
) (CommitSize, Count32, Count32, error) {
	var err error
	var subtasks ToDoList
	var commit *Commit

	if p.commit == nil {
		fmt.Fprintf(os.Stderr, "warning: commit not preloaded: %s\n", p.oid)
		commit, err = scanner.repo.ReadCommit(p.oid)
		if err != nil {
			return CommitSize{}, 0, 0, err
		}
		p.commit = commit
	} else {
		commit = p.commit
	}

	ok := true

	size := CommitSize{}

	// First gather information about the tree:
	treeSize, treeOk := scanner.treeSizes[commit.Tree]
	if treeOk {
		if ok {
			size.addTree(treeSize)
		}
	} else {
		ok = false

		p, err := scanner.pendingTree(commit.Tree)
		if err != nil {
			return CommitSize{}, 0, 0, err
		}
		subtasks.Push(p)
	}

	// Normally we know our parents. So if we don't know the tree,
	// don't even bother trying to look up the parents.
	if !ok {
		toDo.Push(p)
		toDo.PushAll(subtasks)
		return CommitSize{}, 0, 0, NotYetKnown
	}

	// Now accumulate all of the sizes for all parents:
	for _, parent := range commit.Parents {
		parentSize, parentOK := scanner.commitSizes[parent]
		if parentOK {
			if ok {
				size.addParent(parentSize)
			}
		} else {
			ok = false
			// Schedule this one to be computed:
			subtasks.Push(&pendingCommit{oid: parent})
		}
	}

	if !ok {
		toDo.Push(p)
		toDo.PushAll(subtasks)
		return CommitSize{}, 0, 0, NotYetKnown
	}

	// Now add one to the ancestor depth to account for this commit
	// itself:
	size.MaxAncestorDepth.Increment(1)
	return size, commit.Size, NewCount32(uint64(len(commit.Parents))), nil
}

func (p *pendingCommit) Run(scanner *SizeScanner, toDo *ToDoList) error {
	// See if the object's size has been computed since it was
	// enqueued. This can happen if it is used in multiple places
	// in the ancestry graph.
	_, ok := scanner.commitSizes[p.oid]
	if ok {
		return nil
	}

	commitSize, size, parentCount, err := p.Queue(scanner, toDo)
	if err != nil {
		if err != NotYetKnown {
			return err
		}
		return nil
	}
	scanner.recordCommit(p.oid, commitSize, size, parentCount)
	return nil
}

type pendingTag struct {
	oid Oid
	tag *Tag
}

// Compute and return the size of the annotated tag in `p` if we
// already know the size of its referent. If the referent's size is
// not yet known but believed to be computable, add it to
// `scanner.toDo` and return a `NotYetKnown` error. If another error
// occurrs while looking up an object, return that error. The tag in
// `p` is not already in the cache.
func (p *pendingTag) Queue(
	scanner *SizeScanner, toDo *ToDoList,
) (TagSize, Count32, error) {
	var err error
	var subtasks ToDoList
	var tag *Tag

	if p.tag == nil {
		tag, err = scanner.repo.ReadTag(p.oid)
		if err != nil {
			return TagSize{}, 0, err
		}
		p.tag = tag
	} else {
		tag = p.tag
	}

	size := TagSize{TagDepth: 1}
	ok := true
	switch tag.ReferentType {
	case "tag":
		referentSize, referentOK := scanner.tagSizes[tag.Referent]
		if referentOK {
			size.TagDepth.Increment(referentSize.TagDepth)
		} else {
			ok = false
			// Schedule this one to be computed:
			subtasks.Push(&pendingTag{oid: tag.Referent})
		}
	case "commit":
		_, referentOK := scanner.commitSizes[tag.Referent]
		if !referentOK {
			ok = false
			// Schedule this one to be computed:
			subtasks.Push(&pendingCommit{oid: tag.Referent})
		}
	case "tree":
		_, referentOK := scanner.treeSizes[tag.Referent]
		if !referentOK {
			ok = false
			// Schedule this one to be computed:
			p, err := scanner.pendingTree(tag.Referent)
			if err != nil {
				return TagSize{}, 0, err
			}
			subtasks.Push(p)
		}
	case "blob":
		_, referentOK := scanner.commitSizes[tag.Referent]
		if !referentOK {
			_, err := scanner.BlobSize(tag.Referent)
			if err != nil {
				return TagSize{}, 0, err
			}
		}
	default:
	}

	if !ok {
		toDo.Push(p)
		toDo.PushAll(subtasks)
		return TagSize{}, 0, NotYetKnown
	}

	// Now add one to the tag depth to account for this tag itself:
	return size, tag.Size, nil
}

func (p *pendingTag) Run(scanner *SizeScanner, toDo *ToDoList) error {
	// See if the object's size has been computed since it was
	// enqueued. This can happen if it is used in multiple places
	// in the ancestry graph.
	_, ok := scanner.tagSizes[p.oid]
	if ok {
		return nil
	}

	tagSize, size, err := p.Queue(scanner, toDo)
	if err != nil {
		if err != NotYetKnown {
			return err
		}
		return nil
	}
	scanner.recordTag(p.oid, tagSize, size)
	return nil
}

// Compute the sizes of any trees listed in `scanner.toDo` or
// `scanner.treesToDo`. This might involve computing the sizes of
// referred-to objects. Do this without recursion to avoid unlimited
// stack growth.
func (scanner *SizeScanner) fill(toDo *ToDoList) error {
	return toDo.Run(scanner, toDo)
}
