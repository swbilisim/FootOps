package sites

import (
	"strings"
	"testing"
)

func TestComposeHealthInterval(t *testing.T) {
	s := Spec{Name: "magaza", Group: "musteri", Domains: []string{"magaza.example.com"}, Image: "ghcr.io/x/magaza:main", Port: 3000, MemoryMB: 256}
	out := Compose(s, map[string]string{"A": "1", "B": "iki"})
	if !strings.Contains(out, "    healthcheck:\n      interval: 2m\n      start_period: 1m\n      start_interval: 5s\n") {
		t.Fatalf("no healthcheck interval:\n%s", out)
	}
	// The environment still reads back.
	env, err := ParseComposeEnv(out)
	if err != nil || len(env) != 2 || env["B"] != "iki" {
		t.Errorf("env = %v, %v", env, err)
	}
}
