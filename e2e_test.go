//go:build e2e

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// TestE2E drives the real UI on a simulated screen against the local daemon.
// It only mutates resources of the throwaway e2e/compose.yml project and
// resources named d9s-e2e-*, which `make e2e` creates beforehand.
type driver struct {
	t      *testing.T
	a      *App
	screen tcell.SimulationScreen
	out    string
}

func (d *driver) text() string {
	cells, w, h := d.screen.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Bytes) == 0 {
				b.WriteByte(' ')
			} else {
				b.Write(c.Bytes)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (d *driver) dump(name string) {
	if d.out != "" {
		_ = os.WriteFile(filepath.Join(d.out, name+".txt"), []byte(d.text()), 0o644)
	}
}

func (d *driver) waitFor(what string, cond func(string) bool, timeout time.Duration) string {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d.a.tapp.QueueUpdateDraw(func() {})
		time.Sleep(150 * time.Millisecond)
		if s := d.text(); cond(s) {
			return s
		}
	}
	d.dump("FAIL-" + strings.ReplaceAll(what, " ", "_"))
	d.t.Fatalf("timed out waiting for %s; screen:\n%s", what, d.text())
	return ""
}

func (d *driver) waitText(sub string) string {
	d.t.Helper()
	return d.waitFor(fmt.Sprintf("%q", sub), func(s string) bool { return strings.Contains(s, sub) }, 20*time.Second)
}

func (d *driver) waitGone(sub string) {
	d.t.Helper()
	d.waitFor(fmt.Sprintf("%q gone", sub), func(s string) bool { return !strings.Contains(s, sub) }, 30*time.Second)
}

func (d *driver) keys(s string) {
	for _, r := range s {
		d.screen.InjectKey(tcell.KeyRune, r, tcell.ModNone)
		time.Sleep(20 * time.Millisecond)
	}
}

func (d *driver) key(k tcell.Key) {
	d.screen.InjectKey(k, 0, tcell.ModNone)
	time.Sleep(80 * time.Millisecond)
}

func (d *driver) cmd(c string) {
	d.keys(":" + c)
	d.key(tcell.KeyEnter)
	time.Sleep(200 * time.Millisecond)
}

func (d *driver) filter(f string) {
	d.keys("/" + f)
	d.key(tcell.KeyEnter)
	time.Sleep(200 * time.Millisecond)
}

// selected returns the first cell of the selected row, read on the UI goroutine.
func (d *driver) selected() string {
	ch := make(chan string, 1)
	d.a.tapp.QueueUpdate(func() {
		if r := d.a.selectedRow(); r != nil {
			ch <- r.Cells[0]
		} else {
			ch <- ""
		}
	})
	return <-ch
}

func (d *driver) requireSelected(name string) {
	d.t.Helper()
	if got := d.selected(); got != name {
		d.t.Fatalf("expected %q selected, got %q — refusing to continue", name, got)
	}
}

// confirmDanger presses the destructive button of the confirm dialog.
func (d *driver) confirmDanger() {
	d.key(tcell.KeyTab)
	d.key(tcell.KeyEnter)
}

