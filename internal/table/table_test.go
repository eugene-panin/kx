package table

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFit(t *testing.T) {
	cols := []Column{
		{Title: "CLUSTER", Shrink: 8},
		{Title: "SERVER", Shrink: 10, Trim: true},
		{Title: "NS", Drop: 1},
		{Title: "STATE"},
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
	o := New(&buf)
	o.TTY, o.Width = true, 40
	err := o.Table(
		[]Column{{Title: "CLUSTER", Shrink: 8}, {Title: "SERVER", Shrink: 10}, {Title: "NAMESPACE", Drop: 1}, {Title: "STATE"}},
		[]Row{
			{Title: "a-client-with-a-really-long-name-that-goes-on-and-on"},
			{Cells: []Cell{{Text: "kubernetes-admin-kubernetes"}, {Text: "https://k8s.some-long-domain.example.com:6443"}, {Text: "monitoring"}, {Text: "on"}}},
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
