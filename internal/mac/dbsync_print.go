package mac

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// The Terminal's view of veritabani … (FootOps.app reads their --json instead).

func printDBList(w io.Writer, v DBList) {
	if v.PairsError != "" {
		fmt.Fprintf(w, "⚠ %s\n", v.PairsError)
	}
	if len(v.Databases) == 0 {
		fmt.Fprintf(w, "Mac'teki MongoDB'de (%s) veritabanı yok.\n", v.URI)
		return
	}
	fmt.Fprintf(w, "Mac'teki MongoDB (%s):\n", v.URI)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, d := range v.Databases {
		site := ""
		if d.Pair != nil {
			site = "→ " + clean(d.Pair.ServerName) + " / " + d.Pair.Site
		}
		size := bytesTR(d.Bytes)
		switch {
		case d.Missing:
			size = "yok"
		case d.Opened:
			site = "yedekten / arşivden yüklenmiş ya da çekmeden önceki kopya"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", d.Name, size, site)
	}
	tw.Flush()
	fmt.Fprintln(w, "Eşleştirmek: footops-mac veritabani eslestir <veritabanı> <sunucu> <site>")
}

// count is a side's documents of a collection ("—": not there).
func count(n int64) string {
	if n < 0 {
		return "—"
	}
	return strconv.FormatInt(n, 10)
}

func newestTR(n *Newest, loc *time.Location) string {
	if n == nil {
		return "—"
	}
	where := n.Field
	if n.Collection != "" {
		where = n.Collection + "." + n.Field
	}
	return stamp(n.Time, loc) + " (" + clean(where) + ")"
}

func printDBDiff(w io.Writer, res DBSyncResult, now time.Time) {
	d := res.Diff
	loc := now.Location()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "KOLEKSİYON\tBU MAC\tSUNUCU\tAYNI\tFARKLI\tYALNIZ BU MAC'TE\tYALNIZ SUNUCUDA\t")
	for _, c := range d.Collections {
		if c.Error != "" {
			fmt.Fprintf(tw, "%s\t%s\t%s\t?\t?\t?\t?\t\n", clean(c.Name), count(c.Local), count(c.Server))
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t\n", clean(c.Name), count(c.Local), count(c.Server), c.Same, c.Different, c.OnlyLocal, c.OnlyServer)
	}
	fmt.Fprintf(tw, "toplam\t%d\t%d\t%d\t%d\t%d\t%d\t\n", d.Local.Documents, d.Server.Documents, d.Same, d.Different, d.OnlyLocal, d.OnlyServer)
	tw.Flush()
	if d.More > 0 {
		fmt.Fprintf(w, "  ⚠ %d koleksiyon daha var: karşılaştırılmadı (en çok %d)\n", d.More, compareCollections)
	}
	for _, s := range d.Skipped {
		fmt.Fprintf(w, "  · %s\n", clean(s))
	}
	for _, c := range d.Collections {
		if c.Error != "" {
			fmt.Fprintf(w, "  ⚠ %s: %s\n", clean(c.Name), c.Error)
		}
		if x := c.Examples; x != nil {
			for _, ex := range []struct {
				what string
				ids  []string
			}{{"farklı", x.Different}, {"yalnız bu Mac'te", x.OnlyLocal}, {"yalnız sunucuda", x.OnlyServer}} {
				if len(ex.ids) > 0 {
					fmt.Fprintf(w, "  %s, %s: %s\n", clean(c.Name), ex.what, clean(strings.Join(ex.ids, ", ")))
				}
			}
		}
	}
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "  ⚠ %s\n", warn)
	}
	fmt.Fprintf(w, "En yeni kayıt: bu Mac %s · sunucu %s\n", newestTR(d.Local.Newest, loc), newestTR(d.Server.Newest, loc))
	switch {
	case d.Identical:
		fmt.Fprintln(w, "✓ Belgeler birebir aynı.")
	case d.Errors == 0:
		fmt.Fprintf(w, "≠ Aynı değil: %d farklı, %d yalnız bu Mac'te, %d yalnız sunucuda.\n", d.Different, d.OnlyLocal, d.OnlyServer)
	default:
		fmt.Fprintf(w, "≠ %d farklı, %d yalnız bu Mac'te, %d yalnız sunucuda; %d koleksiyon tek tek karşılaştırılamadı.\n",
			d.Different, d.OnlyLocal, d.OnlyServer, d.Errors)
	}
	fmt.Fprintf(w, "Sunucu tarafı %s yedeği (%s): daha tazesi için panelde elle yedek al, sonra yeniden karşılaştır.\n",
		stamp(res.Backup.Time, loc), ago(res.Backup.Time, now))
}
