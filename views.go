package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
)

// Kind identifies the Docker resource type a row stands for.
type Kind string

const (
	KindContainer Kind = "container"
	KindImage     Kind = "image"
	KindVolume    Kind = "volume"
	KindNetwork   Kind = "network"
	KindProject   Kind = "project"
	KindService   Kind = "service"
)

// Column describes one table column.
type Column struct {
	Title   string
	Numeric bool
}

// Row is one table line of a resource view.
type Row struct {
	ID    string
	Kind  Kind
	Cells []string
	// Num holds numeric sort keys for numeric columns, by column index.
	Num map[int]float64
	// Color is the status color of the whole row.
	Color tcell.Color
	// Group is the heading the row is listed under when grouping is on.
	Group string
	Ref   any
}

// View is a resource table that can be switched to with `:` and drilled into.
type View interface {
	// Title names the resource kind shown, e.g. "Containers".
	Title() string
	// Scope describes a drill-down restriction, empty when the view is unrestricted.
	Scope() string
	// Columns returns the table columns; grouped tells whether rows are grouped.
	Columns(grouped bool) []Column
	// Rows builds the table rows from a snapshot.
	Rows(s *Snapshot, d *Docker, grouped bool) []Row
	// Actions lists the keys available on this view.
	Actions() []Action
	// Groupable reports whether rows carry a meaningful Group.
	Groupable() bool
	// DefaultSort returns the initial sort column and direction.
	DefaultSort() (col int, asc bool)
}

var (
	colorRunning = tcell.ColorGreen
	colorWarn    = tcell.ColorYellow
	colorError   = tcell.ColorRed
	colorIdle    = tcell.ColorGray
	colorCreated = tcell.ColorDodgerBlue
	colorNormal  = tcell.ColorSkyblue
)

func containerColor(c Container) tcell.Color {
	switch {
	case c.Health == "unhealthy" || c.State == "dead":
		return colorError
	case c.State == "restarting" || c.Health == "starting" || c.State == "paused":
		return colorWarn
	case c.State == "running":
		return colorRunning
	case c.State == "created":
		return colorCreated
	case c.State == "exited" && c.ExitCode != 0 && c.ExitCode != 137 && c.ExitCode != 143:
		return colorError
	default:
		return colorIdle
	}
}

func stateText(c Container) string {
	if c.State == "exited" {
		return fmt.Sprintf("exited(%d)", c.ExitCode)
	}
	return c.State
}

// ---- containers ------------------------------------------------------------

type containersView struct {
	scope string
	match func(Container) bool
}

func (v *containersView) Title() string            { return "Containers" }
func (v *containersView) Scope() string            { return v.scope }
func (v *containersView) Groupable() bool          { return true }
func (v *containersView) DefaultSort() (int, bool) { return 0, true }
func (v *containersView) Actions() []Action        { return containerActions() }

func (v *containersView) Columns(grouped bool) []Column {
	cols := []Column{{Title: "NAME"}}
	if !grouped {
		cols = append(cols, Column{Title: "PROJECT"})
	}
	return append(cols,
		Column{Title: "SERVICE"}, Column{Title: "IMAGE"}, Column{Title: "STATE"}, Column{Title: "HEALTH"},
		Column{Title: "CPU%", Numeric: true}, Column{Title: "MEM", Numeric: true}, Column{Title: "MEM%", Numeric: true},
		Column{Title: "PORTS"}, Column{Title: "UPTIME", Numeric: true}, Column{Title: "AGE", Numeric: true},
	)
}

