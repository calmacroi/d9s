package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	labelProject     = "com.docker.compose.project"
	labelService     = "com.docker.compose.service"
	labelConfigFiles = "com.docker.compose.project.config_files"
	labelWorkingDir  = "com.docker.compose.project.working_dir"
	labelEnvFile     = "com.docker.compose.project.environment_file"
	labelOneOff      = "com.docker.compose.oneoff"
)

// Docker is the gateway to the local Docker daemon, both through the Engine API
// and through the docker CLI for interactive and compose operations.
type Docker struct {
	api         *client.Client
	ContextName string
	Host        string

	statsMu sync.Mutex
	stats   map[string]Stats
	streams map[string]context.CancelFunc

	inspectMu sync.Mutex
	inspects  map[string]inspectCache
}

type inspectCache struct {
	state   string
	started time.Time
	tty     bool
}

// Stats is the latest resource usage sample of a running container.
type Stats struct {
	CPUPercent float64
	MemUsage   uint64
	MemLimit   uint64
}

// PortMapping is one published or exposed container port.
type PortMapping struct {
	IP      string
	Private uint16
	Public  uint16
	Proto   string
}

// Container is a flattened view of a container enriched with compose metadata.
type Container struct {
	ID       string
	Name     string
	Image    string
	ImageID  string
	State    string
	Status   string
	Health   string
	ExitCode int
	Project  string
	Service  string
	OneOff   bool
	Created  time.Time
	Started  time.Time
	Tty      bool
	Ports    []PortMapping
	Networks []string
	Volumes  []string
	Labels   map[string]string
}

// Image is one tag (or the untagged form) of a local image.
type Image struct {
	ID         string
	Repo       string
	Tag        string
	Size       int64
	Created    time.Time
	Containers int
}

// Volume is a local volume with its compose ownership and users.
type Volume struct {
	Name       string
	Driver     string
	Mountpoint string
	Project    string
	Created    time.Time
	UsedBy     int
}

// Network is a Docker network with the number of attached containers.
type Network struct {
	ID         string
	Name       string
	Driver     string
	Scope      string
	Project    string
	Created    time.Time
	Containers int
}

// ComposeProject is a compose project reconstructed from container labels.
type ComposeProject struct {
	Name        string
	WorkingDir  string
	ConfigFiles []string
	EnvFiles    []string
	Running     int
	Total       int
}

// Snapshot is a consistent picture of all resources taken in one refresh.
type Snapshot struct {
	Taken      time.Time
	Containers []Container
	Images     []Image
	Volumes    []Volume
	Networks   []Network
	Projects   []ComposeProject
	Info       DaemonInfo
	Err        error
}

// DaemonInfo describes the daemon shown in the header.
type DaemonInfo struct {
	ServerVersion string
	APIVersion    string
	OS            string
	NCPU          int
	MemTotal      int64
}

// NewDocker connects to the daemon of the given docker context, or of the
// current one (honoring DOCKER_HOST) when contextName is empty.
func NewDocker(contextName string) (*Docker, error) {
	d := &Docker{
		stats:    map[string]Stats{},
		streams:  map[string]context.CancelFunc{},
		inspects: map[string]inspectCache{},
	}
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if contextName == "" && os.Getenv("DOCKER_HOST") != "" {
		d.Host = os.Getenv("DOCKER_HOST")
		d.ContextName = "env:DOCKER_HOST"
		opts = append(opts, client.FromEnv)
	} else {
		if contextName == "" {
			contextName = cliOutput("context", "show")
		}
		if contextName == "" {
			contextName = "default"
		}
		d.ContextName = contextName
		d.Host = cliOutput("context", "inspect", "--format", "{{.Endpoints.docker.Host}}", contextName)
		if d.Host != "" {
			opts = append(opts, client.WithHost(d.Host))
		} else {
			opts = append(opts, client.FromEnv)
		}
	}
	api, err := client.New(opts...)
	if err != nil {
		return nil, err
	}
	d.api = api
	return d, nil
}

