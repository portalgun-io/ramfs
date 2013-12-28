/*
Package ramfs implements 9P2000 file server keeping all files in
memory.

A 9P2000 server is an agent that provides one or more hierarchical
file systems -- file trees -- that may be accessed by Plan 9
processes. A server responds to requests by clients to navigate the
hierarchy, and to create, remove, read, and write files.

References:
  [intro]   http://plan9.bell-labs.com/magic/man2html/5/0intro
  [attach]  http://plan9.bell-labs.com/magic/man2html/5/attach
  [clunk]   http://plan9.bell-labs.com/magic/man2html/5/clunk
  [error]   http://plan9.bell-labs.com/magic/man2html/5/error
  [flush]   http://plan9.bell-labs.com/magic/man2html/5/flush
  [open]    http://plan9.bell-labs.com/magic/man2html/5/open
  [read]    http://plan9.bell-labs.com/magic/man2html/5/read
  [remove]  http://plan9.bell-labs.com/magic/man2html/5/remove
  [stat]    http://plan9.bell-labs.com/magic/man2html/5/stat
  [version] http://plan9.bell-labs.com/magic/man2html/5/version
  [walk]    http://plan9.bell-labs.com/magic/man2html/5/walk
*/
package ramfs

import (
	"net"
	"path"
	"strings"
	"sync"

	"code.google.com/p/goplan9/plan9"
)

const maxPath = uint64(1<<64 - 1)
const (
	MSIZE = 128*1024 + plan9.IOHDRSZ // maximum message size
	// maximum size that is guaranteed to be transferred atomically
	IOUNIT    = 128 * 1024
	BLOCKSIZE = 2 * 1024 * 1024 // maximum block size

	OREAD   = plan9.OREAD   // open for read
	OWRITE  = plan9.OWRITE  // open for write
	ORDWR   = plan9.ORDWR   // open for read/write
	OEXEC   = plan9.OEXEC   // read but check execute permission
	OTRUNC  = plan9.OTRUNC  // truncate file first
	ORCLOSE = plan9.ORCLOSE // remove on close
	OEXCL   = plan9.OEXCL   // exclusive use
	OAPPEND = plan9.OAPPEND // append only

	QTDIR    = plan9.QTDIR    // type bit for directories
	QTAPPEND = plan9.QTAPPEND // type bit for append only files
	QTEXCL   = plan9.QTEXCL   // type bit for exclusive use files
	QTAUTH   = plan9.QTAUTH   // type bit for authentication file
	QTTMP    = plan9.QTTMP    // type bit for non-backed-up file
	QTFILE   = plan9.QTFILE   // type bits for plain file

	DMDIR    = plan9.DMDIR    // mode bit for directories
	DMAPPEND = plan9.DMAPPEND // mode bit for append only files
	DMEXCL   = plan9.DMEXCL   // mode bit for exclusive use files
	DMAUTH   = plan9.DMAUTH   // mode bit for authentication file
	DMTMP    = plan9.DMTMP    // mode bit for non-backed-up file
	DMREAD   = plan9.DMREAD   // mode bit for read permission
	DMWRITE  = plan9.DMWRITE  // mode bit for write permission
	DMEXEC   = plan9.DMEXEC   // mode bit for execute permission
)

type LogFunc func(format string, v ...interface{})

type FS struct {
	mu        sync.Mutex
	path      uint64
	pathmap   map[uint64]bool
	fidnew    chan (chan *Fid)
	root      *node
	group     *group
	hostowner string
	chatty    bool // not sync'd
	Log       LogFunc
}

// New starts a 9P2000 file server keeping all files in memory. The
// filesystem is entirely maintained in memory, no external storage is
// used. File data is allocated in 128 * 1024 byte blocks.
//
// The root of the filesystem is owned by the user who invoked ramfs and
// is created with Read, Write and Execute permissions for the owner and
// Read and Execute permissions for everyone else (0755). FS create the
// necessary directories and files in /adm/ctl, /adm/group and
// /<hostowner>.
func New(hostowner string) *FS {
	owner := hostowner
	if owner == "" {
		owner = "adm"
	}
	fs := &FS{
		path:      uint64(4),
		pathmap:   make(map[uint64]bool),
		fidnew:    make(chan (chan *Fid)),
		group:     newGroup(owner),
		hostowner: owner,
	}

	root := newNode(fs, "/", owner, "adm", 0755|plan9.DMDIR, 0, nil)
	adm := newNode(fs, "adm", "adm", "adm", 0770|plan9.DMDIR, 1, nil)
	group := newNode(fs, "group", "adm", "adm", 0660, 2, fs.group)
	ctl := newNode(fs, "ctl", "adm", "adm", 0220, 3, newCtl(fs))

	root.children["adm"] = adm
	adm.children["group"] = group
	adm.children["ctl"] = ctl
	root.parent = root
	adm.parent = root
	group.parent = adm
	ctl.parent = adm

	fs.root = root
	go fs.newFid(fs.fidnew)
	return fs
}

func (fs *FS) Halt() error { return nil }

func (fs *FS) newPath() (uint64, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	for path, _ := range fs.pathmap {
		delete(fs.pathmap, path)
		return path, nil
	}

	path := fs.path
	if fs.path == maxPath {
		return 0, perror("out of paths")
	}
	fs.path++
	return path, nil
}

func (fs *FS) delPath(path uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.pathmap[path] = true
}

