package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// command is a `:` command that switches to a resource view.
type command struct {
	names []string
	help  string
	make  func(arg string) View
}

var commands = []command{
	{[]string{"containers", "container", "co", "ps", "c"}, "all containers, grouped by compose project", func(arg string) View {
		if arg == "" {
			return &containersView{}
		}
		return &containersView{scope: "project=" + arg, match: func(c Container) bool { return c.Project == arg }}
	}},
	{[]string{"compose", "projects", "project", "cp", "p"}, "compose projects", func(string) View { return &projectsView{} }},
	{[]string{"services", "service", "svc"}, "services of a compose project (:svc <project>)", nil},
	{[]string{"images", "image", "img", "i"}, "images", func(string) View { return &imagesView{} }},
	{[]string{"volumes", "volume", "vol", "v"}, "volumes", func(string) View { return &volumesView{} }},
	{[]string{"networks", "network", "net", "n"}, "networks", func(string) View { return &networksView{} }},
	{[]string{"quit", "q", "q!"}, "quit d9s", nil},
}

type viewState struct {
	view       View
	filter     string
	sortCol    int
	sortAsc    bool
	grouped    bool
	selectedID string
}

// App is the d9s terminal application.
type App struct {
	tapp    *tview.Application
	docker  *Docker
	refresh time.Duration

	pages  *tview.Pages
	info   *tview.TextView
	hints  *tview.TextView
	table  *tview.Table
	prompt *tview.InputField
	flash  *tview.TextView
	bottom *tview.Pages

	snapMu sync.Mutex
	snap   *Snapshot

	stack      []*viewState
	lineRows   []*Row
	promptKind rune
	flashGen   int
	refreshNow chan struct{}
	overlays   []string
}

// NewApp builds the UI around a connected Docker gateway.
func NewApp(d *Docker, refresh time.Duration, initial string) *App {
	tview.Styles.PrimitiveBackgroundColor = tcell.ColorDefault
	tview.Styles.ContrastBackgroundColor = tcell.ColorDefault
	tview.Styles.BorderColor = tcell.ColorDodgerBlue
	tview.Styles.TitleColor = tcell.ColorAqua

	a := &App{
		tapp:       tview.NewApplication(),
		docker:     d,
		refresh:    refresh,
		snap:       &Snapshot{},
		refreshNow: make(chan struct{}, 1),
	}
	a.info = tview.NewTextView().SetDynamicColors(true)
	a.hints = tview.NewTextView().SetDynamicColors(true)
	logo := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignRight).SetText(logoText)
	header := tview.NewFlex().
		AddItem(a.info, 52, 0, false).
		AddItem(a.hints, 0, 1, false).
		AddItem(logo, 26, 0, false)

	a.table = tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)
	a.table.SetBorder(true).SetBorderPadding(0, 0, 1, 1)
	a.table.SetSelectedStyle(tcell.StyleDefault.Background(tcell.ColorAqua).Foreground(tcell.ColorBlack))
	a.table.SetSelectionChangedFunc(func(row, _ int) {
		if r := a.rowAt(row); r != nil && len(a.stack) > 0 {
			a.top().selectedID = r.ID
		}
	})

	a.prompt = tview.NewInputField().SetFieldBackgroundColor(tcell.ColorDefault).SetLabelColor(tcell.ColorAqua)
	a.prompt.SetDoneFunc(a.promptDone)
	a.prompt.SetChangedFunc(func(text string) {
		if a.promptKind == '/' {
			a.top().filter = text
			a.render()
		}
	})
	a.prompt.SetAutocompleteFunc(a.autocomplete)
	a.prompt.SetAutocompletedFunc(func(text string, _, source int) bool {
		if source == tview.AutocompletedNavigate {
			return false
		}
		a.prompt.SetText(text)
		if source == tview.AutocompletedEnter {
			a.promptDone(tcell.KeyEnter)
		}
		return true
	})
	a.flash = tview.NewTextView().SetDynamicColors(true)
	a.bottom = tview.NewPages().AddPage("flash", a.flash, true, true).AddPage("prompt", a.prompt, true, false)

	main := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, 7, 0, false).
		AddItem(a.table, 0, 1, true).
		AddItem(a.bottom, 1, 0, false)
	a.pages = tview.NewPages().AddPage("main", main, true, true)
	a.tapp.SetRoot(a.pages, true).SetInputCapture(a.globalKeys)

	if !a.runCommand(initial) {
		a.pushView(&containersView{})
	}
	return a
}

