package e2e_tests

import (
	"os"
	"path"
	"strings"
	"testing"

	f "github.com/foundriesio/composeapp/test/fixtures"
)

func TestBuild(t *testing.T) {
	appComposeDef := `
services:
  built:
    build:
      context: ./ctx
    command: sh -c "while true; do sleep 60; done"
  built-named:
    image: hub.example.com/factory/named:latest
    build:
      context: ./ctx
    command: sh -c "while true; do sleep 60; done"
  remote:
    image: registry:5000/factory/runner-image:v0.1
    command: sh -c "while true; do sleep 60; done"
`
	app := f.NewApp(t, appComposeDef)
	ctxDir := path.Join(app.Dir, "ctx")
	f.Check(t, os.MkdirAll(ctxDir, 0o755))
	// no RUN instructions, so building for a foreign platform needs no emulation
	f.Check(t, os.WriteFile(path.Join(ctxDir, "Dockerfile"),
		[]byte("FROM ghcr.io/foundriesio/busybox:1.36-multiarch\nCOPY hello.txt /hello.txt\n"), 0o644))
	f.Check(t, os.WriteFile(path.Join(ctxDir, "hello.txt"), []byte("hello"), 0o644))

	buildStore := t.TempDir()
	app.Build(t, buildStore, "amd64,arm64")
	if !strings.HasPrefix(app.PublishedUri, "hub.example.com/factory/") {
		t.Fatalf("unexpected app uri: %s", app.PublishedUri)
	}
	app.CheckFetchedIn(t, buildStore, "amd64")
	app.CheckFetchedIn(t, buildStore, "arm64")

	// the built store is all a device needs, no registry is involved
	app.PullFromStore(t, buildStore)
	defer app.Remove(t)
	app.CheckFetched(t)

	app.Install(t)
	defer app.Uninstall(t)
	app.CheckInstalled(t)

	// the stock dockerd cannot find a loaded image by its digest, so `compose up` pulls it,
	// which works for published apps but not for built ones that have no registry behind them
	if os.Getenv("DOCKERD_FIO_PATCH") != "1" {
		t.Skip("skipping running the built app that requires the Foundries patches to dockerd; set DOCKERD_FIO_PATCH=1 to run it")
	}
	app.Run(t)
	defer app.Stop(t)
	app.CheckRunning(t)
}