func (fs *FS) newFid(fidnew <-chan (chan *Fid)) {
	for ch := range fidnew {
		ch <- &Fid{
			num:  uint32(0),
			uid:  "none",
			node: fs.root,
		}
		close(ch)
	}
}

func (fs *FS) walk(name string) (*node, error) {
	root := fs.root
	path := split(name)
	if len(path) == 0 {
		return fs.root, nil
	}

	base := &node{}
	err := walk(root, path, func(n *node, path []string) error {
		if len(path) == 0 {
			base = n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return base, nil
}

// See http://godoc.org/github.com/mars9/ramfs#Fid
func (fs *FS) Attach(uname, aname string) (*Fid, error) {
	user, err := fs.group.Get(uname)
	if err != nil {
		user, _ = fs.group.Get("none")
	}
	uid := user.Name

	aname = path.Clean(aname)
	node, err := fs.walk(aname)
	if err != nil {
		return nil, err
	}
	return &Fid{uid: uid, node: node}, nil
}

// See http://godoc.org/github.com/mars9/ramfs#Fid.Create
func (fs *FS) Create(name string, mode uint8, perm Perm) (*Fid, error) {
	user, err := fs.group.Get(fs.hostowner)
	if err != nil {
		panic(err) // can't happen
	}
	uid := user.Name

	name = path.Clean(name)
	dname, name := path.Dir(name), path.Base(name)
	dir, err := fs.walk(dname)
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	node, err := dir.Create(uid, name, mode, plan9.Perm(perm))
	if err != nil {
		return nil, err
	}
	return &Fid{uid: uid, node: node}, nil
}

// See http://godoc.org/github.com/mars9/ramfs#Fid.Open
func (fs *FS) Open(name string, mode uint8) (*Fid, error) {
	user, err := fs.group.Get(fs.hostowner)
	if err != nil {
		panic(err) // can't happen
	}
	uid := user.Name

	name = path.Clean(name)
	node, err := fs.walk(name)
	if err != nil {
		return nil, err
	}

	fid := &Fid{uid: uid, node: node}
	if err := fid.Open(mode); err != nil {
		return nil, err
	}
	return fid, nil
}

// See http://godoc.org/github.com/mars9/ramfs#Fid.Remove
func (fs *FS) Remove(name string) error {
	user, err := fs.group.Get(fs.hostowner)
	if err != nil {
		panic(err) // can't happen
	}
	uid := user.Name

	name = path.Clean(name)
	node, err := fs.walk(name)
	if err != nil {
		return err
	}

	fid := &Fid{uid: uid, node: node}
	return fid.Remove()
}

func (fs *FS) Listen(network, addr string) error {
	work := make(chan *transaction)
	srv := &server{
		work:    work,
		fs:      fs,
		conn:    uint32(0),
		connmap: make(map[uint32]bool),
	}

	listener, err := net.Listen(network, addr)
	if err != nil {
		return err
	}

	for {
		rwc, err := listener.Accept()
		if err != nil {
			continue
		}
		connId, err := srv.newConn()
		if err != nil {
			rwc.Close()
			continue
		}

		go srv.Listen()
		go func(rwc net.Conn, id uint32) {
			defer srv.delConn(id)
			conn := &conn{
				rwc:    rwc,
				fidnew: fs.fidnew,
				work:   work,
				uid:    "none",
				fidmap: make(map[uint32]*Fid),
			}
			if fs.Log != nil {
				conn.log = fs.Log
			}
			conn.send(conn.recv())
		}(rwc, connId)
	}
}

func split(path string) []string {
	if len(path) == 0 || path == "/" || path == "." {
		return []string{}
	}

	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	return strings.Split(path, "/")
}

// Copied from http://goplan9.googlecode.com/hg/plan9/dir.go
//   http://godoc.org/code.google.com/p/goplan9/plan9#Perm
type Perm uint32

type permChar struct {
	bit Perm
	c   int
}

var permChars = []permChar{
	permChar{plan9.DMDIR, 'd'},
	permChar{plan9.DMAPPEND, 'a'},
	permChar{plan9.DMAUTH, 'A'},
	permChar{plan9.DMDEVICE, 'D'},
	permChar{plan9.DMSOCKET, 'S'},
	permChar{plan9.DMNAMEDPIPE, 'P'},
	permChar{0, '-'},
	permChar{plan9.DMEXCL, 'l'},
	permChar{plan9.DMSYMLINK, 'L'},
	permChar{0, '-'},
	permChar{0400, 'r'},
	permChar{0, '-'},
	permChar{0200, 'w'},
	permChar{0, '-'},
	permChar{0100, 'x'},
	permChar{0, '-'},
	permChar{0040, 'r'},
	permChar{0, '-'},
	permChar{0020, 'w'},
	permChar{0, '-'},
	permChar{0010, 'x'},
	permChar{0, '-'},
	permChar{0004, 'r'},
	permChar{0, '-'},
	permChar{0002, 'w'},
	permChar{0, '-'},
	permChar{0001, 'x'},
	permChar{0, '-'},
}

func (p Perm) String() string {
	s := ""
	did := false
	for _, pc := range permChars {
		if p&pc.bit != 0 {
			did = true
			s += string(pc.c)
		}
		if pc.bit == 0 {
			if !did {
				s += string(pc.c)
			}
			did = false
		}
	}
	return s
}