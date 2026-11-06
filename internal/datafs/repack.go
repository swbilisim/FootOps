package datafs

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
)

// Repack turns the tar of a folder that restic dump writes into a files member an import
// accepts: folders and regular files with their permission bits and times, nothing else.
// Links, devices and names an import would refuse are left out (Skipped); owners are
// dropped. Hidden files stay: this is a backup. gzip level 1, like Pack.
func Repack(r io.Reader, w io.Writer, opt Options) (Stats, error) {
	if opt.Limits == (archive.Limits{}) {
		opt.Limits = archive.DefaultLimits
	}
	var st Stats
	last := time.Time{}
	zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return st, err
	}
	tw := tar.NewWriter(zw)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return st, fmt.Errorf("yedekteki dosyalar okunamadı: %w", err)
		}
		name, nerr := archive.CleanName(h.Name)
		if nerr != nil {
			st.Skipped++
			continue
		}
		if name == "" {
			continue // the folder itself
		}
		if st.Files+st.Dirs >= opt.Limits.MaxFiles {
			return st, fmt.Errorf("çok fazla dosya (sınır %d)", opt.Limits.MaxFiles)
		}
		mode := h.Mode & 0o777
		switch h.Typeflag {
		case tar.TypeDir:
			if err := tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: mode, ModTime: h.ModTime}); err != nil {
				return st, err
			}
			st.Dirs++
		case tar.TypeReg:
			if h.Size > opt.Limits.MaxFile || st.Bytes+h.Size > opt.Limits.MaxTotal {
				return st, fmt.Errorf("%s: boyut sınırı aşıldı", name)
			}
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: h.Size, Mode: mode, ModTime: h.ModTime}); err != nil {
				return st, err
			}
			if n, err := io.CopyN(tw, tr, h.Size); err != nil {
				return st, fmt.Errorf("%s: eksik geldi (%d/%d bayt): %w", name, n, h.Size, err)
			}
			st.Files++
			st.Bytes += h.Size
		default:
			st.Skipped++ // a link, a device, a FIFO
		}
		if opt.Progress != nil && time.Since(last) > 500*time.Millisecond {
			last = time.Now()
			opt.Progress(st)
		}
	}
	if err := tw.Close(); err != nil {
		return st, err
	}
	if err := zw.Close(); err != nil {
		return st, err
	}
	if opt.Progress != nil {
		opt.Progress(st)
	}
	return st, nil
}

// Gzip compresses r into w at level 1: a database dump on its way into an archive. Bytes
// is what was read.
func Gzip(r io.Reader, w io.Writer, progress func(Stats)) (Stats, error) {
	var st Stats
	zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return st, err
	}
	buf := make([]byte, 256<<10)
	last := time.Time{}
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, err := zw.Write(buf[:n]); err != nil {
				return st, err
			}
			st.Bytes += int64(n)
			if progress != nil && time.Since(last) > 500*time.Millisecond {
				last = time.Now()
				progress(st)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return st, rerr
		}
	}
	if err := zw.Close(); err != nil {
		return st, err
	}
	if progress != nil {
		progress(st)
	}
	return st, nil
}
