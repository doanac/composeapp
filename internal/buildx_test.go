package internal

import (
	"reflect"
	"testing"
)

func TestArchToPlatform(t *testing.T) {
	for arch, expected := range map[string]string{
		"amd64":        "linux/amd64",
		"arm":          "linux/arm/v7",
		"linux/arm64":  "linux/arm64",
		"linux/arm/v6": "linux/arm/v6",
	} {
		p, err := ArchToPlatform(arch)
		if err != nil {
			t.Fatalf("unexpected error for %s: %s", arch, err)
		}
		if p != expected {
			t.Errorf("expected %s for %s, got %s", expected, arch, p)
		}
	}
	for _, arch := range []string{"", "mips", "linux/amd64;rm", "windows/amd64"} {
		if _, err := ArchToPlatform(arch); err == nil {
			t.Errorf("expected error for arch %q", arch)
		}
	}
}

func TestBakeArgs(t *testing.T) {
	args, err := BakeArgs(&BakeOptions{
		ComposeFile: "docker-compose.yml",
		Builder:     "composectl",
		Platforms:   []string{"linux/amd64", "linux/arm64"},
		Outputs:     map[string]string{"web": "/tmp/b/web", "api": "/tmp/b/api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{
		"buildx", "bake", "--file", "docker-compose.yml", "--provenance=false", "--builder", "composectl",
		"--set", "api.platform=linux/amd64,linux/arm64",
		"--set", "api.output=type=oci,tar=false,dest=/tmp/b/api",
		"--set", "web.platform=linux/amd64,linux/arm64",
		"--set", "web.output=type=oci,tar=false,dest=/tmp/b/web",
		"api", "web",
	}
	if !reflect.DeepEqual(args, expected) {
		t.Errorf("unexpected bake args:\n%q\nexpected:\n%q", args, expected)
	}
}

func TestBakeArgsRejectsInvalidInput(t *testing.T) {
	valid := func() *BakeOptions {
		return &BakeOptions{
			ComposeFile: "docker-compose.yml",
			Platforms:   []string{"linux/amd64"},
			Outputs:     map[string]string{"web": "/tmp/b/web"},
		}
	}
	for name, mutate := range map[string]func(o *BakeOptions){
		"flag as compose file": func(o *BakeOptions) { o.ComposeFile = "--allow=fs=/" },
		"builder name":         func(o *BakeOptions) { o.Builder = "x --push" },
		"platform":             func(o *BakeOptions) { o.Platforms = []string{"linux/amd64,linux/mips"} },
		"no platforms":         func(o *BakeOptions) { o.Platforms = nil },
		"service name":         func(o *BakeOptions) { o.Outputs = map[string]string{"web.output=x": "/tmp/b"} },
		"output with comma":    func(o *BakeOptions) { o.Outputs = map[string]string{"web": "/tmp/b,push=true"} },
		"no services":          func(o *BakeOptions) { o.Outputs = nil },
	} {
		opts := valid()
		mutate(opts)
		if _, err := BakeArgs(opts); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
