//go:build linux || darwin

package datafs

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// The agent asks the helper (`footops data-fs`) for one file manager operation at a
// time: a Request as JSON on stdin, a Reply as JSON on stdout. The folders and files it
// works on are open descriptors the agent passed (Handles); the helper has already
// become the site's user.

// Request is one file manager operation.
type Request struct {
	Op   string `json:"op"` // see Serve
	Dir  string `json:"dir,omitempty"`
	Path string `json:"path,omitempty"`
	Name string `json:"name,omitempty"`
	// Names are items of Dir (move, trash, zip).
	Names     []string    `json:"names,omitempty"`
	To        string      `json:"to,omitempty"` // a folder (move) or a new name (rename)
	ID        string      `json:"id,omitempty"` // a trash entry
	List      ListOptions `json:"list,omitempty"`
	Overwrite bool        `json:"overwrite,omitempty"`
	// NoCopy: place fails with ErrNeedsCopy rather than copying (see PlaceOptions).
	NoCopy bool `json:"no_copy,omitempty"`
	// TrashArchive moves an archive to the trash once it is opened (unpack).
	TrashArchive bool  `json:"trash_archive,omitempty"`
	Stamp        Stamp `json:"stamp"`
}

// Reply is the helper's answer. Code classifies an error: "notfound", "exists",
// "invalid", "copy" (ErrNeedsCopy), or "" for anything else.
type Reply struct {
	Error    string       `json:"error,omitempty"`
	Code     string       `json:"code,omitempty"`
	Listing  *Listing     `json:"listing,omitempty"`
	Trash    []TrashEntry `json:"trash,omitempty"`
	Entry    *TrashEntry  `json:"entry,omitempty"`
	Restored *Restored    `json:"restored,omitempty"`
	Placed   *Placed      `json:"placed,omitempty"`
	Unpacked *Unpacked    `json:"unpacked,omitempty"`
	File     *FileInfo    `json:"file,omitempty"`
	Dirs     []string     `json:"dirs,omitempty"`
}

// FileInfo describes a file opened for download (the file itself goes over the socket).
type FileInfo struct {
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// Handles are what the agent passed: the data directory, the trash (when the operation
// needs it) and one more file: the upload to place, or the socket an opened file goes
// back over.
type Handles struct {
	Data, Trash *os.Root
	Extra       *os.File
	Options     Options // limits, progress and disk room for an unpack
}

// ErrorCode classifies err for the agent.
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "notfound"
	case errors.Is(err, ErrExists):
		return "exists"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	case errors.Is(err, ErrNeedsCopy):
		return "copy"
	}
	return ""
}

// Serve runs one request; an error is part of the reply.
func Serve(req Request, h Handles) Reply {
	r, err := serve(req, h)
	if err != nil {
		return Reply{Error: err.Error(), Code: ErrorCode(err)}
	}
	return r
}

func serve(req Request, h Handles) (Reply, error) {
	needTrash := func() error {
		if h.Trash == nil {
			return errors.New("çöp kutusu verilmedi")
		}
		return nil
	}
	switch req.Op {
	case "list":
		l, err := List(h.Data, req.Dir, req.List)
		return Reply{Listing: &l}, err
	case "dirs":
		d, err := Dirs(h.Data)
		return Reply{Dirs: d}, err
	case "open":
		if h.Extra == nil {
			return Reply{}, errors.New("dosyanın gönderileceği yer verilmedi")
		}
		f, err := OpenFile(h.Data, req.Path)
		if err != nil {
			return Reply{}, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return Reply{}, err
		}
		if err := SendFile(h.Extra, f); err != nil {
			return Reply{}, err
		}
		return Reply{File: &FileInfo{Name: info.Name(), Size: info.Size(), MTime: info.ModTime().UTC()}}, nil
	case "mkdir":
		return Reply{}, Mkdir(h.Data, req.Dir, req.Name)
	case "rename":
		return Reply{}, Rename(h.Data, req.Dir, req.Name, req.To)
	case "move":
		return Reply{}, Move(h.Data, req.Dir, req.Names, req.To)
	case "trash":
		if err := needTrash(); err != nil {
			return Reply{}, err
		}
		e, err := Trash(h.Data, h.Trash, req.Dir, req.Names, req.Stamp)
		return Reply{Entry: &e}, err
	case "trash-list":
		if err := needTrash(); err != nil {
			return Reply{}, err
		}
		l, err := TrashList(h.Trash)
		return Reply{Trash: l}, err
	case "untrash":
		if err := needTrash(); err != nil {
			return Reply{}, err
		}
		r, err := Untrash(h.Data, h.Trash, req.ID)
		return Reply{Restored: &r}, err
	case "place":
		if err := needTrash(); err != nil {
			return Reply{}, err
		}
		if h.Extra == nil {
			return Reply{}, errors.New("yüklenen dosya verilmedi")
		}
		p, err := Place(h.Data, h.Trash, h.Extra, req.Dir, req.Name,
			PlaceOptions{Overwrite: req.Overwrite, NoCopy: req.NoCopy, Room: h.Options.Room}, req.Stamp)
		return Reply{Placed: &p}, err
	case "unpack":
		if err := needTrash(); err != nil {
			return Reply{}, err
		}
		u, err := Unpack(h.Data, h.Trash, req.Path, req.TrashArchive, h.Options, req.Stamp)
		return Reply{Unpacked: &u}, err
	}
	return Reply{}, fmt.Errorf("%w: bilinmeyen işlem %q", ErrInvalid, req.Op)
}

// SendFile passes an open file over a unix socket (SCM_RIGHTS): the agent serves a
// download from a file the site's user opened.
func SendFile(sock, f *os.File) error {
	return unix.Sendmsg(int(sock.Fd()), []byte{1}, unix.UnixRights(int(f.Fd())), nil, 0)
}

// ReceiveFile takes the file SendFile passed. The sending side must be closed first
// (the helper has exited and the agent's copy of its end is closed), so that nothing
// sent means end of file rather than waiting.
func ReceiveFile(sock *os.File, name string) (*os.File, error) {
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	var oobn int
	var err error
	forkLocked(func() {
		_, oobn, _, _, err = unix.Recvmsg(int(sock.Fd()), buf, oob, recvCloexec)
		if err == nil {
			closeOnExec(oob[:oobn])
		}
	})
	if err != nil {
		return nil, err
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return nil, errors.New("dosya gelmedi")
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		for _, fd := range fds {
			unix.Close(fd)
		}
		return nil, errors.New("dosya gelmedi")
	}
	return os.NewFile(uintptr(fds[0]), name), nil
}

// closeOnExec marks the descriptors of a control message close-on-exec (where the
// system could not do it on arrival).
func closeOnExec(oob []byte) {
	msgs, _ := unix.ParseSocketControlMessage(oob)
	for i := range msgs {
		fds, _ := unix.ParseUnixRights(&msgs[i])
		for _, fd := range fds {
			unix.CloseOnExec(fd)
		}
	}
}

// SocketPair is the agent's end and the helper's end of a socket for SendFile. Both are
// close-on-exec; the helper's end is passed on explicitly (ExtraFiles).
func SocketPair() (mine, theirs *os.File, err error) {
	var fds [2]int
	forkLocked(func() {
		fds, err = unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|socketCloexec, 0)
		if err == nil {
			unix.CloseOnExec(fds[0])
			unix.CloseOnExec(fds[1])
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "agent"), os.NewFile(uintptr(fds[1]), "helper"), nil
}