const logoText = `[orange::b]     _  ___
[orange::b]  __| |/ _ \ ___
[orange::b] / _` + "`" + ` | (_) / __|
[orange::b]| (_| |\__, \__ \
[orange::b] \__,_|  /_/|___/
[gray]docker · compose`

// Run starts the refresh loop and blocks until the user quits.
func (a *App) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.refreshLoop(ctx)
	return a.tapp.Run()
}

func (a *App) refreshLoop(ctx context.Context) {
	tick := time.NewTicker(a.refresh)
	defer tick.Stop()
	for {
		fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		s := a.docker.Fetch(fctx)
		cancel()
		a.tapp.QueueUpdateDraw(func() {
			a.snapMu.Lock()
			if s.Err != nil && len(s.Containers) == 0 {
				s.Info = a.snap.Info
			}
			a.snap = s
			a.snapMu.Unlock()
			if s.Err != nil {
				a.flashError(s.Err)
			}
			a.render()
		})
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.refreshNow:
		}
	}
}

func (a *App) triggerRefresh() {
	select {
	case a.refreshNow <- struct{}{}:
	default:
	}
}

func (a *App) snapshot() *Snapshot {
	a.snapMu.Lock()
	defer a.snapMu.Unlock()
	return a.snap
}

func (a *App) top() *viewState { return a.stack[len(a.stack)-1] }

func (a *App) pushView(v View) {
	col, asc := v.DefaultSort()
	a.stack = append(a.stack, &viewState{view: v, sortCol: col, sortAsc: asc, grouped: v.Groupable()})
	a.table.Select(1, 0)
	a.render()
}

// replaceView switches resource kind from the command prompt, resetting the history.
func (a *App) replaceView(v View) {
	a.stack = nil
	a.pushView(v)
}

func (a *App) popView() bool {
	if len(a.stack) <= 1 {
		return false
	}
	a.stack = a.stack[:len(a.stack)-1]
	a.render()
	return true
}

func (a *App) rowAt(line int) *Row {
	if line < 0 || line >= len(a.lineRows) {
		return nil
	}
	return a.lineRows[line]
}

func (a *App) selectedRow() *Row {
	line, _ := a.table.GetSelection()
	return a.rowAt(line)
}