func cliOutput(args ...string) string {
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// CLIArgs prefixes docker CLI arguments so the CLI talks to the same daemon.
func (d *Docker) CLIArgs(args ...string) []string {
	if d.ContextName == "" || strings.HasPrefix(d.ContextName, "env:") {
		return args
	}
	return append([]string{"--context", d.ContextName}, args...)
}

// Close stops all background stats streams and releases the API client.
func (d *Docker) Close() {
	d.statsMu.Lock()
	for _, cancel := range d.streams {
		cancel()
	}
	d.statsMu.Unlock()
	_ = d.api.Close()
}

// Fetch takes a full snapshot of containers, images, volumes and networks.
func (d *Docker) Fetch(ctx context.Context) *Snapshot {
	s := &Snapshot{Taken: time.Now()}
	containers, err := d.listContainers(ctx)
	if err != nil {
		s.Err = err
		return s
	}
	s.Containers = containers
	d.syncStats(containers)

	var wg sync.WaitGroup
	var imgErr, volErr, netErr, infoErr error
	wg.Add(4)
	go func() { defer wg.Done(); s.Images, imgErr = d.listImages(ctx, containers) }()
	go func() { defer wg.Done(); s.Volumes, volErr = d.listVolumes(ctx, containers) }()
	go func() { defer wg.Done(); s.Networks, netErr = d.listNetworks(ctx, containers) }()
	go func() { defer wg.Done(); s.Info, infoErr = d.daemonInfo(ctx) }()
	wg.Wait()
	for _, e := range []error{imgErr, volErr, netErr, infoErr} {
		if e != nil && s.Err == nil {
			s.Err = e
		}
	}
	s.Projects = projectsOf(containers)
	return s
}

func (d *Docker) listContainers(ctx context.Context) ([]Container, error) {
	res, err := d.api.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(res.Items))
	for _, c := range res.Items {
		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		ct := Container{
			ID:      c.ID,
			Name:    name,
			Image:   c.Image,
			ImageID: c.ImageID,
			State:   string(c.State),
			Status:  c.Status,
			Project: c.Labels[labelProject],
			Service: c.Labels[labelService],
			OneOff:  c.Labels[labelOneOff] == "True",
			Created: time.Unix(c.Created, 0),
			Labels:  c.Labels,
		}
		if c.Health != nil {
			ct.Health = string(c.Health.Status)
			if ct.Health == "none" {
				ct.Health = ""
			}
		}
		if ct.State == "exited" {
			ct.ExitCode = parseExitCode(c.Status)
		}
		for _, p := range c.Ports {
			pm := PortMapping{Private: p.PrivatePort, Public: p.PublicPort, Proto: p.Type}
			if p.IP.IsValid() {
				pm.IP = p.IP.String()
			}
			ct.Ports = append(ct.Ports, pm)
		}
		if c.NetworkSettings != nil {
			for n := range c.NetworkSettings.Networks {
				ct.Networks = append(ct.Networks, n)
			}
			sort.Strings(ct.Networks)
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name != "" {
				ct.Volumes = append(ct.Volumes, m.Name)
			}
		}
		ic := d.inspectFor(ctx, c.ID, ct.State)
		ct.Started, ct.Tty = ic.started, ic.tty
		out = append(out, ct)
	}
	d.pruneInspects(out)
	return out, nil
}

// inspectFor returns start time and TTY mode, re-inspecting only when the
// container's state changed since the last refresh.
func (d *Docker) inspectFor(ctx context.Context, id, state string) inspectCache {
	d.inspectMu.Lock()
	ic, ok := d.inspects[id]
	d.inspectMu.Unlock()
	if ok && ic.state == state {
		return ic
	}
	res, err := d.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return inspectCache{state: state}
	}
	ic = inspectCache{state: state}
	if res.Container.State != nil {
		ic.started, _ = time.Parse(time.RFC3339Nano, res.Container.State.StartedAt)
	}
	if res.Container.Config != nil {
		ic.tty = res.Container.Config.Tty
	}
	d.inspectMu.Lock()
	d.inspects[id] = ic
	d.inspectMu.Unlock()
	return ic
}

func (d *Docker) pruneInspects(live []Container) {
	keep := map[string]bool{}
	for _, c := range live {
		keep[c.ID] = true
	}
	d.inspectMu.Lock()
	for id := range d.inspects {
		if !keep[id] {
			delete(d.inspects, id)
		}
	}
	d.inspectMu.Unlock()
}

func parseExitCode(status string) int {
	open, end := strings.Index(status, "("), strings.Index(status, ")")
	if open < 0 || end < open {
		return 0
	}
	n, _ := strconv.Atoi(status[open+1 : end])
	return n
}

