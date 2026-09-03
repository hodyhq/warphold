package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
)

// Stats returns the runner for the "stats" job: measure one (or every)
// agent's repository size and record it in repo_stats, so the dashboard's
// Stored tile and dedup ratio need not open a repository on every page load
// (spec §7.2, daily). It never writes: the repository is opened read-only,
// like verify.
func Stats(st *store.Store, k seal.Key, cloud CloudStoreFn) Runner {
	return perAgent(st, k, cloud, "measured", true, func(ctx context.Context, rep repo.Repository, a store.Agent) error {
		dr, ok := rep.(repo.DirectRepository)
		if !ok {
			return errors.New("stats needs a direct repository connection")
		}

		var stored, blobs int64
		if err := dr.BlobReader().ListBlobs(ctx, "", func(bm blob.Metadata) error {
			stored += bm.Length
			blobs++

			return nil
		}); err != nil {
			return fmt.Errorf("listing blobs: %w", err)
		}

		// logical_bytes is the uncompressed, undeduplicated size of the data
		// behind every content still in the repository - what stored_bytes
		// would be without the dedup and compression that shrank it onto
		// disk. Together with stored_bytes they are the dedup ratio the
		// overview reports.
		var logical int64
		if err := dr.ContentReader().IterateContents(ctx, content.IterateOptions{}, func(ci content.Info) error {
			logical += int64(ci.OriginalLength)

			return nil
		}); err != nil {
			return fmt.Errorf("iterating contents: %w", err)
		}

		return st.SetStats(ctx, a.ID, time.Now(), logical, stored, blobs)
	})
}
