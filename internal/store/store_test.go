package store

import (
	"testing"
)

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"kubernetes-admin@kubernetes":            "kubernetes-admin-kubernetes",
		"arn:aws:eks:eu-west-1:123:cluster/shop": "shop",
		"gke_proj_europe-west1_main":             "gke_proj_europe-west1_main",
		"@@@":                                    "cluster",
		"trailing/":                              "trailing",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