func (v *containersView) Rows(s *Snapshot, d *Docker, grouped bool) []Row {
	var rows []Row
	for _, c := range s.Containers {
		if v.match != nil && !v.match(c) {
			continue
		}
		cpu, mem, memPct := "-", "-", "-"
		num := map[int]float64{}
		if st, ok := d.StatsFor(c.ID); ok {
			cpu = fmt.Sprintf("%.1f", st.CPUPercent)
			mem = humanBytes(int64(st.MemUsage))
			if st.MemLimit > 0 {
				p := float64(st.MemUsage) / float64(st.MemLimit) * 100
				memPct = fmt.Sprintf("%.1f", p)
				num[-3] = p
			}
			num[-1], num[-2] = st.CPUPercent, float64(st.MemUsage)
		}
		uptime := "-"
		if c.State == "running" && !c.Started.IsZero() {
			uptime = humanDuration(time.Since(c.Started))
			num[-4] = time.Since(c.Started).Seconds()
		}
		num[-5] = time.Since(c.Created).Seconds()
		health := c.Health
		if health == "" {
			health = "-"
		}
		cells := []string{c.Name}
		if !grouped {
			cells = append(cells, orDash(c.Project))
		}
		cells = append(cells, orDash(c.Service), shortImage(c.Image), stateText(c), health,
			cpu, mem, memPct, portsText(c.Ports), uptime, humanDuration(time.Since(c.Created)))
		rows = append(rows, Row{
			ID: c.ID, Kind: KindContainer, Cells: cells, Num: remapNum(num, len(cells)),
			Color: containerColor(c), Group: c.Project, Ref: c,
		})
	}
	return rows
}

// remapNum turns the negative "from the end" keys used by views with optional
// columns into absolute column indexes; trailing columns are CPU% MEM MEM% PORTS UPTIME AGE.
func remapNum(num map[int]float64, n int) map[int]float64 {
	out := map[int]float64{}
	pos := map[int]int{-1: n - 6, -2: n - 5, -3: n - 4, -4: n - 2, -5: n - 1}
	for k, val := range num {
		if k < 0 {
			out[pos[k]] = val
		} else {
			out[k] = val
		}
	}
	return out
}

// ---- images ----------------------------------------------------------------

type imagesView struct{}

func (v *imagesView) Title() string            { return "Images" }
func (v *imagesView) Scope() string            { return "" }
func (v *imagesView) Groupable() bool          { return false }
func (v *imagesView) DefaultSort() (int, bool) { return 0, true }
func (v *imagesView) Actions() []Action        { return imageActions() }
func (v *imagesView) Columns(bool) []Column {
	return []Column{{Title: "REPOSITORY"}, {Title: "TAG"}, {Title: "IMAGE ID"},
		{Title: "SIZE", Numeric: true}, {Title: "CONTAINERS", Numeric: true}, {Title: "AGE", Numeric: true}}
}

func (v *imagesView) Rows(s *Snapshot, _ *Docker, _ bool) []Row {
	rows := make([]Row, 0, len(s.Images))
	for _, im := range s.Images {
		color := colorNormal
		if im.Containers > 0 {
			color = colorRunning
		} else if im.Repo == "<none>" {
			color = colorIdle
		}
		rows = append(rows, Row{
			ID: im.ID + "|" + im.Repo + ":" + im.Tag, Kind: KindImage, Ref: im, Color: color,
			Cells: []string{im.Repo, im.Tag, shortID(im.ID), humanBytes(im.Size), fmt.Sprint(im.Containers), humanDuration(time.Since(im.Created))},
			Num:   map[int]float64{3: float64(im.Size), 4: float64(im.Containers), 5: time.Since(im.Created).Seconds()},
		})
	}
	return rows
}

// ---- volumes ---------------------------------------------------------------

type volumesView struct{}

func (v *volumesView) Title() string            { return "Volumes" }
func (v *volumesView) Scope() string            { return "" }
func (v *volumesView) Groupable() bool          { return true }
func (v *volumesView) DefaultSort() (int, bool) { return 0, true }
func (v *volumesView) Actions() []Action        { return volumeActions() }
func (v *volumesView) Columns(grouped bool) []Column {
	cols := []Column{{Title: "NAME"}}
	if !grouped {
		cols = append(cols, Column{Title: "PROJECT"})
	}
	return append(cols, Column{Title: "DRIVER"}, Column{Title: "USED BY", Numeric: true}, Column{Title: "MOUNTPOINT"}, Column{Title: "AGE", Numeric: true})
}

