package web

import (
	"strings"

	"github.com/swbilisim/FootOps/internal/sites"
)

// dnsRecord is one Cloudflare DNS record a site needs.
type dnsRecord struct {
	Domain, Zone, Name, Type, Content string
}

// dnsRecords lists the records for a site's domains. A domain's zone comes from the
// certificate that covers it (Origin certificates are made per zone): the zone's own
// name gets an A record, a name under it a CNAME to the zone when the site serves that
// too (as www usually is), otherwise its own A record.
func dnsRecords(domains []string, certs []sites.Cert, ipv4 string) []dnsRecord {
	if ipv4 == "" {
		ipv4 = "bu sunucunun IPv4 adresi"
	}
	served := map[string]bool{}
	for _, d := range domains {
		served[d] = true
	}
	certFor, _ := sites.CertFor(domains, certs)
	out := make([]dnsRecord, 0, len(domains))
	for _, d := range domains {
		zone := zoneOf(d, certFor[d].Names)
		rec := dnsRecord{Domain: d, Zone: zone, Name: "@", Type: "A", Content: ipv4}
		if d != zone {
			rec.Name = strings.TrimSuffix(d, "."+zone)
			if served[zone] {
				rec.Type, rec.Content = "CNAME", zone
			}
		}
		out = append(out, rec)
	}
	return out
}

// secondLevel are labels under which a country registers names (ornek.com.tr,
// belediye.bel.tr), for domains no certificate names.
var secondLevel = map[string]bool{"com": true, "net": true, "org": true, "gov": true, "edu": true, "bel": true,
	"gen": true, "web": true, "biz": true, "info": true, "av": true, "k12": true, "tv": true, "name": true, "co": true}

// zoneOf is the Cloudflare zone of a domain: the shortest name of its certificate that
// contains it, otherwise its registrable part.
func zoneOf(domain string, certNames []string) string {
	best := ""
	for _, n := range certNames {
		n = strings.TrimPrefix(n, "*.")
		if (domain == n || strings.HasSuffix(domain, "."+n)) && (best == "" || len(n) < len(best)) {
			best = n
		}
	}
	if best != "" {
		return best
	}
	labels := strings.Split(domain, ".")
	k := 2
	if n := len(labels); n >= 3 && len(labels[n-1]) == 2 && secondLevel[labels[n-2]] {
		k = 3
	}
	if len(labels) <= k {
		return domain
	}
	return strings.Join(labels[len(labels)-k:], ".")
}
