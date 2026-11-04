package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"sigs.k8s.io/yaml"
)

// pushOverlay shows a full-screen or modal page on top of the main view.
func (a *App) pushOverlay(name string, p tview.Primitive, fullscreen bool) {
	a.overlays = append(a.overlays, name)
	a.pages.AddPage(name, p, fullscreen, true)
	a.tapp.SetFocus(p)
}

func (a *App) popOverlay() {
	if len(a.overlays) == 0 {
		return
	}
	name := a.overlays[len(a.overlays)-1]
	a.overlays = a.overlays[:len(a.overlays)-1]
	a.pages.RemovePage(name)
	if len(a.overlays) == 0 {
		a.tapp.SetFocus(a.table)
	}
}

func centered(p tview.Primitive, width, height int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(p, height, 0, true).
			AddItem(nil, 0, 1, false), width, 0, true).
		AddItem(nil, 0, 1, false)
}

// ---- confirm ---------------------------------------------------------------

func (a *App) confirm(msg, verb string, onYes func()) {
	m := tview.NewModal().SetText(msg).AddButtons([]string{"Cancel", verb}).
		SetBackgroundColor(tcell.ColorDarkRed).
		SetButtonBackgroundColor(tcell.ColorBlack)
	m.SetDoneFunc(func(_ int, label string) {
		a.popOverlay()
		if label == verb {
			onYes()
		}
	})
	m.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'n') {
			a.popOverlay()
			return nil
		}
		return ev
	})
	a.pushOverlay("confirm", m, false)
}

// ---- ports -----------------------------------------------------------------

func (a *App) showPorts(c Container) {
	var lines []string
	var urls []string
	seen := map[string]bool{}
	for _, p := range c.Ports {
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		if p.Public == 0 {
			lines = append(lines, fmt.Sprintf("  %d/%s  (exposed, not published)", p.Private, proto))
			continue
		}
		key := fmt.Sprintf("%d-%d-%s", p.Public, p.Private, proto)
		if seen[key] {
			continue
		}
		seen[key] = true
		url := fmt.Sprintf("http://localhost:%d", p.Public)
		lines = append(lines, fmt.Sprintf("  localhost:%-6d → %d/%s   %s", p.Public, p.Private, proto, url))
		if proto == "tcp" {
			urls = append(urls, url)
		}
	}
	text := fmt.Sprintf("Port mappings of %s\n\n", c.Name)
	if len(lines) == 0 {
		text += "  no ports exposed\n"
	} else {
		text += strings.Join(lines, "\n") + "\n"
	}
	if len(c.Networks) > 0 {
		text += "\nNetworks: " + strings.Join(c.Networks, ", ") +
			"\nOther containers reach it as " + orDash(c.Service) + " / " + c.Name + " on these networks."
	}
	m := tview.NewModal().SetText(text)
	buttons := []string{"Close"}
	for i, u := range urls {
		if i == 4 {
			break
		}
		buttons = append(buttons, "Open "+strings.TrimPrefix(u, "http://localhost"))
	}
	m.AddButtons(buttons)
	m.SetDoneFunc(func(_ int, label string) {
		a.popOverlay()
		if port, ok := strings.CutPrefix(label, "Open :"); ok {
			url := "http://localhost:" + port
			if err := exec.Command("open", url).Start(); err != nil {
				a.flashError(err)
			} else {
				a.flashSuccess("opened " + url)
			}
		}
	})
	a.pushOverlay("ports", m, false)
}

// ---- shell -----------------------------------------------------------------

const shellProbe = `if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi`