// render redraws header and table from the current snapshot and view state.
func (a *App) render() {
	if len(a.stack) == 0 {
		return
	}
	st := a.top()
	s := a.snapshot()
	a.renderInfo(s)
	a.renderHints(st.view)

	cols := st.view.Columns(st.grouped)
	rows := filterRows(st.view.Rows(s, a.docker, st.grouped), st.filter)
	if st.sortCol >= len(cols) {
		st.sortCol = 0
	}
	sortRows(rows, st.sortCol, st.sortAsc, cols[st.sortCol].Numeric)

	a.table.Clear()
	a.lineRows = []*Row{nil}
	for i, c := range cols {
		title := c.Title
		if i == st.sortCol {
			title += map[bool]string{true: "↑", false: "↓"}[st.sortAsc]
		}
		cell := tview.NewTableCell(title).SetSelectable(false).SetTextColor(tcell.ColorWhite).
			SetAttributes(tcell.AttrBold).SetExpansion(1)
		if c.Numeric {
			cell.SetAlign(tview.AlignRight)
		}
		a.table.SetCell(0, i, cell)
	}

	line := 1
	addRow := func(r *Row) {
		for i, txt := range r.Cells {
			cell := tview.NewTableCell(truncate(txt, 60)).SetTextColor(r.Color).SetExpansion(1)
			if cols[i].Numeric {
				cell.SetAlign(tview.AlignRight)
			}
			a.table.SetCell(line, i, cell)
		}
		a.lineRows = append(a.lineRows, r)
		line++
	}
	if st.grouped {
		for _, g := range groupRows(rows) {
			running, total := 0, len(g.rows)
			for _, r := range g.rows {
				if r.Color == colorRunning {
					running++
				}
			}
			name := g.name
			if name == "" {
				name = "(no compose project)"
			}
			label := fmt.Sprintf("▼ %s  [%d/%d active]", name, running, total)
			a.table.SetCell(line, 0, tview.NewTableCell(label).SetSelectable(false).
				SetTextColor(tcell.ColorOrange).SetAttributes(tcell.AttrBold))
			for i := 1; i < len(cols); i++ {
				a.table.SetCell(line, i, tview.NewTableCell("").SetSelectable(false))
			}
			a.lineRows = append(a.lineRows, nil)
			line++
			for _, r := range g.rows {
				addRow(r)
			}
		}
	} else {
		for _, r := range rows {
			addRow(r)
		}
	}

	title := fmt.Sprintf(" [aqua::b]%s[white]", st.view.Title())
	if sc := st.view.Scope(); sc != "" {
		title += fmt.Sprintf("([fuchsia]%s[white])", tview.Escape(sc))
	}
	title += fmt.Sprintf("[[papayawhip]%d[white]]", len(rows))
	if st.filter != "" {
		title += fmt.Sprintf(" [fuchsia]</%s>[white]", tview.Escape(st.filter))
	}
	if len(a.stack) > 1 {
		title += " [gray]<esc> back"
	}
	a.table.SetTitle(title + " ")

	a.restoreSelection(st)
}

func (a *App) restoreSelection(st *viewState) {
	target := -1
	for i, r := range a.lineRows {
		if r != nil && r.ID == st.selectedID {
			target = i
			break
		}
	}
	if target < 0 {
		for i, r := range a.lineRows {
			if r != nil {
				target = i
				break
			}
		}
	}
	if target > 0 {
		a.table.Select(target, 0)
		st.selectedID = a.lineRows[target].ID
		if target == firstSelectable(a.lineRows) {
			a.table.ScrollToBeginning()
		}
	}
}

func firstSelectable(rows []*Row) int {
	for i, r := range rows {
		if r != nil {
			return i
		}
	}
	return -1
}

type rowGroup struct {
	name string
	rows []*Row
}

func groupRows(rows []*Row) []rowGroup {
	idx := map[string]int{}
	var groups []rowGroup
	for _, r := range rows {
		i, ok := idx[r.Group]
		if !ok {
			i = len(groups)
			idx[r.Group] = i
			groups = append(groups, rowGroup{name: r.Group})
		}
		groups[i].rows = append(groups[i].rows, r)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if (groups[i].name == "") != (groups[j].name == "") {
			return groups[j].name == ""
		}
		return groups[i].name < groups[j].name
	})
	return groups
}

