package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
)

// Action is a key binding available on the selected row of a view.
type Action struct {
	// Key is the key that triggers the action; tcell.KeyRune means Rune is used.
	Key  tcell.Key
	Rune rune
	// Name is the short label shown in the header and help.
	Name string
	// Danger marks actions that change or remove resources.
	Danger bool
	Run    func(a *App, r *Row)
}

// Label renders the key as shown to the user, e.g. "l", "ctrl-d" or "enter".
func (k Action) Label() string {
	switch k.Key {
	case tcell.KeyRune:
		return string(k.Rune)
	case tcell.KeyEnter:
		return "enter"
	default:
		return strings.ToLower(strings.ReplaceAll(tcell.KeyNames[k.Key], "Ctrl-", "ctrl-"))
	}
}

// Matches reports whether a key event triggers the action.
func (k Action) Matches(ev *tcell.EventKey) bool {
	if k.Key == tcell.KeyRune {
		return ev.Key() == tcell.KeyRune && ev.Rune() == k.Rune
	}
	return ev.Key() == k.Key
}

func rk(r rune, name string, run func(a *App, r *Row)) Action {
	return Action{Key: tcell.KeyRune, Rune: r, Name: name, Run: run}
}

func kk(k tcell.Key, name string, run func(a *App, r *Row)) Action {
	return Action{Key: k, Name: name, Run: run}
}

func danger(a Action) Action { a.Danger = true; return a }

func asContainer(r *Row) Container { return r.Ref.(Container) }

func containerActions() []Action {
	logs := func(a *App, r *Row) { a.showContainerLogs(asContainer(r)) }
	return []Action{
		kk(tcell.KeyEnter, "Logs", logs),
		rk('l', "Logs", logs),
		rk('d', "Describe", func(a *App, r *Row) { a.showDescribe(KindContainer, r.ID, asContainer(r).Name) }),
		rk('s', "Shell", func(a *App, r *Row) { a.shell(asContainer(r)) }),
		rk('p', "Ports", func(a *App, r *Row) { a.showPorts(asContainer(r)) }),
		danger(rk('S', "Start", func(a *App, r *Row) {
			c := asContainer(r)
			a.runOp("start "+c.Name, func(ctx context.Context) error { return a.docker.ContainerStart(ctx, c.ID) })
		})),
		danger(rk('T', "Stop", func(a *App, r *Row) {
			c := asContainer(r)
			a.runOp("stop "+c.Name, func(ctx context.Context) error { return a.docker.ContainerStop(ctx, c.ID) })
		})),
		danger(rk('R', "Restart", func(a *App, r *Row) {
			c := asContainer(r)
			a.runOp("restart "+c.Name, func(ctx context.Context) error { return a.docker.ContainerRestart(ctx, c.ID) })
		})),
		danger(kk(tcell.KeyCtrlD, "Delete", func(a *App, r *Row) {
			c := asContainer(r)
			msg := fmt.Sprintf("Delete container %q?", c.Name)
			if c.State == "running" {
				msg += "\nIt is running and will be killed."
			}
			a.confirm(msg, "Delete", func() {
				a.runOp("delete "+c.Name, func(ctx context.Context) error { return a.docker.Remove(ctx, KindContainer, c.ID) })
			})
		})),
	}
}

func imageActions() []Action {
	return []Action{
		kk(tcell.KeyEnter, "Containers", func(a *App, r *Row) {
			im := r.Ref.(Image)
			a.pushView(&containersView{scope: "image=" + shortID(im.ID), match: func(c Container) bool { return c.ImageID == im.ID }})
		}),
		rk('d', "Describe", func(a *App, r *Row) {
			im := r.Ref.(Image)
			a.showDescribe(KindImage, im.ID, im.Repo+":"+im.Tag)
		}),
		danger(kk(tcell.KeyCtrlD, "Delete", func(a *App, r *Row) {
			im := r.Ref.(Image)
			ref := im.Repo + ":" + im.Tag
			if im.Repo == "<none>" {
				ref = im.ID
			}
			a.confirm(fmt.Sprintf("Delete image %q?", ref), "Delete", func() {
				a.runOp("delete image "+ref, func(ctx context.Context) error { return a.docker.Remove(ctx, KindImage, ref) })
			})
		})),
	}
}

func volumeActions() []Action {
	return []Action{
		kk(tcell.KeyEnter, "Containers", func(a *App, r *Row) {
			name := r.ID
			a.pushView(&containersView{scope: "volume=" + name, match: func(c Container) bool { return contains(c.Volumes, name) }})
		}),
		rk('d', "Describe", func(a *App, r *Row) { a.showDescribe(KindVolume, r.ID, r.ID) }),
		danger(kk(tcell.KeyCtrlD, "Delete", func(a *App, r *Row) {
			name := r.ID
			a.confirm(fmt.Sprintf("Delete volume %q?\nIts data is lost permanently.", name), "Delete", func() {
				a.runOp("delete volume "+name, func(ctx context.Context) error { return a.docker.Remove(ctx, KindVolume, name) })
			})
		})),
	}
}

func networkActions() []Action {
	return []Action{
		kk(tcell.KeyEnter, "Containers", func(a *App, r *Row) {
			name := r.Ref.(Network).Name
			a.pushView(&containersView{scope: "network=" + name, match: func(c Container) bool { return contains(c.Networks, name) }})
		}),
		rk('d', "Describe", func(a *App, r *Row) { a.showDescribe(KindNetwork, r.ID, r.Ref.(Network).Name) }),
		danger(kk(tcell.KeyCtrlD, "Delete", func(a *App, r *Row) {
			n := r.Ref.(Network)
			a.confirm(fmt.Sprintf("Delete network %q?", n.Name), "Delete", func() {
				a.runOp("delete network "+n.Name, func(ctx context.Context) error { return a.docker.Remove(ctx, KindNetwork, n.ID) })
			})
		})),
	}
}