func (a *App) shell(c Container) {
	if c.State != "running" {
		a.flashError(fmt.Errorf("container %s is %s; start it first (S)", c.Name, c.State))
		return
	}
	var runErr error
	a.tapp.Suspend(func() {
		fmt.Printf("\033[2J\033[H\033[1;33m» shell in %s (exit to return to d9s)\033[0m\n", c.Name)
		cmd := exec.Command("docker", a.docker.CLIArgs("exec", "-it", c.ID, "sh", "-c", shellProbe)...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		runErr = cmd.Run()
		if runErr != nil {
			var exit *exec.ExitError
			if ok := asExitError(runErr, &exit); ok && exit.ExitCode() != 126 && exit.ExitCode() != 127 {
				runErr = nil
			}
		}
	})
	if runErr != nil {
		a.flashError(fmt.Errorf("shell %s: %w", c.Name, runErr))
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// ---- describe --------------------------------------------------------------

var (
	yamlKey = regexp.MustCompile(`^(\s*-?\s*)([^:\s][^:]*?):(\s|$)`)
	jsonKey = regexp.MustCompile(`^(\s*)("[^"]+")(:)`)
)

func (a *App) showDescribe(kind Kind, id, name string) {
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
	tv.SetBorder(true).SetBorderPadding(0, 0, 1, 1)
	asYAML := true
	var raw []byte
	draw := func() {
		format := "yaml"
		if !asYAML {
			format = "json"
		}
		tv.SetTitle(fmt.Sprintf(" [aqua::b]Describe[white] %s %s [gray](%s · y toggle · esc back) ", kind, tview.Escape(name), format))
		if raw == nil {
			return
		}
		tv.SetText(colorizeDoc(raw, asYAML))
		tv.ScrollToBeginning()
	}
	draw()
	tv.SetText("[gray]loading…")
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == 'q'):
			a.popOverlay()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'y':
			asYAML = !asYAML
			draw()
			return nil
		}
		return ev
	})
	a.pushOverlay("describe", tv, true)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		data, err := a.docker.InspectJSON(ctx, kind, id)
		a.tapp.QueueUpdateDraw(func() {
			if err != nil {
				tv.SetText("[red]" + tview.Escape(err.Error()))
				return
			}
			raw = data
			draw()
		})
	}()
}

func colorizeDoc(jsonDoc []byte, asYAML bool) string {
	text := string(jsonDoc)
	re := jsonKey
	if asYAML {
		if y, err := yaml.JSONToYAML(jsonDoc); err == nil {
			text = string(y)
		}
		re = yamlKey
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		l = tview.Escape(l)
		if m := re.FindStringSubmatchIndex(l); m != nil {
			l = l[:m[2]] + l[m[2]:m[3]] + "[dodgerblue]" + l[m[4]:m[5]] + "[white]" + l[m[5]:]
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// ---- help ------------------------------------------------------------------

func (a *App) showHelp(v View) {
	var b strings.Builder
	section := func(title string) { fmt.Fprintf(&b, "\n[orange::b]%s[-::-]\n", title) }
	key := func(k, desc string) { fmt.Fprintf(&b, "  [dodgerblue]%-12s[white]%s\n", tview.Escape(k), desc) }

	section(strings.ToUpper(v.Title()) + " ACTIONS")
	for _, act := range v.Actions() {
		key(act.Label(), act.Name)
	}
	section("NAVIGATION")
	key("j/k ↑/↓", "move selection")
	key("g/G", "top / bottom")
	key("ctrl-f/b", "page down / up")
	key("esc", "clear filter, then go back")
	key("q", "back, quit at top level")
	key("ctrl-c", "quit")
	section("VIEW")
	key("/", "filter rows (live, prefix ! to invert)")
	key("< / >", "previous / next sort column")
	key("=", "reverse sort direction")
	key("ctrl-g", "toggle grouping by compose project")
	key("ctrl-r", "refresh now")
	section("COMMANDS (:)")
	for _, c := range commands {
		short := c.names[len(c.names)-1]
		if c.names[0] == "quit" {
			short = "q"
		}
		fmt.Fprintf(&b, "  [dodgerblue]%-20s[white]%s\n", ":"+c.names[0]+" :"+short, c.help)
	}
	section("LOGS VIEW")
	key("f", "toggle follow (auto-scroll to new lines)")
	key("t", "toggle timestamps")
	key("w", "toggle line wrap")
	key("/", "filter lines")
	key("c", "clear screen")
	key("0", "load full history (instead of last 500 lines)")
	section("DESCRIBE VIEW")
	key("y", "toggle YAML / JSON")

	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetText(b.String())
	tv.SetBorder(true).SetTitle(" [aqua::b]Help[white] (esc to close) ")
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == '?')) {
			a.popOverlay()
			return nil
		}
		return ev
	})
	a.pushOverlay("help", centered(tv, 84, 46), true)
}

