package enroll_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/store"
)

func TestTokenLifecycle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	defer st.Close()
	ctx := context.Background()
	tk := enroll.NewTokens(st)
	now := time.Now()
	tk.SetNowForTesting(func() time.Time { return now })

	// enrollment_tokens.group_id is a foreign key, so a real group must exist first.
	tid, err := st.CreateTarget(ctx, &store.Target{Name: "local", Kind: "filesystem", Path: t.TempDir()})
	require.NoError(t, err)
	tpl, err := st.CreateTemplate(ctx, &store.Template{Name: "Home default", Sources: []string{"~"}})
	require.NoError(t, err)
	gid, err := st.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl})
	require.NoError(t, err)

	plain, tok, err := tk.Issue(ctx, gid, 0, -1, 7)
	require.NoError(t, err)
	require.True(t, len(plain) > 20 && plain[:3] == "wh_")
	require.Equal(t, 1, tok.MaxUses)
	require.WithinDuration(t, now.Add(enroll.DefaultTTL), tok.ExpiresAt, time.Second)

	got, err := tk.Consume(ctx, plain)
	require.NoError(t, err)
	require.Equal(t, tok.ID, got.ID)
	_, err = tk.Consume(ctx, plain)
	require.ErrorIs(t, err, enroll.ErrTokenInvalid, "single use")

	_, err = tk.Consume(ctx, "wh_nope")
	require.ErrorIs(t, err, enroll.ErrTokenInvalid)

	_, _, err = tk.Issue(ctx, gid, 31*24*time.Hour, 1, 7)
	require.ErrorIs(t, err, enroll.ErrTTLTooLong)

	multi, _, err := tk.Issue(ctx, gid, 2*time.Hour, 0, 7)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_, err = tk.Consume(ctx, multi)
		require.NoError(t, err, "unlimited uses")
	}
	now = now.Add(3 * time.Hour)
	_, err = tk.Consume(ctx, multi)
	require.ErrorIs(t, err, enroll.ErrTokenInvalid, "expired")
}