func (d *Docker) listImages(ctx context.Context, containers []Container) ([]Image, error) {
	res, err := d.api.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return nil, err
	}
	used := map[string]int{}
	for _, c := range containers {
		used[c.ImageID]++
	}
	var out []Image
	for _, im := range res.Items {
		base := Image{ID: im.ID, Size: im.Size, Created: time.Unix(im.Created, 0), Containers: used[im.ID]}
		tags := im.RepoTags
		if len(tags) == 0 {
			tags = []string{"<none>:<none>"}
		}
		for _, t := range tags {
			img := base
			if i := strings.LastIndex(t, ":"); i > 0 && !strings.Contains(t[i:], "/") {
				img.Repo, img.Tag = t[:i], t[i+1:]
			} else {
				img.Repo, img.Tag = t, "latest"
			}
			out = append(out, img)
		}
	}
	return out, nil
}

func (d *Docker) listVolumes(ctx context.Context, containers []Container) ([]Volume, error) {
	res, err := d.api.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return nil, err
	}
	used := map[string]int{}
	for _, c := range containers {
		for _, v := range c.Volumes {
			used[v]++
		}
	}
	out := make([]Volume, 0, len(res.Items))
	for _, v := range res.Items {
		created, _ := time.Parse(time.RFC3339, v.CreatedAt)
		out = append(out, Volume{
			Name: v.Name, Driver: v.Driver, Mountpoint: v.Mountpoint,
			Project: v.Labels[labelProject], Created: created, UsedBy: used[v.Name],
		})
	}
	return out, nil
}

func (d *Docker) listNetworks(ctx context.Context, containers []Container) ([]Network, error) {
	res, err := d.api.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, err
	}
	used := map[string]int{}
	for _, c := range containers {
		for _, n := range c.Networks {
			used[n]++
		}
	}
	out := make([]Network, 0, len(res.Items))
	for _, n := range res.Items {
		out = append(out, Network{
			ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope,
			Project: n.Labels[labelProject], Created: n.Created, Containers: used[n.Name],
		})
	}
	return out, nil
}

func (d *Docker) daemonInfo(ctx context.Context) (DaemonInfo, error) {
	v, err := d.api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return DaemonInfo{}, err
	}
	info := DaemonInfo{ServerVersion: v.Version, APIVersion: v.APIVersion, OS: v.Os + "/" + v.Arch}
	if i, err := d.api.Info(ctx, client.InfoOptions{}); err == nil {
		info.NCPU, info.MemTotal = i.Info.NCPU, i.Info.MemTotal
		if i.Info.OperatingSystem != "" {
			info.OS = i.Info.OperatingSystem
		}
	}
	return info, nil
}