// ---- logs ------------------------------------------------------------------

type logSource func(ctx context.Context, follow, timestamps bool, tail string) (io.ReadCloser, error)

const maxLogLines = 20000

type logView struct {
	app    *App
	title  string
	source logSource
	tv     *tview.TextView
	filter *tview.InputField
	layout *tview.Flex

	mu         sync.Mutex
	lines      []string
	dirty      bool
	follow     bool
	timestamps bool
	wrap       bool
	allLines   bool
	needle     string
	cancel     context.CancelFunc
	status     string
}

func (a *App) showLogs(title string, src logSource) {
	lv := &logView{app: a, title: title, source: src, follow: true, wrap: true}
	lv.tv = tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true).SetRegions(false)
	lv.tv.SetBorder(true).SetBorderPadding(0, 0, 1, 1)
	lv.tv.SetChangedFunc(func() {})
	lv.filter = tview.NewInputField().SetLabel("/").SetFieldBackgroundColor(tcell.ColorDefault).SetLabelColor(tcell.ColorAqua)
	lv.filter.SetChangedFunc(func(text string) {
		lv.mu.Lock()
		lv.needle = text
		lv.dirty = true
		lv.mu.Unlock()
		lv.redraw()
	})
	lv.filter.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			lv.filter.SetText("")
		}
		a.tapp.SetFocus(lv.tv)
	})
	lv.layout = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(lv.tv, 0, 1, true).AddItem(lv.filter, 1, 0, false)
	lv.tv.SetInputCapture(lv.keys)
	a.pushOverlay("logs", lv.layout, true)
	a.tapp.SetFocus(lv.tv)
	lv.start()
	go lv.ticker()
}

func (lv *logView) keys(ev *tcell.EventKey) *tcell.EventKey {
	if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == 'q') {
		if lv.needle != "" && ev.Key() == tcell.KeyEscape {
			lv.filter.SetText("")
			return nil
		}
		lv.stop()
		lv.app.popOverlay()
		return nil
	}
	if ev.Key() != tcell.KeyRune {
		if ev.Key() == tcell.KeyUp || ev.Key() == tcell.KeyPgUp || ev.Key() == tcell.KeyCtrlB {
			lv.setFollow(false)
		}
		return ev
	}
	switch ev.Rune() {
	case 'f':
		lv.setFollow(!lv.follow)
	case 't':
		lv.timestamps = !lv.timestamps
		lv.start()
	case 'w':
		lv.wrap = !lv.wrap
		lv.tv.SetWrap(lv.wrap)
		lv.redraw()
	case 'c':
		lv.mu.Lock()
		lv.lines = nil
		lv.dirty = true
		lv.mu.Unlock()
		lv.redraw()
	case '0':
		lv.allLines = !lv.allLines
		lv.start()
	case '/':
		lv.app.tapp.SetFocus(lv.filter)
	case 'k', 'g':
		lv.setFollow(false)
		return ev
	case 'G':
		lv.setFollow(true)
	default:
		return ev
	}
	return nil
}

func (lv *logView) setFollow(on bool) {
	lv.follow = on
	lv.redraw()
}

