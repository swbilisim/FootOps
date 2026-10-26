package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// TestAlerterCost keeps alert evaluation (every 10 s) cheap, including reading the
// certificates from disk. Run with -v to see the numbers.
func TestAlerterCost(t *testing.T) {
	root := t.TempDir()
	st := sites.Store{Root: root}
	for i := 0; i < 5; i++ {
		crt, key := certPair(t, fmt.Sprintf("site%d.example.com", i))
		c, err := sites.ParseCertPair(crt, key, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		k, _ := sites.KeyPEM(key)
		if err := st.InstallCert(c, k); err != nil {
			t.Fatal(err)
		}
	}
	o := healthy()
	for i := 0; i < 16; i++ {
		o.Containers = append(o.Containers, ContainerInfo{Name: fmt.Sprintf("site-%d", i), Project: "p", State: "running", MemUsage: 100 << 20, MemLimit: 512 << 20})
	}
	a := NewAlerter()
	a.Certs = st.Certs
	now := time.Now()
	n := 200
	start := time.Now()
	for i := 0; i < n; i++ {
		now = now.Add(10 * time.Second)
		a.Evaluate(o, now)
	}
	per := time.Since(start) / time.Duration(n)
	allocs := testing.AllocsPerRun(20, func() { now = now.Add(10 * time.Second); a.Evaluate(o, now) })
	t.Logf("Evaluate with 19 containers and 5 certificates: %v, %.0f allocs", per, allocs)
}