func TestE2E(t *testing.T) {
	dock, err := NewDocker("")
	if err != nil {
		t.Fatal(err)
	}
	defer dock.Close()
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	a := NewApp(dock, time.Second, "containers")
	a.tapp.SetScreen(screen)
	done := make(chan error, 1)
	go func() { done <- a.Run() }()
	d := &driver{t: t, a: a, screen: screen, out: os.Getenv("D9S_E2E_OUT")}
	time.Sleep(300 * time.Millisecond)
	screen.SetSize(220, 50)
	a.tapp.QueueUpdateDraw(func() {})

	// containers view, grouped by compose project, header info
	d.waitText("▼ d9s-e2e")
	d.waitText("▼ symbiocommerce")
	d.waitText("Context:")
	d.dump("01-containers")

	// live filter + stats + health
	d.filter("d9s-e2e")
	s := d.waitFor("cpu stats of web", func(s string) bool {
		return strings.Contains(s, "d9s-e2e-web-1") && strings.Contains(s, "healthy") && strings.Contains(s, "Ki ")
	}, 20*time.Second)
	if strings.Contains(s, "▼ symbiocommerce") {
		t.Fatal("filter did not hide other projects")
	}
	d.waitText("18089→8080")
	d.dump("02-filtered")

	// sorting and grouping toggle
	d.keys(">>>")
	d.waitText("STATE↑")
	d.keys("=")
	d.waitText("STATE↓")
	d.key(tcell.KeyCtrlG)
	d.waitText("PROJECT")
	d.dump("03-sorted-ungrouped")
	d.key(tcell.KeyCtrlG)
	d.keys("<<<")
	d.waitText("NAME↓")
	d.keys("=")

	// logs: follow, timestamps, filter
	d.key(tcell.KeyEscape)
	d.filter("d9s-e2e-worker")
	d.requireSelected("d9s-e2e-worker-1")
	d.keys("l")
	d.waitText("tick")
	d.waitText("follow:on")
	d.keys("t")
	d.waitFor("timestamps", func(s string) bool {
		return strings.Contains(s, "timestamps:on") && strings.Contains(s, time.Now().UTC().Format("2006-01-02T"))
	}, 15*time.Second)
	d.keys("/ERROR")
	d.key(tcell.KeyEnter)
	d.waitText("simulated failure")
	d.dump("04-logs-filtered")
	if strings.Contains(d.text(), "INFO working") {
		t.Fatal("log filter not applied")
	}
	d.keys("f")
	d.waitText("follow:off")
	d.key(tcell.KeyEscape)
	d.key(tcell.KeyEscape)
	d.waitText("Containers[")

	// describe yaml/json
	d.keys("d")
	d.waitText("Config:")
	d.dump("05-describe-yaml")
	d.keys("y")
	d.waitText(`"Config":`)
	d.key(tcell.KeyEscape)

	// stop / start / restart
	d.keys("T")
	d.waitText("exited(")
	d.keys("S")
	d.waitFor("worker running", func(s string) bool { return strings.Contains(s, "running") && !strings.Contains(s, "exited(") }, 30*time.Second)
	d.keys("R")
	d.waitText("✓ restart d9s-e2e-worker-1")

	// ports dialog of web
	d.key(tcell.KeyEscape)
	d.filter("d9s-e2e-web")
	d.requireSelected("d9s-e2e-web-1")
	d.keys("p")
	d.waitText("localhost:18089")
	d.dump("06-ports")
	d.key(tcell.KeyEnter)

	// delete a throwaway container
	d.key(tcell.KeyEscape)
	d.filter("d9s-e2e-throwaway")
	d.requireSelected("d9s-e2e-throwaway")
	d.key(tcell.KeyCtrlD)
	d.waitText("Delete container")
	d.dump("07-confirm")
	d.confirmDanger()
	d.waitText("✓ delete d9s-e2e-throwaway")
	d.waitFor("throwaway gone", func(s string) bool { return strings.Contains(s, "Containers[0]") }, 20*time.Second)
	d.key(tcell.KeyEscape)

	// compose projects → services; service actions
	d.cmd("compose")
	d.waitText("Compose[")
	d.filter("d9s-e2e")
	d.requireSelected("d9s-e2e")
	d.dump("08-compose")
	d.key(tcell.KeyEnter)
	d.waitText("Services(project=d9s-e2e)")
	d.waitText("not created")
	d.dump("09-services")
	d.filter("idle")
	d.requireSelected("idle")
	d.keys("S")
	d.waitText("✓ compose up -d idle")
	d.waitFor("idle running", func(s string) bool { return strings.Contains(s, "1/1") }, 30*time.Second)
	d.keys("R")
	d.waitText("✓ compose restart idle")
	d.keys("T")
	d.waitText("✓ compose stop idle")
	d.key(tcell.KeyCtrlD)
	d.waitText("Stop and remove the containers of service")
	d.confirmDanger()
	d.waitText("✓ compose rm --stop --force idle")
	d.waitText("not created")
	d.key(tcell.KeyEscape)
	d.key(tcell.KeyEscape)
	d.waitText("Compose[")
	d.requireSelected("d9s-e2e")
	d.keys("l")
	d.waitText("worker-1  | tick")
	d.dump("10-compose-logs")
	d.key(tcell.KeyEscape)

	// images / volumes / networks
	d.cmd("images")
	d.waitText("Images[")
	d.filter("alpine")
	d.waitText("alpine")
	d.dump("11-images")
	d.keys("d")
	d.waitText("RepoTags:")
	d.key(tcell.KeyEscape)

	d.cmd("vol")
	d.waitText("Volumes[")
	d.filter("d9s-e2e")
	d.waitText("d9s-e2e_data")
	d.dump("12-volumes")
	d.filter("d9s-e2e-tmpvol")
	d.requireSelected("d9s-e2e-tmpvol")
	d.key(tcell.KeyCtrlD)
	d.waitText("Delete volume")
	d.confirmDanger()
	d.waitText("✓ delete volume d9s-e2e-tmpvol")
	d.waitText("Volumes[0]")
	d.filter("d9s-e2e_data")
	d.key(tcell.KeyEnter)
	d.waitText("Containers(volume=d9s-e2e_data)")
	d.waitText("d9s-e2e-web-1")
	d.key(tcell.KeyEscape)

	d.cmd("net")
	d.waitText("Networks[")
	d.filter("d9s-e2e")
	d.waitText("d9s-e2e_default")
	d.dump("13-networks")
	d.filter("d9s-e2e-tmpnet")
	d.requireSelected("d9s-e2e-tmpnet")
	d.key(tcell.KeyCtrlD)
	d.confirmDanger()
	d.waitText("✓ delete network d9s-e2e-tmpnet")

	// help overlay
	d.keys("?")
	d.waitText("LOGS VIEW")
	d.dump("14-help")
	d.key(tcell.KeyEscape)

	// compose down of the throwaway project
	d.cmd("compose")
	d.filter("d9s-e2e")
	d.requireSelected("d9s-e2e")
	d.key(tcell.KeyCtrlD)
	d.waitText("docker compose down")
	d.confirmDanger()
	d.waitText("✓ compose down (d9s-e2e)")
	d.waitText("Compose[0]")

	d.cmd("q")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("app did not quit")
	}
	if out, _ := exec.Command("docker", "ps", "-a", "-q", "--filter", "label=com.docker.compose.project=d9s-e2e").Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Fatal("compose down left containers behind")
	}
}
