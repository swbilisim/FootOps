package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/staging"
)

// Hidden helper commands. The agent starts them as root inside a limited systemd scope,
// with the files they work on already open as descriptors; the first thing they do is
// drop to the site's user. They are never run by hand.
//
//	data-pack     fd 3: the site's data directory        stdout: tar.gz
//	data-extract  fd 3 (or stdin with --stdin): the archive  fd 4: the new directory
//	              fd 5: the current one (--merge)
//	data-fs       one file manager operation: a datafs.Request on stdin, the Reply on
//	              stdout; fd 3: the data directory, fd 4: the trash (--trash), fd 5:
//	              the upload to place or the socket for an opened file (--extra)
//	data-zip      fd 3: the data directory, a datafs.Request (dir, names) on stdin;
//	              stdout: zip; a failure is reported as "FOOTOPS_ERROR <Reply json>"
//
// They report "FOOTOPS_PROGRESS <json>" lines and a final "FOOTOPS_STATS <json>" on
// stderr (data-fs only for an unpack, and without the final line).
const (
	fdArchive = 3
	fdDest    = 4
	fdCurrent = 5
)

func runData(cmd string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	uid := fs.Int("uid", -1, "site user")
	gid := fs.Int("gid", -1, "site group")
	format := fs.String("format", "", "zip, tar or tgz (data-extract)")
	strip := fs.String("strip", "", "top-level folder to remove (data-extract)")
	merge := fs.Bool("merge", false, "add the current directory's other files (data-extract)")
	stdin := fs.Bool("stdin", false, "read the archive from stdin instead of descriptor 3 (tar, tgz)")
	withTrash := fs.Bool("trash", false, "descriptor 4 is the trash (data-fs)")
	withExtra := fs.Bool("extra", false, "descriptor 5 is passed (data-fs)")
	room := fs.Bool("room", false, "keep the disk's reserve free while writing (data-fs, data-extract)")
	endLine := fs.Bool("end-line", false, "the stream ends with its sender's end line (data-extract --stdin, tar)")
	maxBytes := fs.Int64("max-bytes", 0, "write at most this many bytes (data-extract; 0: the default limit)")
	maxFiles := fs.Int("max-files", 0, "write at most this many files and folders (data-extract; 0: the default limit)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := datafs.DropPrivileges(*uid, *gid); err != nil {
		return err
	}
	report := func(kind string, st datafs.Stats) {
		b, _ := json.Marshal(st)
		fmt.Fprintf(stderr, "FOOTOPS_%s %s\n", kind, b)
	}
	progress := func(st datafs.Stats) { report("PROGRESS", st) }
	openRoot := func(fd int) (*os.Root, error) {
		// The magic link reopens the directory the agent passed, which this user may not
		// be able to reach by its path (the site's folder is root's).
		return os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", fd))
	}

	switch cmd {
	case "data-fs", "data-zip":
		var req datafs.Request
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&req); err != nil {
			return fmt.Errorf("istek okunamadı: %w", err)
		}
		data, err := openRoot(fdArchive)
		if err != nil {
			return err
		}
		defer data.Close()
		if cmd == "data-zip" {
			bw := bufio.NewWriterSize(stdout, 256<<10)
			st, err := datafs.PackZip(data, req.Dir, req.Names, bw, progress)
			if err == nil {
				err = bw.Flush()
			}
			if err != nil {
				// The agent answers with this when nothing reached the browser yet.
				b, _ := json.Marshal(datafs.Reply{Error: err.Error(), Code: datafs.ErrorCode(err)})
				fmt.Fprintf(stderr, "FOOTOPS_ERROR %s\n", b)
				return err
			}
			report("STATS", st)
			return nil
		}
		h := datafs.Handles{Data: data, Options: datafs.Options{Progress: progress}}
		if *withTrash {
			if h.Trash, err = openRoot(fdDest); err != nil {
				return err
			}
			defer h.Trash.Close()
		}
		if *withExtra {
			h.Extra = os.NewFile(fdCurrent, "extra")
		}
		if *room {
			h.Options.Room = func(need int64) error {
				return staging.CheckFree(fmt.Sprintf("/proc/self/fd/%d", fdArchive), need)
			}
		}
		return json.NewEncoder(stdout).Encode(datafs.Serve(req, h))
	case "data-pack":
		src, err := openRoot(fdArchive)
		if err != nil {
			return err
		}
		defer src.Close()
		bw := bufio.NewWriterSize(stdout, 256<<10)
		st, err := datafs.Pack(src, bw, datafs.Options{Progress: progress})
		if err == nil {
			err = bw.Flush()
		}
		if err != nil {
			return err
		}
		report("STATS", st)
		return nil
	case "data-extract":
		dst, err := openRoot(fdDest)
		if err != nil {
			return err
		}
		defer dst.Close()
		var in *os.File
		if *stdin {
			if *format == "zip" {
				return errors.New("zip stdin'den okunamaz")
			}
			in = os.Stdin
		} else {
			in = os.NewFile(fdArchive, "archive")
		}
		opt := datafs.Options{Strip: *strip, Progress: progress, Limits: archive.DefaultLimits}
		// A stream from another server is checked as it arrives: no more than announced,
		// and never into the disk's reserve.
		if *maxBytes > 0 && *maxBytes < opt.Limits.MaxTotal {
			opt.Limits.MaxTotal = *maxBytes
		}
		if *maxFiles > 0 && *maxFiles < opt.Limits.MaxFiles {
			opt.Limits.MaxFiles = *maxFiles
		}
		if *room {
			opt.Room = func(need int64) error {
				return staging.CheckFree(fmt.Sprintf("/proc/self/fd/%d", fdDest), need)
			}
		}
		var st datafs.Stats
		switch *format {
		case "zip":
			info, err := in.Stat()
			if err != nil {
				return err
			}
			st, err = datafs.ExtractZip(in, info.Size(), dst, opt)
			if err != nil {
				return err
			}
		case "tgz":
			if st, err = datafs.ExtractTarGz(bufio.NewReaderSize(in, 256<<10), dst, opt); err != nil {
				return err
			}
		case "tar":
			br := bufio.NewReaderSize(in, 256<<10)
			if st, err = datafs.ExtractTar(br, dst, opt); err != nil {
				return err
			}
			// A tar piped from another server (F4) is read to its end, so the sender
			// finishes writing (its exit status tells whether all of it came) instead of
			// dying on a closed pipe; tar pads its end marker to a 10 KB record, and the
			// sender's end line follows.
			switch {
			case *endLine:
				err = datafs.DrainEnd(br)
			case *stdin:
				err = datafs.DrainTrailer(br)
			}
			if err != nil {
				return err
			}
		default:
			return errors.New("--format zip, tar ya da tgz olmalı")
		}
		if *merge {
			cur, err := openRoot(fdCurrent)
			if err != nil {
				return err
			}
			defer cur.Close()
			added, err := datafs.Fill(cur, dst)
			if err != nil {
				return err
			}
			report("KEPT", added)
		}
		report("STATS", st)
		return nil
	}
	return fmt.Errorf("bilinmeyen komut %s", cmd)
}
