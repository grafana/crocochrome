package integration_test

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"strings"
)

// buildContainer builds the container image for sm-k6-runner by running `docker` commands, generating an image on the
// host with the supplied name. Dockerfile is assumed to be placed in repoRoot.
// If name does not contain a tag, buildContainer will generate a random one instead of defaulting to `latest`. This
// allows buildContainer to be called in parallel tests.
// buildContainer returns the full tag used to build the image, or any error encountered launching the docker command.
//
// buildContainer exists to work around the inability of testcontainers to build images that use Buildkit features, such
// as COPY --chown, multiarch builds, etc.
// buildContainer requires a docker-compatible binary in $PATH capable of building Dockerfiles that use Buildkit.
//
// https://github.com/testcontainers/testcontainers-go/discussions/573
// https://github.com/testcontainers/testcontainers-java/issues/2857
func buildContainer(repoRoot, name string) (string, error) {
	if !strings.Contains(name, ":") {
		name = name + ":" + randomHexString(8)
	}

	dockerArgs := []string{
		"build",
		"-t", name,
		repoRoot,
	}

	cmd := exec.Command("docker", dockerArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return name, cmd.Run()
}

func randomHexString(stringLen uint) string {
	noise := make([]byte, stringLen/2)
	_, err := io.ReadFull(rand.Reader, noise)
	if err != nil {
		panic(err)
	}

	return hex.EncodeToString(noise)
}