func (v *volumesView) Rows(s *Snapshot, _ *Docker, grouped bool) []Row {
	rows := make([]Row, 0, len(s.Volumes))
	for _, vol := range s.Volumes {
		cells := []string{vol.Name}
		if !grouped {
			cells = append(cells, orDash(vol.Project))
		}
		age, ageNum := "-", 0.0
		if !vol.Created.IsZero() {
			age, ageNum = humanDuration(time.Since(vol.Created)), time.Since(vol.Created).Seconds()
		}
		cells = append(cells, vol.Driver, fmt.Sprint(vol.UsedBy), vol.Mountpoint, age)
		n := len(cells)
		color := colorIdle
		if vol.UsedBy > 0 {
			color = colorRunning
		}
		rows = append(rows, Row{ID: vol.Name, Kind: KindVolume, Ref: vol, Cells: cells, Color: color, Group: vol.Project,
			Num: map[int]float64{n - 3: float64(vol.UsedBy), n - 1: ageNum}})
	}
	return rows
}

// ---- networks --------------------------------------------------------------

type networksView struct{}

func (v *networksView) Title() string            { return "Networks" }
func (v *networksView) Scope() string            { return "" }
func (v *networksView) Groupable() bool          { return false }
func (v *networksView) DefaultSort() (int, bool) { return 0, true }
func (v *networksView) Actions() []Action        { return networkActions() }
func (v *networksView) Columns(bool) []Column {
	return []Column{{Title: "NAME"}, {Title: "NETWORK ID"}, {Title: "DRIVER"}, {Title: "SCOPE"},
		{Title: "PROJECT"}, {Title: "CONTAINERS", Numeric: true}, {Title: "AGE", Numeric: true}}
}

func (v *networksView) Rows(s *Snapshot, _ *Docker, _ bool) []Row {
	rows := make([]Row, 0, len(s.Networks))
	for _, n := range s.Networks {
		color := colorIdle
		if n.Containers > 0 {
			color = colorRunning
		}
		rows = append(rows, Row{ID: n.ID, Kind: KindNetwork, Ref: n, Color: color,
			Cells: []string{n.Name, shortID(n.ID), n.Driver, n.Scope, orDash(n.Project), fmt.Sprint(n.Containers), humanDuration(time.Since(n.Created))},
			Num:   map[int]float64{5: float64(n.Containers), 6: time.Since(n.Created).Seconds()}})
	}
	return rows
}

// ---- compose projects ------------------------------------------------------

type projectsView struct{}

func (v *projectsView) Title() string            { return "Compose" }
func (v *projectsView) Scope() string            { return "" }
func (v *projectsView) Groupable() bool          { return false }
func (v *projectsView) DefaultSort() (int, bool) { return 0, true }
func (v *projectsView) Actions() []Action        { return projectActions() }
func (v *projectsView) Columns(bool) []Column {
	return []Column{{Title: "PROJECT"}, {Title: "RUNNING", Numeric: true}, {Title: "STATUS"},
		{Title: "CPU%", Numeric: true}, {Title: "MEM", Numeric: true}, {Title: "CONFIG FILES"}}
}

func (v *projectsView) Rows(s *Snapshot, d *Docker, _ bool) []Row {
	rows := make([]Row, 0, len(s.Projects))
	for _, p := range s.Projects {
		var cpu float64
		var mem uint64
		for _, c := range s.Containers {
			if c.Project == p.Name {
				if st, ok := d.StatsFor(c.ID); ok {
					cpu += st.CPUPercent
					mem += st.MemUsage
				}
			}
		}
		status, color := "stopped", colorIdle
		switch {
		case p.Running == p.Total:
			status, color = "running", colorRunning
		case p.Running > 0:
			status, color = "partial", colorWarn
		}
		cfg := strings.Join(p.ConfigFiles, ",")
		if !p.ComposeConfigAvailable() {
			cfg += " (missing)"
		}
		rows = append(rows, Row{ID: p.Name, Kind: KindProject, Ref: p, Color: color,
			Cells: []string{p.Name, fmt.Sprintf("%d/%d", p.Running, p.Total), status, fmt.Sprintf("%.1f", cpu), humanBytes(int64(mem)), cfg},
			Num:   map[int]float64{1: float64(p.Running), 3: cpu, 4: float64(mem)}})
	}
	return rows
}

// ---- compose services ------------------------------------------------------

// serviceRef identifies a compose service row and carries what actions need.
type serviceRef struct {
	Project ComposeProject
	Service string
}