// start (re)opens the log stream, replacing the buffered lines.
func (lv *logView) start() {
	lv.stop()
	ctx, cancel := context.WithCancel(context.Background())
	lv.mu.Lock()
	lv.cancel = cancel
	lv.lines = nil
	lv.status = "connecting"
	lv.dirty = true
	lv.mu.Unlock()
	tail := "500"
	if lv.allLines {
		tail = "all"
	}
	go func() {
		rc, err := lv.source(ctx, true, lv.timestamps, tail)
		if err != nil {
			lv.setStatus("error: " + err.Error())
			return
		}
		defer rc.Close()
		go func() { <-ctx.Done(); rc.Close() }()
		lv.setStatus("streaming")
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			lv.mu.Lock()
			lv.lines = append(lv.lines, ansiSeq.ReplaceAllString(strings.TrimRight(sc.Text(), "\r"), ""))
			if len(lv.lines) > maxLogLines {
				lv.lines = lv.lines[len(lv.lines)-maxLogLines:]
			}
			lv.dirty = true
			lv.mu.Unlock()
		}
		if ctx.Err() == nil {
			lv.setStatus("stream ended")
		}
	}()
}

func (lv *logView) setStatus(s string) {
	lv.mu.Lock()
	lv.status = s
	lv.dirty = true
	lv.mu.Unlock()
}

func (lv *logView) stop() {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	if lv.cancel != nil {
		lv.cancel()
		lv.cancel = nil
	}
}

func (lv *logView) ticker() {
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		lv.mu.Lock()
		active, dirty := lv.cancel != nil, lv.dirty
		lv.mu.Unlock()
		if !active {
			return
		}
		if dirty {
			lv.app.tapp.QueueUpdateDraw(lv.redraw)
		}
	}
}

func (lv *logView) redraw() {
	lv.mu.Lock()
	lv.dirty = false
	needle := strings.ToLower(lv.needle)
	var b strings.Builder
	shown := 0
	for _, l := range lv.lines {
		if needle != "" {
			idx := strings.Index(strings.ToLower(l), needle)
			if idx < 0 {
				continue
			}
			b.WriteString(tview.Escape(l[:idx]) + "[black:yellow]" + tview.Escape(l[idx:idx+len(needle)]) + "[-:-]" + tview.Escape(l[idx+len(needle):]))
		} else {
			b.WriteString(colorLogLine(tview.Escape(l)))
		}
		b.WriteByte('\n')
		shown++
	}
	total, status := len(lv.lines), lv.status
	lv.mu.Unlock()

	flag := func(on bool, name string) string {
		if on {
			return "[green]" + name + ":on[gray]"
		}
		return "[gray]" + name + ":off"
	}
	tail := "500"
	if lv.allLines {
		tail = "all"
	}
	count := fmt.Sprint(total)
	if needle != "" {
		count = fmt.Sprintf("%d/%d", shown, total)
	}
	lv.tv.SetTitle(fmt.Sprintf(" [aqua::b]%s[white] [%s] %s %s %s [gray]tail:%s · %s · esc back ",
		tview.Escape(lv.title), count, flag(lv.follow, "follow"), flag(lv.timestamps, "timestamps"), flag(lv.wrap, "wrap"), tail, status))
	row, col := lv.tv.GetScrollOffset()
	lv.tv.SetText(b.String())
	if lv.follow {
		lv.tv.ScrollToEnd()
	} else {
		lv.tv.ScrollTo(row, col)
	}
}

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

var logLevel = regexp.MustCompile(`\b(ERROR|FATAL|WARN|WARNING|DEBUG|TRACE)\b`)

func colorLogLine(l string) string {
	m := logLevel.FindString(l)
	switch m {
	case "ERROR", "FATAL":
		return "[red]" + l + "[-]"
	case "WARN", "WARNING":
		return "[yellow]" + l + "[-]"
	case "DEBUG", "TRACE":
		return "[gray]" + l + "[-]"
	}
	return l
}
