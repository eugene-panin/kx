package main

import (
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

const colGap = 2

// output renders tables for a terminal: colored and fitted to its width. For
// pipes it degrades to plain, untruncated text that scripts can rely on.
type output struct {
	w     io.Writer
	tty   bool
	color bool
	width int // 0 means unlimited

	plain, ok, bad, warn, dim, bold, title, header, sel lipgloss.Style
}

func newOutput(w io.Writer) *output {
	o := &output{w: w}
	if isTerminal(w) {
		f := w.(*os.File)
		o.tty = true
		if width, _, err := term.GetSize(f.Fd()); err == nil {
			o.width = width
		}
		o.color = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	}
	r := lipgloss.NewRenderer(w)
	// Basic ANSI colors follow the user's terminal theme.
	o.plain = r.NewStyle()
	o.ok = r.NewStyle().Foreground(lipgloss.Color("2"))
	o.bad = r.NewStyle().Foreground(lipgloss.Color("1"))
	o.warn = r.NewStyle().Foreground(lipgloss.Color("3"))
	o.dim = r.NewStyle().Faint(true)
	o.bold = r.NewStyle().Bold(true)
	o.title = r.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	o.header = r.NewStyle().Faint(true)
	o.sel = r.NewStyle().Reverse(true)
	return o
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

func (o *output) paint(st lipgloss.Style, s string) string {
	if !o.color || s == "" {
		return s
	}
	return st.Render(s)
}

type column struct {
	title  string
	shrink int  // narrowest width when truncating with "…"; 0 never truncates
	trim   bool // secondary value: may lose up to a third before columns are hidden
	drop   int  // order in which columns are hidden when too wide, 1 first; 0 never
}

type cell struct {
	text  string
	style lipgloss.Style
}

// row is either a list of cells or a full-width title (e.g. a client name)
// that does not take part in column sizing.
type row struct {
	cells []cell
	title string
}

func (o *output) table(cols []column, rows []row) error {
	lines := o.render(cols, rows)
	_, err := io.WriteString(o.w, strings.Join(lines, "\n")+"\n")
	return err
}

// render lays rows out to the output width. The first line is the column
// header; the rest map one to one onto rows.
func (o *output) render(cols []column, rows []row) []string {
	nat := make([]int, len(cols))
	for i, c := range cols {
		nat[i] = ansi.StringWidth(c.title)
	}
	for _, r := range rows {
		for i, c := range r.cells {
			nat[i] = max(nat[i], ansi.StringWidth(c.text))
		}
	}
	show, widths := fit(cols, nat, o.width)

	line := func(cells []cell) string {
		var parts []string
		for i, c := range cells {
			if !show[i] {
				continue
			}
			text := ansi.Truncate(c.text, widths[i], "…")
			pad := strings.Repeat(" ", widths[i]-ansi.StringWidth(text))
			parts = append(parts, o.paint(c.style, text)+pad)
		}
		l := strings.TrimRight(strings.Join(parts, strings.Repeat(" ", colGap)), " ")
		if o.width > 0 {
			// Even the minimum widths may not fit a very narrow window.
			l = ansi.Truncate(l, o.width-1, "…")
		}
		return l
	}
	header := make([]cell, len(cols))
	for i, c := range cols {
		header[i] = cell{c.title, o.header}
	}
	lines := []string{line(header)}
	for _, r := range rows {
		if r.cells == nil {
			if o.width > 0 {
				r.title = ansi.Truncate(r.title, o.width-1, "…")
			}
			lines = append(lines, r.title)
			continue
		}
		lines = append(lines, line(r.cells))
	}
	return lines
}

// fit decides which columns to show and how wide, so that a row fits limit:
// first trim secondary values by up to a third, then hide secondary columns
// (in drop order), and only then truncate down to the hard minimums.
func fit(cols []column, nat []int, limit int) (show []bool, widths []int) {
	show = make([]bool, len(cols))
	for i := range show {
		show[i] = true
	}
	widths = append([]int(nil), nat...)
	if limit <= 0 {
		return show, widths
	}
	soft := make([]int, len(cols))
	hard := make([]int, len(cols))
	for i, c := range cols {
		hard[i] = nat[i]
		if c.shrink > 0 {
			hard[i] = min(nat[i], c.shrink)
		}
		soft[i] = nat[i]
		if c.trim {
			soft[i] = max(hard[i], nat[i]*2/3)
		}
	}
	// Keep the last terminal column free: some terminals wrap on it.
	limit--
	for {
		total, slack, n := 0, 0, 0
		for i := range cols {
			if show[i] {
				n++
				total += widths[i]
				slack += widths[i] - soft[i]
			}
		}
		excess := total + colGap*(n-1) - limit
		if excess <= 0 {
			return show, widths
		}
		if slack >= excess {
			shrink(show, widths, soft, excess)
			return show, widths
		}
		next := -1
		for i, c := range cols {
			if show[i] && c.drop > 0 && (next < 0 || c.drop < cols[next].drop) {
				next = i
			}
		}
		if next < 0 {
			shrink(show, widths, hard, excess)
			return show, widths
		}
		show[next] = false
	}
}

// shrink takes excess off the columns with the most room above their minimum.
func shrink(show []bool, widths, mins []int, excess int) {
	for ; excess > 0; excess-- {
		best := -1
		for i := range widths {
			if show[i] && widths[i] > mins[i] && (best < 0 || widths[i]-mins[i] > widths[best]-mins[best]) {
				best = i
			}
		}
		if best < 0 {
			return
		}
		widths[best]--
	}
}

// wrapIndented word-wraps s to the output width with every line indented.
func (o *output) wrapIndented(s, indent string) string {
	if o.width > len(indent)+10 {
		s = ansi.Wrap(s, o.width-1-len(indent), " ")
	}
	return indent + strings.ReplaceAll(s, "\n", "\n"+indent)
}
