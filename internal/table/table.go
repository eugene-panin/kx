package table

import (
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

const colGap = 2

// Output renders tables for a terminal: colored and fitted to its width. For
// pipes it degrades to plain, untruncated text that scripts can rely on.
type Output struct {
	W     io.Writer
	TTY   bool
	Color bool
	Width int // 0 means unlimited

	Plain, OK, Bad, Warn, Dim, Bold, Title, Header, Sel lipgloss.Style
}

func New(w io.Writer) *Output {
	o := &Output{W: w}
	if IsTerminal(w) {
		f := w.(*os.File)
		o.TTY = true
		if width, _, err := term.GetSize(f.Fd()); err == nil {
			o.Width = width
		}
		o.Color = os.Getenv("NO_COLOR") == "" && os.Getenv("KX_NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	}
	if os.Getenv("FORCE_COLOR") != "" && os.Getenv("NO_COLOR") == "" && os.Getenv("KX_NO_COLOR") == "" {
		o.Color = true
	}
	// Basic ANSI colors follow the user's terminal theme.
	o.Plain = lipgloss.NewStyle()
	o.OK = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	o.Bad = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	o.Warn = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	o.Dim = lipgloss.NewStyle().Faint(true)
	o.Bold = lipgloss.NewStyle().Bold(true)
	o.Title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	o.Header = lipgloss.NewStyle().Faint(true)
	o.Sel = lipgloss.NewStyle().Reverse(true)
	return o
}

func IsTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

func (o *Output) Paint(st lipgloss.Style, s string) string {
	if !o.Color || s == "" {
		return s
	}
	return st.Render(s)
}

type Column struct {
	Title  string
	Shrink int  // narrowest width when truncating with "…"; 0 never truncates
	Trim   bool // secondary value: may lose up to a third before columns are hidden
	Drop   int  // order in which columns are hidden when too wide, 1 first; 0 never
}

type Cell struct {
	Text  string
	Style lipgloss.Style
}

// Row is either a list of cells or a full-width title (e.g. a client name)
// that does not take part in column sizing.
type Row struct {
	Cells []Cell
	Title string
}

func (o *Output) Table(cols []Column, rows []Row) error {
	lines := o.Render(cols, rows)
	_, err := io.WriteString(o.W, strings.Join(lines, "\n")+"\n")
	return err
}

// Render lays rows out to the output width. The first line is the column
// header; the rest map one to one onto rows.
func (o *Output) Render(cols []Column, rows []Row) []string {
	if !o.TTY {
		// One record per line for grep and awk: an empty cell would shift the
		// fields after it, so it gets a placeholder.
		filled := make([]Row, len(rows))
		for i, r := range rows {
			filled[i] = r
			if r.Cells != nil {
				filled[i].Cells = make([]Cell, len(r.Cells))
				for k, c := range r.Cells {
					if strings.TrimSpace(c.Text) == "" && k > 0 {
						c.Text = "-"
					}
					filled[i].Cells[k] = c
				}
			}
		}
		rows = filled
	}
	nat := make([]int, len(cols))
	for i, c := range cols {
		nat[i] = ansi.StringWidth(c.Title)
	}
	for _, r := range rows {
		for i, c := range r.Cells {
			nat[i] = max(nat[i], ansi.StringWidth(c.Text))
		}
	}
	show, widths := fit(cols, nat, o.Width)

	line := func(cells []Cell) string {
		var parts []string
		for i, c := range cells {
			if !show[i] {
				continue
			}
			text := ansi.Truncate(c.Text, widths[i], "…")
			pad := strings.Repeat(" ", widths[i]-ansi.StringWidth(text))
			parts = append(parts, o.Paint(c.Style, text)+pad)
		}
		l := strings.TrimRight(strings.Join(parts, strings.Repeat(" ", colGap)), " ")
		if o.Width > 0 {
			// Even the minimum widths may not fit a very narrow window.
			l = ansi.Truncate(l, o.Width-1, "…")
		}
		return l
	}
	header := make([]Cell, len(cols))
	for i, c := range cols {
		header[i] = Cell{Text: c.Title, Style: o.Header}
	}
	lines := []string{line(header)}
	for _, r := range rows {
		if r.Cells == nil {
			if o.Width > 0 {
				r.Title = ansi.Truncate(r.Title, o.Width-1, "…")
			}
			lines = append(lines, r.Title)
			continue
		}
		lines = append(lines, line(r.Cells))
	}
	return lines
}

// fit decides which columns to show and how wide, so that a row fits limit:
// first trim secondary values by up to a third, then hide secondary columns
// (in drop order), and only then truncate down to the hard minimums.
func fit(cols []Column, nat []int, limit int) (show []bool, widths []int) {
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
		if c.Shrink > 0 {
			hard[i] = min(nat[i], c.Shrink)
		}
		soft[i] = nat[i]
		if c.Trim {
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
			if show[i] && c.Drop > 0 && (next < 0 || c.Drop < cols[next].Drop) {
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

// WrapIndented word-wraps s to the output width with every line indented.
func (o *Output) WrapIndented(s, indent string) string {
	if o.Width > len(indent)+10 {
		s = ansi.Wrap(s, o.Width-1-len(indent), " ")
	}
	return indent + strings.ReplaceAll(s, "\n", "\n"+indent)
}
