package main

import (
	"testing"

	"github.com/jomar/hookd/internal/config"
)

func TestNameserverLabels(t *testing.T) {
	srv := config.ServerConfig{
		Domain: "Oast.Example.com.",
		DNS: config.DNSConfig{Nameservers: []config.NameserverConfig{
			{Name: "NS1.oast.example.com."},
			{Name: "a.ns.oast.example.com"},
		}},
	}
	got := nameserverLabels(srv)
	for _, want := range []string{"ns1", "a", "ns"} {
		if !got[want] {
			t.Errorf("label %q not reserved, got %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("expected 3 labels, got %v", got)
	}
}

func TestAvoidLabels(t *testing.T) {
	ids := []string{"ns1", "ns2", "abc"}
	gen := avoidLabels(func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}, map[string]bool{"ns1": true, "ns2": true})
	if id := gen(); id != "abc" {
		t.Errorf("expected the first unreserved ID, got %q", id)
	}
}
