package internal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

type (
	// BakeOptions describes a `docker buildx bake` run that builds compose services for
	// one or more platforms and exports each service image as an OCI layout directory.
	BakeOptions struct {
		ComposeFile string
		Builder     string
		Platforms   []string
		// Outputs maps a compose service name to the directory its OCI layout is exported to
		Outputs    map[string]string
		DockerHost string
	}
)

var (
	// compose service names and buildx builder names share the same allowed character set
	nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

	archToPlatform = map[string]string{
		"amd64":   "linux/amd64",
		"arm64":   "linux/arm64",
		"arm":     "linux/arm/v7",
		"armv7":   "linux/arm/v7",
		"armv6":   "linux/arm/v6",
		"386":     "linux/386",
		"ppc64le": "linux/ppc64le",
		"s390x":   "linux/s390x",
		"riscv64": "linux/riscv64",
	}
)

// ArchToPlatform maps an architecture name, as given to `publish` and `build`, to a buildx platform.
func ArchToPlatform(arch string) (string, error) {
	if p, ok := archToPlatform[arch]; ok {
		return p, nil
	}
	for _, p := range archToPlatform {
		if p == arch {
			return p, nil
		}
	}
	return "", fmt.Errorf("unsupported architecture: %q", arch)
}

// BakeArgs validates the options and returns the `docker` CLI arguments that run the bake.
func BakeArgs(opts *BakeOptions) ([]string, error) {
	if err := validateBakeOptions(opts); err != nil {
		return nil, err
	}
	args := []string{"buildx", "bake", "--file", opts.ComposeFile, "--provenance=false"}
	if len(opts.Builder) > 0 {
		args = append(args, "--builder", opts.Builder)
	}
	services := make([]string, 0, len(opts.Outputs))
	for svc := range opts.Outputs {
		services = append(services, svc)
	}
	sort.Strings(services)
	platforms := strings.Join(opts.Platforms, ",")
	for _, svc := range services {
		args = append(args,
			"--set", svc+".platform="+platforms,
			"--set", svc+".output=type=oci,tar=false,dest="+opts.Outputs[svc])
	}
	return append(args, services...), nil
}

func validateBakeOptions(opts *BakeOptions) error {
	if len(opts.ComposeFile) == 0 || strings.HasPrefix(opts.ComposeFile, "-") {
		return fmt.Errorf("invalid compose file path: %q", opts.ComposeFile)
	}
	if len(opts.Builder) > 0 && !nameRe.MatchString(opts.Builder) {
		return fmt.Errorf("invalid buildx builder name: %q", opts.Builder)
	}
	if len(opts.Platforms) == 0 {
		return fmt.Errorf("no platforms to build for")
	}
	for _, p := range opts.Platforms {
		if _, err := ArchToPlatform(p); err != nil {
			return err
		}
	}
	if len(opts.Outputs) == 0 {
		return fmt.Errorf("no services to build")
	}
	for svc, dest := range opts.Outputs {
		if !nameRe.MatchString(svc) {
			return fmt.Errorf("invalid compose service name: %q", svc)
		}
		// the output value is parsed by buildx as CSV, so a comma or quote would inject extra attributes
		if len(dest) == 0 || strings.ContainsAny(dest, ",\"'\n") {
			return fmt.Errorf("invalid output directory for service %s: %q", svc, dest)
		}
	}
	return nil
}

// RunBake builds the compose services with `docker buildx bake`, streaming its output.
func RunBake(ctx context.Context, opts *BakeOptions) error {
	args, err := BakeArgs(opts)
	if err != nil {
		return err
	}
	if err := runDocker(ctx, opts.DockerHost, true, args...); err != nil {
		return fmt.Errorf("failed to build app service images: %w", err)
	}
	return nil
}

// EnsureBuilder creates a buildx builder using the docker-container driver unless one with
// the given name exists already. The docker driver cannot export multi-platform OCI images
// unless dockerd uses the containerd image store, the docker-container driver always can.
func EnsureBuilder(ctx context.Context, name string, dockerHost string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid buildx builder name: %q", name)
	}
	if runDocker(ctx, dockerHost, false, "buildx", "inspect", name) == nil {
		return nil
	}
	fmt.Printf("Creating buildx builder %s...\n", name)
	if err := runDocker(ctx, dockerHost, true, "buildx", "create", "--name", name, "--driver", "docker-container"); err != nil {
		return fmt.Errorf("failed to create buildx builder %s: %w", name, err)
	}
	return nil
}

func runDocker(ctx context.Context, dockerHost string, verbose bool, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = os.Environ()
	if len(dockerHost) > 0 {
		cmd.Env = append(cmd.Env, "DOCKER_HOST="+dockerHost)
	}
	if verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}