func filterRows(rows []Row, filter string) []*Row {
	invert := strings.HasPrefix(filter, "!")
	needle := strings.ToLower(strings.TrimPrefix(filter, "!"))
	out := make([]*Row, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		if needle != "" {
			hit := strings.Contains(strings.ToLower(strings.Join(r.Cells, " ")+" "+r.Group), needle)
			if hit == invert {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

func sortRows(rows []*Row, col int, asc, numeric bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if numeric {
			x, y := a.Num[col], b.Num[col]
			if x != y {
				return (x < y) == asc
			}
		} else if col < len(a.Cells) && col < len(b.Cells) {
			x, y := strings.ToLower(a.Cells[col]), strings.ToLower(b.Cells[col])
			if x != y {
				return (x < y) == asc
			}
		}
		return a.Cells[0] < b.Cells[0]
	})
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (a *App) renderInfo(s *Snapshot) {
	running := 0
	for _, c := range s.Containers {
		if c.State == "running" {
			running++
		}
	}
	host := a.docker.Host
	if len(host) > 40 {
		host = "…" + host[len(host)-39:]
	}
	ver := "[red]unreachable"
	if s.Info.ServerVersion != "" {
		ver = fmt.Sprintf("[white]%s [gray](API %s)", s.Info.ServerVersion, s.Info.APIVersion)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[orange]Context:    [white::b]%s[-::-]\n", tview.Escape(a.docker.ContextName))
	fmt.Fprintf(&b, "[orange]Host:       [white]%s\n", tview.Escape(host))
	fmt.Fprintf(&b, "[orange]Docker:     %s\n", ver)
	fmt.Fprintf(&b, "[orange]Engine:     [white]%s · %d CPU · %s\n", tview.Escape(truncate(s.Info.OS, 18)), s.Info.NCPU, humanBytes(s.Info.MemTotal))
	fmt.Fprintf(&b, "[orange]Containers: [green]%d[white]/%d  [orange]Projects: [white]%d\n", running, len(s.Containers), len(s.Projects))
	fmt.Fprintf(&b, "[orange]Images: [white]%d  [orange]Volumes: [white]%d  [orange]Nets: [white]%d", len(s.Images), len(s.Volumes), len(s.Networks))
	a.info.SetText(b.String())
}

func (a *App) renderHints(v View) {
	type hint struct{ key, name string }
	var hs []hint
	seen := map[string]bool{}
	for _, act := range v.Actions() {
		if seen[act.Name] && act.Key == tcell.KeyEnter {
			continue
		}
		seen[act.Name] = true
		hs = append(hs, hint{act.Label(), act.Name})
	}
	hs = append(hs, hint{":", "Command"}, hint{"/", "Filter"}, hint{"<,>", "Sort col"}, hint{"=", "Sort dir"})
	if v.Groupable() {
		hs = append(hs, hint{"ctrl-g", "Group"})
	}
	hs = append(hs, hint{"?", "Help"})
	const perCol = 6
	cols := (len(hs) + perCol - 1) / perCol
	var b strings.Builder
	for line := 0; line < perCol; line++ {
		for c := 0; c < cols; c++ {
			i := c*perCol + line
			if i >= len(hs) {
				continue
			}
			key := "<" + hs[i].key + ">"
			fmt.Fprintf(&b, "[dodgerblue]%-9s [gray]%-12s", tview.Escape(key), hs[i].name)
		}
		b.WriteString("\n")
	}
	a.hints.SetText(b.String())
}

// ---- input -----------------------------------------------------------------

func (a *App) globalKeys(ev *tcell.EventKey) *tcell.EventKey {
	if ev.Key() == tcell.KeyCtrlC {
		a.tapp.Stop()
		return nil
	}
	if len(a.overlays) > 0 || a.prompt.HasFocus() {
		return ev
	}
	st := a.top()
	if ev.Key() == tcell.KeyRune {
		switch ev.Rune() {
		case ':', '/':
			a.openPrompt(ev.Rune())
			return nil
		case '?':
			a.showHelp(st.view)
			return nil
		case 'q':
			if !a.popView() {
				a.tapp.Stop()
			}
			return nil
		case '<', '>':
			n := len(st.view.Columns(st.grouped))
			if ev.Rune() == '<' {
				st.sortCol = (st.sortCol + n - 1) % n
			} else {
				st.sortCol = (st.sortCol + 1) % n
			}
			a.render()
			return nil
		case '=':
			st.sortAsc = !st.sortAsc
			a.render()
			return nil
		}
	}
	switch ev.Key() {
	case tcell.KeyEscape:
		if st.filter != "" {
			st.filter = ""
			a.render()
		} else {
			a.popView()
		}
		return nil
	case tcell.KeyCtrlR:
		a.triggerRefresh()
		a.flashInfo("refreshing…")
		return nil
	case tcell.KeyCtrlG:
		if st.view.Groupable() {
			st.grouped = !st.grouped
			a.render()
		}
		return nil
	}
	for _, act := range st.view.Actions() {
		if act.Matches(ev) {
			if r := a.selectedRow(); r != nil {
				act.Run(a, r)
			}
			return nil
		}
	}
	return ev
}

func (a *App) openPrompt(kind rune) {
	a.promptKind = kind
	a.prompt.SetLabel(string(kind))
	if kind == '/' {
		a.prompt.SetText("")
		a.prompt.SetPlaceholder("filter rows (prefix ! to invert)")
	} else {
		a.prompt.SetText("")
		a.prompt.SetPlaceholder("containers | compose | svc <project> | images | volumes | networks | quit")
	}
	a.prompt.SetPlaceholderTextColor(tcell.ColorGray)
	a.bottom.SwitchToPage("prompt")
	a.tapp.SetFocus(a.prompt)
}

func (a *App) closePrompt() {
	a.bottom.SwitchToPage("flash")
	a.tapp.SetFocus(a.table)
}

func (a *App) promptDone(key tcell.Key) {
	text := strings.TrimSpace(a.prompt.GetText())
	switch {
	case a.promptKind == '/' && key == tcell.KeyEscape:
		a.top().filter = ""
		a.render()
	case a.promptKind == ':' && key == tcell.KeyEnter && text != "":
		if !a.runCommand(text) {
			a.flashError(fmt.Errorf("unknown command %q (try :containers, :compose, :images, :volumes, :networks)", text))
		}
	}
	a.closePrompt()
}

// runCommand executes a `:` command, returning false if it is unknown.
func (a *App) runCommand(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	name, arg := strings.ToLower(fields[0]), ""
	if len(fields) > 1 {
		arg = fields[1]
	}
	for _, c := range commands {
		if !contains(c.names, name) {
			continue
		}
		switch c.names[0] {
		case "quit":
			a.tapp.Stop()
		case "services":
			if arg == "" {
				arg = "symbiocommerce"
			}
			p := ComposeProject{Name: arg}
			for _, x := range a.snapshot().Projects {
				if x.Name == arg {
					p = x
				}
			}
			a.stack = nil
			a.openServices(p)
		default:
			a.replaceView(c.make(arg))
		}
		return true
	}
	return false
}

func (a *App) autocomplete(text string) []string {
	if a.promptKind != ':' || text == "" || strings.Contains(text, " ") {
		return nil
	}
	var out []string
	for _, c := range commands {
		for _, n := range c.names {
			if strings.HasPrefix(n, strings.ToLower(text)) && len(n) > 2 {
				out = append(out, n)
			}
		}
	}
	return out
}

// ---- flash -----------------------------------------------------------------

func (a *App) flashMsg(color, msg string) {
	a.flashGen++
	gen := a.flashGen
	a.flash.SetText(fmt.Sprintf("[%s]%s", color, tview.Escape(firstLine(msg))))
	time.AfterFunc(5*time.Second, func() {
		a.tapp.QueueUpdateDraw(func() {
			if a.flashGen == gen {
				a.flash.SetText("")
			}
		})
	})
}

func (a *App) flashInfo(msg string)    { a.flashMsg("aqua", msg) }
func (a *App) flashWarn(msg string)    { a.flashMsg("yellow", "⚠ "+msg) }
func (a *App) flashError(err error)    { a.flashMsg("red", "✗ "+err.Error()) }
func (a *App) flashSuccess(msg string) { a.flashMsg("green", "✓ "+msg) }

// runOp runs a daemon operation in the background and reports the outcome.
func (a *App) runOp(label string, op func(ctx context.Context) error) {
	a.flashInfo(label + " …")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := op(ctx)
		a.tapp.QueueUpdateDraw(func() {
			if err != nil {
				a.flashError(fmt.Errorf("%s: %s", label, strings.ReplaceAll(err.Error(), "\n", " | ")))
			} else {
				a.flashSuccess(label)
			}
		})
		a.triggerRefresh()
	}()
}
