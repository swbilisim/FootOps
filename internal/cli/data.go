package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/swbilisim/FootOps/internal/datafs"
)

// Hidden helper commands. The agent starts them as root inside a limited systemd scope,
// with the files they work on already open as descriptors; the first thing they do is
// drop to the site's user. They are never run by hand.
//
//	data-pack     fd 3: the site's data directory        stdout: tar.gz
//	data-extract  fd 3 (or stdin with --stdin): the archive  fd 4: the new directory
//	              fd 5: the current one (--merge)
//
// Both report "FOOTOPS_PROGRESS <json>" lines and a final "FOOTOPS_STATS <json>" on stderr.
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
		opt := datafs.Options{Strip: *strip, Progress: progress}
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
			if st, err = datafs.ExtractTar(bufio.NewReaderSize(in, 256<<10), dst, opt); err != nil {
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
