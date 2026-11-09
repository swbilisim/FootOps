package mac

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// The Terminal's view of arsiv … (FootOps.app reads their --json instead).

var kindNameTR = map[string]string{kindFootOps: "FootOps arşivi", kindMongodump: "mongodump arşivi", kindFiles: "dosya arşivi"}

// listedMax is the most collections listed in the Terminal.
const listedMax = 40

func printArchiveInfo(w io.Writer, i ArchiveInfo, now time.Time) {
	loc := now.Location()
	fmt.Fprintf(w, "%s · %s · %s\n", i.Name, kindNameTR[i.Kind], bytesTR(i.Size))
	if i.Kind == kindFootOps {
		line := "Site: " + orDash(i.Site)
		if !i.Created.IsZero() {
			line += " · " + stamp(i.Created, loc)
		}
		if i.FootOps != "" {
			line += " · FootOps " + i.FootOps
		}
		fmt.Fprintln(w, line)
		if b := i.Backup; b != nil {
			kind := kindTR[b.Kind]
			if kind == "" {
				kind = orDash(b.Kind)
			}
			fmt.Fprintf(w, "Yedekten: %s, %s yedeği (%s)\n", orDash(strings.ToUpper(b.Server)), kind, stamp(b.Time, loc))
		}
	}
	if d := i.Database; d != nil {
		name := d.Name
		if name == "" {
			name = strings.Join(d.Databases, ", ")
		}
		colls, views := 0, 0
		for _, c := range d.Collections {
			if d.Name == "" || c.DB == d.Name {
				if c.View {
					views++
				} else {
					colls++
				}
			}
		}
		docs := "belge sayısı yüklenince belli olur"
		if d.Documents >= 0 {
			docs = fmt.Sprintf("%d belge (manifestte)", d.Documents)
		}
		fmt.Fprintf(w, "Veritabanı: %s — %d koleksiyon", name, colls)
		if views > 0 {
			fmt.Fprintf(w, " + %d görünüm", views)
		}
		fmt.Fprintf(w, ", %s · döküm %s", docs, bytesTR(d.Bytes))
		if d.Compressed > 0 {
			fmt.Fprintf(w, " (sıkıştırılmış %s)", bytesTR(d.Compressed))
		}
		if d.Server != "" {
			fmt.Fprintf(w, " · MongoDB %s", d.Server)
		}
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		shown := 0
		for _, c := range d.Collections {
			if shown++; shown > listedMax {
				break
			}
			count := "?"
			switch {
			case c.View:
				count = "görünüm"
			case c.Documents >= 0:
				count = fmt.Sprint(c.Documents)
			}
			label := c.Name
			if d.Name == "" {
				label = c.DB + "." + c.Name
			}
			fmt.Fprintf(tw, "  %s\t%s\n", label, count)
		}
		tw.Flush()
		if more := len(d.Collections) + d.More - listedMax; more > 0 {
			fmt.Fprintf(w, "  … ve %d koleksiyon daha (--json hepsini verir)\n", more)
		}
	}
	if f := i.Files; f != nil {
		fmt.Fprintf(w, "Dosyalar: %d dosya, %d klasör, %s", f.Files-f.Hidden, f.Dirs, bytesTR(f.Bytes-f.HiddenBytes))
		if i.Kind == kindFootOps {
			fmt.Fprintf(w, " (sıkıştırılmış %s)", bytesTR(f.Compressed))
		}
		fmt.Fprintln(w)
	}
	if i.Env {
		fmt.Fprintf(w, "Ortam değişkenleri: %d (sırlar içerir)\n", i.EnvVars)
	}
	fmt.Fprintf(w, "sha256: %s\n", i.SHA256)
	for _, n := range i.Notes {
		fmt.Fprintf(w, "  ℹ %s\n", n)
	}
	how := "panelin İçe aktar'ı da aynı denetimi yapar"
	if i.Database != nil {
		how += "; veritabanını ayrıca geçici bir veritabanına yükleyip sayar"
	}
	fmt.Fprintf(w, "✓ Bütünlük tamam (%s): %s.\n", secondsTR(time.Duration(i.TookMS)*time.Millisecond), how)
	switch i.Kind {
	case kindFootOps:
		fmt.Fprintln(w, "Açmak: footops-mac arsiv ac <dosya> [--mongo] · yalnız Mac'teki MongoDB'ye: footops-mac arsiv mongo <dosya>")
	case kindMongodump:
		fmt.Fprintln(w, "Mac'teki MongoDB'ye: footops-mac arsiv mongo <dosya>")
	}
}

func printArchiveOpenStart(w io.Writer, res OpenResult, req archiveOpenReq) {
	where := res.Target
	if !req.folder {
		where = "Mac'teki MongoDB"
	}
	fmt.Fprintf(w, "Açılıyor: %s → %s\n", res.Archive, where)
}

func printCreated(w io.Writer, res CreateResult, panels []ServerPanel) {
	a := res.Archive
	fmt.Fprintf(w, "✓ Arşiv hazır: %s (%s, %s)\n", res.Path, bytesTR(a.Size), secondsTR(time.Duration(res.TookMS)*time.Millisecond))
	if d := a.Database; d != nil {
		fmt.Fprintf(w, "  veritabanı %s: %d belge, döküm %s\n", d.Name, d.Documents, bytesTR(d.Bytes))
	}
	if f := a.Files; f != nil {
		fmt.Fprintf(w, "  dosyalar: %d dosya, %d klasör, %s", f.Files, f.Dirs, bytesTR(f.Bytes))
		if res.Skipped > 0 {
			fmt.Fprintf(w, " (%d öğe alınmadı: gizli dosya, bağlantı ya da içe aktarılamayacak ad)", res.Skipped)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  sha256: %s\n", a.SHA256)
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "  ⚠ %s\n", warn)
	}
	if len(panels) == 0 {
		fmt.Fprintln(w, "Panelde: site sayfası › İçe aktar → dosyayı sayfaya sürükle.")
		return
	}
	fmt.Fprintln(w, "Panelde içe aktarmak için (hedef sitenin adı farklıysa adresteki adı değiştir) dosyayı şu sayfaya sürükle:")
	for _, p := range panels {
		fmt.Fprintf(w, "  %s: %s/sites/%s/import\n", p.Code, p.Panel, res.Site)
	}
}

func printLocalDatabases(w io.Writer, v LocalDatabases) {
	if len(v.Databases) == 0 {
		fmt.Fprintf(w, "Mac'teki MongoDB'de (%s) veritabanı yok.\n", v.URI)
		return
	}
	fmt.Fprintf(w, "Mac'teki MongoDB (%s):\n", v.URI)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, d := range v.Databases {
		note := ""
		if d.Opened {
			note = "yedekten / arşivden yüklenmiş"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", d.Name, bytesTR(d.Bytes), note)
	}
	tw.Flush()
	fmt.Fprintln(w, "Arşiv üretmek: footops-mac arsiv olustur --db <veritabanı> [--dosyalar <klasör>] [--site <ad>]")
}
