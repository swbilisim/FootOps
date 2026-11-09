package mac

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// The Terminal's view of yedek … (FootOps.app reads their --json instead).

func printBackupServers(w io.Writer, list []BackupServer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SUNUCU\tKOVA\tDEPO ŞİFRESİ")
	for _, s := range list {
		pw := "Anahtar Zinciri'nde"
		if !s.Password {
			pw = "yok (restic-" + s.Code + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Code, s.Bucket, pw)
	}
	tw.Flush()
}

// stamp is a backup's time as the list shows it (and ac takes it back): the Mac's clock,
// to the minute.
func stamp(t time.Time, loc *time.Location) string { return t.In(loc).Format("2006-01-02 15:04") }

func printBackupList(w io.Writer, l BackupList, now time.Time) {
	loc := now.Location()
	fmt.Fprintf(w, "%s · %d anlık görüntü · %s\n\n", l.Bucket, l.Snapshots, secondsTR(time.Duration(l.TookMS)*time.Millisecond))
	if len(l.Sites) == 0 {
		fmt.Fprintln(w, "Bu depoda FootOps'un site yedeği yok.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SİTE\tVERİTABANI\tDOSYALAR\tAYARLAR\tYEDEK")
		for _, s := range l.Sites {
			last := map[string]time.Time{}
			for _, p := range s.Points {
				for part, bp := range p.Parts {
					if bp.Time.After(last[part]) {
						last[part] = bp.Time
					}
				}
			}
			cell := func(part string) string {
				if t, ok := last[part]; ok {
					return stamp(t, loc)
				}
				return "—"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", s.Name, cell(partDB), cell(partFiles), cell(partDef), len(s.Points))
		}
		tw.Flush()
	}
	if !l.ServerPart.IsZero() {
		fmt.Fprintf(w, "\nSunucu ayarları: %s\n", stamp(l.ServerPart, loc))
	}
	if l.Other > 0 {
		fmt.Fprintf(w, "(%d anlık görüntü FootOps'unkilere benzemediği için gösterilmedi)\n", l.Other)
	}
	fmt.Fprintln(w, "\nBir sitenin yedekleri: footops-mac yedek liste <sunucu> <site>")
}

func printSiteBackups(w io.Writer, s SiteBackups, now time.Time) {
	loc := now.Location()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ZAMAN\tTÜR\tKİMLİK\tPARÇALAR")
	shown := 0
	for _, p := range s.Points {
		if shown++; shown > 60 {
			fmt.Fprintf(tw, "… ve %d yedek daha (--json hepsini verir)\t\t\t\n", len(s.Points)-60)
			break
		}
		kind := kindTR[p.Kind]
		if kind == "" {
			kind = orDash(p.Kind)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", stamp(p.Time, loc), kind, shortRun(p.Run), partsText(p.Parts))
	}
	tw.Flush()
	fmt.Fprintf(w, "\nAçmak için: footops-mac yedek ac <sunucu> %s \"<zaman>\"  (ya da kimliği)\n", s.Name)
}

// shortRun is the first letters of a backup's id, enough to name it (ac takes them).
func shortRun(run string) string {
	if len(run) > 8 {
		return run[:8]
	}
	return run
}

func partsText(parts map[string]BackupPart) string {
	var out []string
	if p, ok := parts[partDB]; ok {
		out = append(out, "veritabanı ("+bytesTR(p.Bytes)+")")
	}
	if p, ok := parts[partFiles]; ok {
		out = append(out, fmt.Sprintf("dosyalar (%d dosya, %s)", p.Files, bytesTR(p.Bytes)))
	}
	if _, ok := parts[partDef]; ok {
		out = append(out, "ayarlar")
	}
	return strings.Join(out, ", ")
}

func printOpenStart(w io.Writer, res OpenResult, req openReq, loc *time.Location) {
	where := res.Target
	if !req.folder {
		where = "Mac'teki MongoDB"
	}
	fmt.Fprintf(w, "Açılıyor: %s / %s → %s\n", res.Bucket, res.Site, where)
	var parts []string
	for _, p := range siteParts {
		op, ok := res.Parts[p]
		if !ok {
			continue
		}
		text := partTR[p] + " " + stamp(op.Time, loc)
		if k := kindTR[op.Kind]; k != "" {
			text += " (" + k + ")"
		}
		parts = append(parts, text)
	}
	fmt.Fprintf(w, "  %s\n", strings.Join(parts, " · "))
}

func printOpened(w io.Writer, v OpenedView) {
	now := v.Time
	if len(v.Folders) == 0 {
		fmt.Fprintln(w, "Açılmış yedek klasörü yok.")
	} else {
		fmt.Fprintln(w, "KLASÖRLER (sırlar içerebilir; işin bitince sil: footops-mac yedek sil <klasör>)")
		for _, f := range v.Folders {
			note := ""
			switch {
			case f.Busy:
				note = "  … açılıyor"
			case f.Partial:
				note = "  ⚠ yarım kaldı (açma kesildi)"
			case f.Old:
				note = "  ⚠ bir haftadan eski"
			}
			if f.Error != "" {
				fmt.Fprintf(w, "  %s\n    %s / %s · %s%s\n", f.Path, f.Server, f.Site, f.Error, note)
				continue
			}
			fmt.Fprintf(w, "  %s\n    %s / %s · yedek %s · açıldı %s · %d dosya, %s%s\n", f.Path, f.Server, f.Site,
				stamp(f.Time, now.Location()), ago(f.At, now), f.Files, bytesTR(f.Bytes), note)
		}
	}
	fmt.Fprintln(w)
	switch {
	case v.MongoError != "":
		fmt.Fprintf(w, "MongoDB: %s\n", v.MongoError)
	case len(v.Databases) == 0:
		fmt.Fprintln(w, "Mac'teki MongoDB'de yedekten yüklenmiş veritabanı (*_yedek_*) yok.")
	default:
		fmt.Fprintln(w, "MONGODB (silmek: footops-mac yedek sil <veritabanı>)")
		for _, d := range v.Databases {
			text := fmt.Sprintf("  %s · %s", d.Name, bytesTR(d.Bytes))
			if d.Registered {
				text += fmt.Sprintf(" · %s / %s, yedek %s · yüklendi %s", d.Server, d.Site, stamp(d.Time, now.Location()), ago(d.At, now))
			} else {
				text += " · footops-mac'in listesinde değil"
			}
			if d.Old {
				text += "  ⚠ bir haftadan eski"
			}
			fmt.Fprintln(w, text)
		}
	}
}