func projectActions() []Action {
	return []Action{
		kk(tcell.KeyEnter, "Services", func(a *App, r *Row) { a.openServices(r.Ref.(ComposeProject)) }),
		rk('c', "Containers", func(a *App, r *Row) {
			name := r.ID
			a.pushView(&containersView{scope: "project=" + name, match: func(c Container) bool { return c.Project == name }})
		}),
		rk('l', "Logs", func(a *App, r *Row) { a.showComposeLogs(r.Ref.(ComposeProject), "") }),
		danger(rk('S', "Up", func(a *App, r *Row) { a.composeUp(r.Ref.(ComposeProject), "") })),
		danger(rk('T', "Stop", func(a *App, r *Row) { a.composeRun(r.Ref.(ComposeProject), "stop") })),
		danger(rk('R', "Restart", func(a *App, r *Row) { a.composeRun(r.Ref.(ComposeProject), "restart") })),
		danger(kk(tcell.KeyCtrlD, "Down", func(a *App, r *Row) {
			p := r.Ref.(ComposeProject)
			a.confirm(fmt.Sprintf("Run `docker compose down` for project %q?\nAll %d containers and its networks are removed (volumes are kept).", p.Name, p.Total), "Down", func() {
				a.composeRun(p, "down")
			})
		})),
	}
}

func serviceActions() []Action {
	ref := func(r *Row) serviceRef { return r.Ref.(serviceRef) }
	return []Action{
		kk(tcell.KeyEnter, "Containers", func(a *App, r *Row) {
			s := ref(r)
			a.pushView(&containersView{scope: "service=" + s.Project.Name + "/" + s.Service,
				match: func(c Container) bool { return c.Project == s.Project.Name && c.Service == s.Service }})
		}),
		rk('l', "Logs", func(a *App, r *Row) { s := ref(r); a.showComposeLogs(s.Project, s.Service) }),
		rk('s', "Shell", func(a *App, r *Row) {
			if c, ok := a.serviceContainer(ref(r)); ok {
				a.shell(c)
			}
		}),
		rk('d', "Describe", func(a *App, r *Row) {
			if c, ok := a.serviceContainer(ref(r)); ok {
				a.showDescribe(KindContainer, c.ID, c.Name)
			}
		}),
		danger(rk('S', "Up", func(a *App, r *Row) { s := ref(r); a.composeUp(s.Project, s.Service) })),
		danger(rk('T', "Stop", func(a *App, r *Row) { s := ref(r); a.composeRun(s.Project, "stop", s.Service) })),
		danger(rk('R', "Restart", func(a *App, r *Row) { s := ref(r); a.composeRun(s.Project, "restart", s.Service) })),
		danger(kk(tcell.KeyCtrlD, "Remove", func(a *App, r *Row) {
			s := ref(r)
			a.confirm(fmt.Sprintf("Stop and remove the containers of service %q (project %q)?", s.Service, s.Project.Name), "Remove", func() {
				a.composeRun(s.Project, "rm", "--stop", "--force", s.Service)
			})
		})),
	}
}

// serviceContainer picks the container of a service to shell into or describe,
// preferring a running one.
func (a *App) serviceContainer(s serviceRef) (Container, bool) {
	var found *Container
	for _, c := range a.snapshot().Containers {
		if c.Project == s.Project.Name && c.Service == s.Service && !c.OneOff {
			c := c
			if found == nil || (c.State == "running" && found.State != "running") {
				found = &c
			}
		}
	}
	if found == nil {
		a.flashError(fmt.Errorf("service %q has no container", s.Service))
		return Container{}, false
	}
	return *found, true
}

func (a *App) openServices(p ComposeProject) {
	v := &servicesView{project: p.Name}
	a.pushView(v)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		svcs, err := a.docker.ComposeServices(ctx, p)
		a.tapp.QueueUpdateDraw(func() {
			if err != nil {
				a.flashWarn("showing running services only: " + firstLine(err.Error()))
				return
			}
			v.declared = svcs
			a.render()
		})
	}()
}

func (a *App) composeUp(p ComposeProject, service string) {
	if !p.ComposeConfigAvailable() {
		a.composeRun(p, "start", service)
		return
	}
	a.composeRun(p, "up", "-d", service)
}

func (a *App) composeRun(p ComposeProject, args ...string) {
	var clean []string
	for _, s := range args {
		if s != "" {
			clean = append(clean, s)
		}
	}
	a.runOp("compose "+strings.Join(clean, " ")+" ("+p.Name+")", func(ctx context.Context) error {
		_, err := a.docker.Compose(ctx, p, clean...)
		return err
	})
}

func (a *App) showContainerLogs(c Container) {
	a.showLogs("Logs "+c.Name, func(ctx context.Context, follow, ts bool, tail string) (io.ReadCloser, error) {
		return a.docker.Logs(ctx, c, follow, ts, tail)
	})
}

func (a *App) showComposeLogs(p ComposeProject, service string) {
	title := "Logs " + p.Name
	if service != "" {
		title += "/" + service
	}
	a.showLogs(title, func(ctx context.Context, follow, ts bool, tail string) (io.ReadCloser, error) {
		return a.docker.ComposeLogs(ctx, p, service, follow, ts, tail)
	})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
