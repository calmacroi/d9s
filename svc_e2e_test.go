//go:build e2e

package main

import (
	"context"
	"testing"
)

// TestComposeServicesReadOnly checks service discovery of every running project without changing anything.
func TestComposeServicesReadOnly(t *testing.T) {
	d, err := NewDocker("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := d.Fetch(context.Background())
	for _, p := range s.Projects {
		svcs, err := d.ComposeServices(context.Background(), p)
		t.Logf("%s available=%v services=%d err=%v", p.Name, p.ComposeConfigAvailable(), len(svcs), err)
	}
}
