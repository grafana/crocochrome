package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/crocochrome"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestIntegration performs integration tests by spinning up a production-ish container and try to run k6 against it,
// implementing a client to crocochrome and using the official k6 image to run the test.
func TestIntegration(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skipf("Skipping integration test due to -short")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	image, err := buildImage("..", "crocochrome")
	if err != nil {
		t.Fatalf("building crocochrome container: %v", err)
	}

	network, err := network.New(ctx, network.WithAttachable()) // Attachable allows port mapping.
	if err != nil {
		t.Fatalf("creating container network: %v", err)
	}
	// TODO: Removing the network fails due to port forwards still in effect. Figure out the proper way to do it.
	// For now , testcontainer's reaper (ryuk) seems to take care of removing it after the test ends.

	ccName := "crocochrome-" + randomHexString(6)
	cc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Name:         ccName,
			Image:        image,
			ExposedPorts: []string{"8080/tcp"},
			WaitingFor:   wait.ForExposedPort(),
			Networks:     []string{network.Name},
			// Since https://github.com/grafana/crocochrome/pull/12, crocochrome requires /chromium-tmp to exist
			// and be writable.
			Mounts: testcontainers.Mounts(testcontainers.VolumeMount("chromium-tmp", "/chromium-tmp")),
		},
	})
	testcontainers.CleanupContainer(t, cc)
	if err != nil {
		t.Fatalf("starting crocochrome container: %v", err)
	}

	endpoint, err := cc.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		t.Fatalf("getting crocochrome endpoint: %v", err)
	}

	// Create a k6 container to run the scripts from.
	k6, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			// Renovate updates the version below. Keep its format as it is or update the renovate config with it.
			Image:      "grafana/k6:0.59.0",
			Entrypoint: []string{"/bin/sleep", "infinity"},
			Networks:   []string{network.Name},
		},
	})
	testcontainers.CleanupContainer(t, cc)
	if err != nil {
		t.Fatalf("starting k6 container: %v", err)
	}

	for _, tc := range []struct {
		name   string
		script string
	}{
		{
			name:   "simple browser test",
			script: scriptk6io,
		},
	} {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Crocochrome can only run one session at a time. Do not run in parallel.
			session, err := createSession(endpoint)
			if err != nil {
				t.Fatalf("creating session: %v", err)
			}

			t.Cleanup(func() {
				err := deleteSession(endpoint, session.ID)
				if err != nil {
					t.Fatalf("deleting session: %v", err)
				}
			})

			// TODO: Would be NEAT if we could just use stdin. However testcontainers does not seem to support that.
			scriptPath := filepath.Join("/", "home", "k6", strings.ReplaceAll(t.Name(), "/", "_")+".js")
			err = k6.CopyToContainer(ctx, []byte(tc.script), scriptPath, 0o644)
			if err != nil {
				t.Fatalf("copying k6 script to container: %v", err)
			}

			rc, stdouterr, err := k6.Exec(
				ctx,
				[]string{"k6", "run", scriptPath},
				exec.Multiplexed(),
				exec.WithEnv([]string{"K6_BROWSER_WS_URL=ws://" + ccName + ":8080/proxy/" + session.ID}),
			)
			if err != nil {
				t.Fatalf("running k6 script: %v", err)
			}

			output, _ := io.ReadAll(stdouterr)
			t.Logf("k6 output:\n%s", string(output))

			// Usual k6 hack: k6 will exit with 0 even if fatal errors occur.
			if bytes.Contains(output, []byte("error")) {
				t.Fatalf("k6 output contains the string \"error\"")
			}

			if rc != 0 {
				t.Fatalf("unexpected k6 return code %d", rc)
			}
		})
	}
}

// createSession calls the crocochrome API to create a session, starting a chromium process and retrieving its WS URL.
func createSession(endpoint string) (*crocochrome.SessionInfo, error) {
	resp, err := http.Post(endpoint+"/sessions", "application/json", nil)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("got unexpected status %d", resp.StatusCode)
	}

	session := crocochrome.SessionInfo{}
	err = json.NewDecoder(resp.Body).Decode(&session)
	if err != nil {
		return nil, err
	}

	return &session, nil
}

// deleteSession calls the crocochrome API to delete a session.
func deleteSession(endpoint, sessionID string) error {
	req, err := http.NewRequest(http.MethodDelete, endpoint+"/sessions/"+sessionID, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("got unexpected status %d", resp.StatusCode)
	}

	return nil
}

// From https://grafana.com/docs/k6/latest/using-k6-browser/#a-simple-browser-test
var scriptk6io string = `
import { browser } from 'k6/browser';
import { check } from 'https://jslib.k6.io/k6-utils/1.5.0/index.js';

export const options = {
  scenarios: {
    ui: {
      executor: 'shared-iterations',
      options: {
        browser: {
          type: 'chromium',
        },
      },
    },
  },
  thresholds: {
    checks: ['rate==1.0'],
  },
};

export default async function () {
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    await page.goto('https://test.k6.io/my_messages.php');

    await page.locator('input[name="login"]').type('admin');
    await page.locator('input[name="password"]').type('123');

    await Promise.all([page.waitForNavigation(), page.locator('input[type="submit"]').click()]);

    await check(page.locator('h2'), {
      header: async (h2) => (await h2.textContent()) == 'Welcome, admin!',
    });
  } finally {
    await page.close();
  }
}
`
