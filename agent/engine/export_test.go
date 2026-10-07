package engine

import (
	"context"
	"time"

	"github.com/kopia/kopia/repo"
)

// SetOpenRepoForTest swaps the agent's repository opener and shrinks the
// retry pacing, restoring both when the returned func runs.
func SetOpenRepoForTest(open func(ctx context.Context, configFile, password string, opts *repo.Options) (repo.Repository, error)) (restore func()) {
	prevOpen, prevFirst, prevMax := openRepo, openRetryFirst, openRetryMax
	openRepo, openRetryFirst, openRetryMax = open, 10*time.Millisecond, 50*time.Millisecond

	return func() { openRepo, openRetryFirst, openRetryMax = prevOpen, prevFirst, prevMax }
}