func projectsOf(containers []Container) []ComposeProject {
	byName := map[string]*ComposeProject{}
	for _, c := range containers {
		if c.Project == "" {
			continue
		}
		p, ok := byName[c.Project]
		if !ok {
			p = &ComposeProject{Name: c.Project}
			byName[c.Project] = p
		}
		if p.WorkingDir == "" {
			p.WorkingDir = c.Labels[labelWorkingDir]
		}
		if len(p.ConfigFiles) == 0 && c.Labels[labelConfigFiles] != "" {
			p.ConfigFiles = strings.Split(c.Labels[labelConfigFiles], ",")
		}
		if len(p.EnvFiles) == 0 && c.Labels[labelEnvFile] != "" {
			p.EnvFiles = strings.Split(c.Labels[labelEnvFile], ",")
		}
		p.Total++
		if c.State == "running" {
			p.Running++
		}
	}
	out := make([]ComposeProject, 0, len(byName))
	for _, p := range byName {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// StatsFor returns the latest usage sample of a container, if one is known.
func (d *Docker) StatsFor(id string) (Stats, bool) {
	d.statsMu.Lock()
	defer d.statsMu.Unlock()
	s, ok := d.stats[id]
	return s, ok
}

func (d *Docker) syncStats(containers []Container) {
	running := map[string]bool{}
	for _, c := range containers {
		if c.State == "running" {
			running[c.ID] = true
		}
	}
	d.statsMu.Lock()
	defer d.statsMu.Unlock()
	for id, cancel := range d.streams {
		if !running[id] {
			cancel()
			delete(d.streams, id)
			delete(d.stats, id)
		}
	}
	for id := range running {
		if _, ok := d.streams[id]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		d.streams[id] = cancel
		go d.streamStats(ctx, id)
	}
}

func (d *Docker) streamStats(ctx context.Context, id string) {
	defer func() {
		d.statsMu.Lock()
		if ctx.Err() == nil {
			delete(d.streams, id)
		}
		d.statsMu.Unlock()
	}()
	res, err := d.api.ContainerStats(ctx, id, client.ContainerStatsOptions{Stream: true})
	if err != nil {
		return
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	for {
		var s container.StatsResponse
		if err := dec.Decode(&s); err != nil {
			return
		}
		st := Stats{MemUsage: memUsage(s.MemoryStats), MemLimit: s.MemoryStats.Limit}
		cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
		sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
		if sysDelta > 0 && cpuDelta > 0 {
			cpus := float64(s.CPUStats.OnlineCPUs)
			if cpus == 0 {
				cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
			}
			st.CPUPercent = cpuDelta / sysDelta * cpus * 100
		}
		d.statsMu.Lock()
		d.stats[id] = st
		d.statsMu.Unlock()
	}
}

// memUsage mirrors `docker stats`, which excludes reclaimable page cache.
func memUsage(m container.MemoryStats) uint64 {
	if v, ok := m.Stats["inactive_file"]; ok && v < m.Usage {
		return m.Usage - v
	}
	if v, ok := m.Stats["total_inactive_file"]; ok && v < m.Usage {
		return m.Usage - v
	}
	return m.Usage
}

// Logs opens a log stream of a container as plain text lines.
func (d *Docker) Logs(ctx context.Context, c Container, follow, timestamps bool, tail string) (io.ReadCloser, error) {
	rc, err := d.api.ContainerLogs(ctx, c.ID, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: follow, Timestamps: timestamps, Tail: tail,
	})
	if err != nil {
		return nil, err
	}
	if c.Tty {
		return rc, nil
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		pw.CloseWithError(err)
	}()
	return &joinedCloser{Reader: pr, closers: []io.Closer{rc, pr}}, nil
}

type joinedCloser struct {
	io.Reader
	closers []io.Closer
}

func (j *joinedCloser) Close() error {
	for _, c := range j.closers {
		_ = c.Close()
	}
	return nil
}

// InspectJSON returns the raw inspect document of a resource.
func (d *Docker) InspectJSON(ctx context.Context, kind Kind, id string) ([]byte, error) {
	var raw any
	switch kind {
	case KindContainer:
		res, err := d.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return nil, err
		}
		raw = res.Raw
	case KindImage:
		res, err := d.api.ImageInspect(ctx, id)
		if err != nil {
			return nil, err
		}
		raw = res.InspectResponse
	case KindVolume:
		res, err := d.api.VolumeInspect(ctx, id, client.VolumeInspectOptions{})
		if err != nil {
			return nil, err
		}
		raw = res.Raw
	case KindNetwork:
		res, err := d.api.NetworkInspect(ctx, id, client.NetworkInspectOptions{})
		if err != nil {
			return nil, err
		}
		raw = res.Raw
	default:
		return nil, fmt.Errorf("inspect not supported for %s", kind)
	}
	return json.MarshalIndent(raw, "", "  ")
}

// ContainerStart starts a stopped container.
func (d *Docker) ContainerStart(ctx context.Context, id string) error {
	_, err := d.api.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

// ContainerStop stops a running container gracefully.
func (d *Docker) ContainerStop(ctx context.Context, id string) error {
	_, err := d.api.ContainerStop(ctx, id, client.ContainerStopOptions{})
	return err
}

// ContainerRestart restarts a container.
func (d *Docker) ContainerRestart(ctx context.Context, id string) error {
	_, err := d.api.ContainerRestart(ctx, id, client.ContainerRestartOptions{})
	return err
}

// Remove deletes a resource; running containers are force-removed.
func (d *Docker) Remove(ctx context.Context, kind Kind, id string) error {
	var err error
	switch kind {
	case KindContainer:
		_, err = d.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	case KindImage:
		_, err = d.api.ImageRemove(ctx, id, client.ImageRemoveOptions{})
	case KindVolume:
		_, err = d.api.VolumeRemove(ctx, id, client.VolumeRemoveOptions{})
	case KindNetwork:
		_, err = d.api.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
	default:
		err = fmt.Errorf("delete not supported for %s", kind)
	}
	return err
}
