package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/mail"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
)

// digestFixture is a bare store - agents, reports, repo stats and admins -
// with no real Kopia repository: the digest never opens one, so building one
// per test would only slow it down.
type digestFixture struct {
	st  *store.Store
	key seal.Key
}

func newDigestFixture(t *testing.T) *digestFixture {
	t.Helper()

	st := openTemp(t)
	salt, err := seal.NewSalt()
	require.NoError(t, err)

	return &digestFixture{st: st, key: seal.Derive("a fleet passphrase", salt)}
}

func (f *digestFixture) configureSMTP(t *testing.T) {
	t.Helper()

	require.NoError(t, f.st.SetSetting(context.Background(), "smtp_from", "fleet@example.com"))
}

func (f *digestFixture) addAdmin(t *testing.T, email string) int64 {
	t.Helper()

	id, err := f.st.CreateAdmin(context.Background(), email, "hash", time.Now())
	require.NoError(t, err)

	return id
}

// addAgent creates one agent, with a distinct hostname from its display name
// so a test can prove the digest never leaks the hostname.
func (f *digestFixture) addAgent(t *testing.T, id, name string) store.Agent {
	t.Helper()

	ctx := context.Background()

	tid, err := f.st.CreateTarget(ctx, &store.Target{Name: "disk", Kind: "filesystem", Path: t.TempDir(), CreatedAt: time.Now()})
	require.NoError(t, err)
	tpl, err := f.st.CreateTemplate(ctx, &store.Template{Name: "default", Sources: []string{"~"}, PolicyJSON: []byte(`{}`), CreatedAt: time.Now()})
	require.NoError(t, err)
	gid, err := f.st.CreateGroup(ctx, &store.Group{Name: "g", TargetID: tid, TemplateID: tpl, CreatedAt: time.Now()})
	require.NoError(t, err)

	a := store.Agent{
		ID: id, Name: name, Hostname: "secret-laptop-" + id, OS: "linux", Arch: "amd64", Scope: "user",
		GroupID: gid, BearerHash: []byte("h_" + id), SealedBundle: []byte("s_" + id), EnrolledAt: time.Now(),
	}
	require.NoError(t, f.st.CreateAgent(ctx, &a))

	return a
}

func (f *digestFixture) report(t *testing.T, agentID string, finishedAt time.Time, status string) {
	t.Helper()

	_, err := f.st.AddReport(context.Background(), &store.Report{
		AgentID: agentID, TaskID: agentID + "-" + finishedAt.String(), Kind: "snapshot",
		StartedAt: finishedAt.Add(-time.Minute), FinishedAt: finishedAt, Status: status,
	})
	require.NoError(t, err)
}

// job inserts one finished job row, oldest first is the caller's job: tests
// build a kind's history in chronological order so RecentJobs' newest-first
// order comes out right.
func (f *digestFixture) job(t *testing.T, kind string, at time.Time, status string) {
	t.Helper()

	ctx := context.Background()

	id, err := f.st.EnqueueJob(ctx, &store.Job{Kind: kind, ScheduledFor: at})
	require.NoError(t, err)
	require.NoError(t, f.st.FinishJob(ctx, id, at, status, status))
}

// fakeSender records the call it received and returns err.
type fakeSender struct {
	to                  []string
	subject, text, html string
	called              bool
	err                 error
}

func (f *fakeSender) send(_ context.Context, to []string, subject, text, htmlBody string) error {
	f.called = true
	f.to, f.subject, f.text, f.html = to, subject, text, htmlBody

	return f.err
}

func TestDigestSkipsWhenSMTPNotConfigured(t *testing.T) {
	f := newDigestFixture(t)
	f.addAdmin(t, "owner@example.com")

	sender := &fakeSender{}
	detail, err := Digest(f.st, f.key.Open, sender.send)(context.Background(), store.Job{Kind: "digest"})

	require.ErrorIs(t, err, ErrSkipped)
	require.Equal(t, "smtp not configured", detail)
	require.False(t, sender.called, "no message is sent when SMTP was never set up")
}

func TestDigestSkipsWhenNoAdmin(t *testing.T) {
	f := newDigestFixture(t)
	f.configureSMTP(t)

	sender := &fakeSender{}
	detail, err := Digest(f.st, f.key.Open, sender.send)(context.Background(), store.Job{Kind: "digest"})

	require.ErrorIs(t, err, ErrSkipped)
	require.Equal(t, "no admin to send to", detail)
	require.False(t, sender.called)
}

func TestDigestSendsWithZeroDevicesAndNoDivideByZero(t *testing.T) {
	f := newDigestFixture(t)
	f.configureSMTP(t)
	f.addAdmin(t, "owner@example.com")

	sender := &fakeSender{}
	detail, err := Digest(f.st, f.key.Open, sender.send)(context.Background(), store.Job{Kind: "digest"})

	require.NoError(t, err)
	require.Equal(t, "sent to 1 admin(s)", detail)
	require.True(t, sender.called)
	require.Equal(t, []string{"owner@example.com"}, sender.to)
	require.Equal(t, digestSubject, sender.subject)

	require.Contains(t, sender.text, "none enrolled yet")
	require.NotContains(t, sender.text, "dedup ratio", "no repo stats exist yet")
	require.Contains(t, sender.html, "No devices enrolled yet")
	require.NotContains(t, sender.html, "dedup ratio")
}

