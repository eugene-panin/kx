package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFit(t *testing.T) {
	cols := []column{
		{title: "CLUSTER", shrink: 8},
		{title: "SERVER", shrink: 10, trim: true},
		{title: "NS", drop: 1},
		{title: "STATE"},
	}
	nat := []int{12, 30, 10, 5} // 57 + 3 gaps of 2 = 63 columns
	for _, tc := range []struct {
		limit  int
		show   []bool
		widths []int
	}{
		{0, []bool{true, true, true, true}, []int{12, 30, 10, 5}},
		{64, []bool{true, true, true, true}, []int{12, 30, 10, 5}},
		// Slightly too wide (the last terminal column stays spare): trim a little.
		{63, []bool{true, true, true, true}, []int{12, 29, 10, 5}},
		// Trimming by a third is not enough: hide NS, then trim what is left.
		{40, []bool{true, true, false, true}, []int{12, 18, 10, 5}},
		// Below every minimum: shrink as far as allowed and let it wrap.
		{10, []bool{true, true, false, true}, []int{8, 10, 10, 5}},
	} {
		show, widths := fit(cols, nat, tc.limit)
		if !slices.Equal(show, tc.show) || !slices.Equal(widths, tc.widths) {
			t.Errorf("fit(limit=%d) = %v %v, want %v %v", tc.limit, show, widths, tc.show, tc.widths)
		}
	}
}

func TestTableFitsWidth(t *testing.T) {
	var buf bytes.Buffer
	o := newOutput(&buf)
	o.tty, o.width = true, 40
	err := o.table(
		[]column{{title: "CLUSTER", shrink: 8}, {title: "SERVER", shrink: 10}, {title: "NAMESPACE", drop: 1}, {title: "STATE"}},
		[]row{
			{title: "a-client-with-a-really-long-name-that-goes-on-and-on"},
			{cells: []cell{{text: "kubernetes-admin-kubernetes"}, {text: "https://k8s.some-long-domain.example.com:6443"}, {text: "monitoring"}, {text: "on"}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if w := ansi.StringWidth(l); w >= 40 {
			t.Errorf("line is %d wide: %q", w, l)
		}
	}
	if !strings.Contains(buf.String(), "…") || strings.Contains(buf.String(), "NAMESPACE") {
		t.Errorf("expected truncation and a hidden NAMESPACE:\n%s", buf.String())
	}
}
