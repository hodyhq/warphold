package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
)

// commandAgentEnroll enrolls this machine into a Fleet using a one-time
// enrollment token: it exchanges the token for a per-agent repository and
// bearer token, connects to that repository, and persists the agent's
// enrollment state.
type commandAgentEnroll struct {
	server string
	token  string
	scope  string
	name   string
	svc    advancedAppServices
	out    textOutput
}

func (c *commandAgentEnroll) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("enroll", "Enroll this machine into a Fleet using an enrollment token.")
	cmd.Flag("server", "Fleet server URL, e.g. https://fleet.example").Required().StringVar(&c.server)
	cmd.Flag("token", "Enrollment token").Required().StringVar(&c.token)
	cmd.Flag("scope", "user (backs up $HOME) or system (root, backs up system paths)").Default("user").EnumVar(&c.scope, "user", "system")
	cmd.Flag("name", "Display name (defaults to hostname)").StringVar(&c.name)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

// enrollResponse is the body of POST {server}/api/v1/fleet/enroll.
type enrollResponse struct {
	AgentID      string `json:"agent_id"`
	Bearer       string `json:"bearer"`
	Name         string `json:"name"`
	ConnectToken string `json:"connect_token"`
	PollInterval int    `json:"poll_interval_seconds"`
}

// enrollHTTPTimeout bounds the enroll HTTP call. Provisioning a fresh
// repository (e.g. creating B2 keys and initializing the format blob) can
// take a while, but it must not hang forever.
const enrollHTTPTimeout = 5 * time.Minute

func (c *commandAgentEnroll) run(ctx context.Context) error {
	er, err := c.callEnroll(ctx)
	if err != nil {
		return err
	}

	ci, password, err := repo.DecodeToken(er.ConnectToken)
	if err != nil {
		return errors.Wrap(err, "malformed connect token")
	}

	st, err := blob.NewStorage(ctx, ci, false)
	if err != nil {
		return errors.Wrap(err, "cannot open repository storage")
	}
	defer st.Close(ctx) //nolint:errcheck

	if err := os.MkdirAll(state.CacheDir(c.scope), 0o700); err != nil {
		return errors.Wrap(err, "cannot create cache directory")
	}

	configFile := state.RepoConfigPath(c.scope)
	connectErr := repo.Connect(ctx, configFile, st, password, &repo.ConnectOptions{
		CachingOptions: content.CachingOptions{CacheDirectory: state.CacheDir(c.scope)},
	})
	if err := passwordpersist.OnSuccess(ctx, connectErr, c.svc.passwordPersistenceStrategy(), configFile, password); err != nil {
		return errors.Wrap(err, "cannot connect to repository")
	}

	name := c.name
	if name == "" {
		name = er.Name
	}

	if err := state.Save(c.scope, &state.Config{
		Server:       strings.TrimRight(c.server, "/"),
		AgentID:      er.AgentID,
		Bearer:       er.Bearer,
		Name:         name,
		PollInterval: er.PollInterval,
		Scope:        c.scope,
	}); err != nil {
		return errors.Wrap(err, "cannot save agent state")
	}

	c.out.printStdout("Enrolled as %s (%s).\n", name, er.AgentID)

	return nil
}

// callEnroll exchanges the enrollment token for the agent's credentials and
// its one-time repository connect token.
func (c *commandAgentEnroll) callEnroll(ctx context.Context) (*enrollResponse, error) {
	host, _ := os.Hostname()

	body, err := json.Marshal(map[string]string{
		"token": c.token, "hostname": host, "os": runtime.GOOS, "arch": runtime.GOARCH,
		"version": repo.BuildVersion, "scope": c.scope,
	})
	if err != nil {
		return nil, errors.Wrap(err, "cannot build enrollment request")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.server, "/")+"/api/v1/fleet/enroll", bytes.NewReader(body))
	if err != nil {
		return nil, errors.Wrap(err, "cannot build enrollment request")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: enrollHTTPTimeout}).Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "cannot reach the Fleet server")
	}
	defer resp.Body.Close() //nolint:errcheck

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.Wrap(err, "cannot read enrollment response")
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, errors.Errorf("enrollment refused (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var er enrollResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, errors.Wrap(err, "malformed enrollment response")
	}

	return &er, nil
}
