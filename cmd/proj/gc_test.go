package main

import "testing"

func TestTitleHost(t *testing.T) {
	cases := map[string]string{
		"proj @host [go]":          "host",
		"foss @host2":              "host2",
		"virtmc @host2 [big,qemu]": "host2",
		"rctest @host":             "host",
		"noatsign":                 "?",
	}
	for title, want := range cases {
		if got := titleHost(title); got != want {
			t.Errorf("titleHost(%q) = %q, want %q", title, got, want)
		}
	}
}