func TestDigestRendersDeviceHealthAndOffsiteState(t *testing.T) {
	f := newDigestFixture(t)
	ctx := context.Background()

	f.configureSMTP(t)
	f.addAdmin(t, "owner@example.com")
	f.st.SetSetting(ctx, "public_url", "https://fleet.example.com") //nolint:errcheck
	f.st.SetSetting(ctx, "mirror_interval", "3600")                 //nolint:errcheck

	a := f.addAgent(t, "ag_1", "Hody's Laptop")
	f.report(t, a.ID, time.Now().Add(-2*time.Hour), "ok")
	require.NoError(t, f.st.SetStats(ctx, a.ID, time.Now(), 2000, 1000, 5))
	require.NoError(t, f.st.SetMirrored(ctx, a.ID, time.Now().Add(-30*24*time.Hour), 900))

	sender := &fakeSender{}
	_, err := Digest(f.st, f.key.Open, sender.send)(ctx, store.Job{Kind: "digest"})
	require.NoError(t, err)

	require.Contains(t, sender.text, "Hody's Laptop", "the device's own name is shown")
	require.NotContains(t, sender.text, a.Hostname, "the raw hostname must never leak into the digest")
	require.Contains(t, sender.text, "https://fleet.example.com", "the Fleet's own public URL is the one hostname allowed")
	require.Contains(t, sender.text, "dedup ratio 2.00x")
	require.Contains(t, sender.text, "stale, last offsite", "30 days is well past 3x the 1h mirror interval")
	require.NotContains(t, sender.text, a.ID+"_secret") // sanity: no stray internals

	require.Contains(t, sender.html, "Hody&#39;s Laptop")
	require.NotContains(t, sender.html, a.Hostname)
}

func TestDigestNamesAKindFailingForOverAWeek(t *testing.T) {
	f := newDigestFixture(t)
	ctx := context.Background()

	f.configureSMTP(t)
	f.addAdmin(t, "owner@example.com")

	now := time.Now()
	// Eight days of consecutive mirror failures, oldest first.
	for i := 8; i >= 1; i-- {
		f.job(t, "mirror", now.Add(-time.Duration(i)*24*time.Hour), "error")
	}
	// verify has failed just twice, well under a week.
	f.job(t, "verify", now.Add(-2*24*time.Hour), "error")
	f.job(t, "verify", now.Add(-time.Hour), "error")

	sender := &fakeSender{}
	_, err := Digest(f.st, f.key.Open, sender.send)(ctx, store.Job{Kind: "digest"})
	require.NoError(t, err)

	require.Contains(t, sender.text, "mirror has been failing for over a week")
	require.NotContains(t, sender.text, "verify has been failing")
	require.Contains(t, sender.text, "mirror: error")
	require.Contains(t, sender.text, "verify: error")
}

func TestDigestOneRecentOKRunEndsTheFailingStreak(t *testing.T) {
	f := newDigestFixture(t)
	ctx := context.Background()

	f.configureSMTP(t)
	f.addAdmin(t, "owner@example.com")

	now := time.Now()
	for i := 10; i >= 3; i-- {
		f.job(t, "reap", now.Add(-time.Duration(i)*24*time.Hour), "error")
	}

	f.job(t, "reap", now.Add(-2*24*time.Hour), "ok")
	f.job(t, "reap", now.Add(-time.Hour), "error")

	sender := &fakeSender{}
	_, err := Digest(f.st, f.key.Open, sender.send)(ctx, store.Job{Kind: "digest"})
	require.NoError(t, err)

	require.NotContains(t, sender.text, "reap has been failing", "the ok run two days ago broke the streak")
	require.Contains(t, sender.text, "reap: error")
}

func TestDigestRedactsCredentialsOnSendFailure(t *testing.T) {
	f := newDigestFixture(t)
	f.configureSMTP(t)
	require.NoError(t, f.st.SetSetting(context.Background(), "smtp_username", "svc-user"))

	sealed, err := mail.SealPassword(f.key, "hunter2")
	require.NoError(t, err)
	require.NoError(t, f.st.SetSetting(context.Background(), "sealed_smtp_password", sealed))
	f.addAdmin(t, "owner@example.com")

	sender := &fakeSender{err: errors.New("550 auth failed for svc-user with password hunter2")}
	_, err = Digest(f.st, f.key.Open, sender.send)(context.Background(), store.Job{Kind: "digest"})

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSkipped)
	require.NotContains(t, err.Error(), "hunter2")
	require.NotContains(t, err.Error(), "svc-user")
	require.Contains(t, err.Error(), "[redacted]")
}

// A device whose printed recovery kit nobody has acknowledged holding is the
// one thing in the digest that no job can fix, so the digest says how many
// there are - and stops saying it once the kit is acknowledged.
func TestDigestCountsUnacknowledgedRecoveryKits(t *testing.T) {
	f := newDigestFixture(t)
	ctx := context.Background()

	f.configureSMTP(t)
	adminID := f.addAdmin(t, "owner@example.com")

	a := f.addAgent(t, "ag_1", "Hody's Laptop")

	sender := &fakeSender{}
	_, err := Digest(f.st, f.key.Open, sender.send)(ctx, store.Job{Kind: "digest"})
	require.NoError(t, err)
	require.Contains(t, sender.text, "1 device(s) have no saved recovery kit")
	require.Contains(t, sender.html, "no saved recovery kit")

	require.NoError(t, f.st.SetKitAck(ctx, a.ID, adminID, time.Now()))

	sender = &fakeSender{}
	_, err = Digest(f.st, f.key.Open, sender.send)(ctx, store.Job{Kind: "digest"})
	require.NoError(t, err)
	require.NotContains(t, sender.text, "no saved recovery kit")
	require.NotContains(t, sender.html, "no saved recovery kit")
}