type servicesView struct {
	project  string
	declared []string
}

func (v *servicesView) Title() string            { return "Services" }
func (v *servicesView) Scope() string            { return "project=" + v.project }
func (v *servicesView) Groupable() bool          { return false }
func (v *servicesView) DefaultSort() (int, bool) { return 0, true }
func (v *servicesView) Actions() []Action        { return serviceActions() }
func (v *servicesView) Columns(bool) []Column {
	return []Column{{Title: "SERVICE"}, {Title: "RUNNING", Numeric: true}, {Title: "STATE"}, {Title: "HEALTH"},
		{Title: "IMAGE"}, {Title: "CPU%", Numeric: true}, {Title: "MEM", Numeric: true}, {Title: "PORTS"}}
}

func (v *servicesView) Rows(s *Snapshot, d *Docker, _ bool) []Row {
	var project ComposeProject
	for _, p := range s.Projects {
		if p.Name == v.project {
			project = p
		}
	}
	if project.Name == "" {
		project.Name = v.project
	}
	type agg struct {
		running, total int
		states, health map[string]bool
		image          string
		cpu            float64
		mem            uint64
		ports          []PortMapping
		worst          tcell.Color
	}
	byService := map[string]*agg{}
	get := func(name string) *agg {
		a, ok := byService[name]
		if !ok {
			a = &agg{states: map[string]bool{}, health: map[string]bool{}, worst: colorIdle}
			byService[name] = a
		}
		return a
	}
	for _, name := range v.declared {
		get(name)
	}
	for _, c := range s.Containers {
		if c.Project != v.project || c.OneOff {
			continue
		}
		a := get(c.Service)
		a.total++
		if c.State == "running" {
			a.running++
		}
		a.states[stateText(c)] = true
		if c.Health != "" {
			a.health[c.Health] = true
		}
		a.image = shortImage(c.Image)
		a.ports = append(a.ports, c.Ports...)
		if st, ok := d.StatsFor(c.ID); ok {
			a.cpu += st.CPUPercent
			a.mem += st.MemUsage
		}
		if col := containerColor(c); a.worst == colorIdle || col == colorError || (col == colorWarn && a.worst != colorError) {
			a.worst = col
		}
	}
	rows := make([]Row, 0, len(byService))
	for name, a := range byService {
		state, color := strings.Join(sortedKeys(a.states), ","), a.worst
		if a.total == 0 {
			state, color = "not created", colorCreated
		}
		health := strings.Join(sortedKeys(a.health), ",")
		rows = append(rows, Row{ID: name, Kind: KindService, Ref: serviceRef{Project: project, Service: name}, Color: color,
			Cells: []string{name, fmt.Sprintf("%d/%d", a.running, a.total), state, orDash(health), orDash(a.image),
				fmt.Sprintf("%.1f", a.cpu), humanBytes(int64(a.mem)), portsText(a.ports)},
			Num: map[int]float64{1: float64(a.running), 5: a.cpu, 6: float64(a.mem)}})
	}
	return rows
}

// ---- formatting helpers ----------------------------------------------------

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func shortImage(img string) string {
	if strings.HasPrefix(img, "sha256:") {
		return shortID(img)
	}
	if i := strings.Index(img, "@sha256:"); i > 0 {
		return img[:i]
	}
	return img
}

func portsText(ports []PortMapping) string {
	seen := map[string]bool{}
	var parts []string
	for _, p := range ports {
		if p.Public == 0 {
			continue
		}
		s := fmt.Sprintf("%d→%d", p.Public, p.Private)
		if p.Proto != "" && p.Proto != "tcp" {
			s += "/" + p.Proto
		}
		if !seen[s] {
			seen[s] = true
			parts = append(parts, s)
		}
	}
	sort.Slice(parts, func(i, j int) bool {
		return len(parts[i]) < len(parts[j]) || (len(parts[i]) == len(parts[j]) && parts[i] < parts[j])
	})
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ci", float64(b)/float64(div), "KMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	default:
		return fmt.Sprintf("%dy%dd", int(d.Hours())/24/365, int(d.Hours())/24%365)
	}
}
