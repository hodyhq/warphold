package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLogoutRevokesServerSide pins the point of server-side sessions: the
// cookie is a lookup key, so a copy taken before logout is worthless after.
func TestLogoutRevokesServerSide(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	stolen := h.jar

	resp, _ := h.do("DELETE", "/api/v1/fleet/session", nil)
	require.Equal(t, 204, resp.StatusCode)

	h.jar = stolen
	resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 401, resp.StatusCode, "the pre-logout cookie is dead")

	// Replaying the logout with the revoked cookie changes nothing.
	resp, _ = h.do("DELETE", "/api/v1/fleet/session", nil)
	require.Equal(t, 204, resp.StatusCode)
	h.jar = stolen
	resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 401, resp.StatusCode)
}

func TestAdminsListCreateAndDelete(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	resp, list := h.doList("GET", "/api/v1/fleet/admins")
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, list, 1)
	require.Equal(t, "hody@hody.dev", list[0]["email"])
	require.Equal(t, "owner", list[0]["role"])
	require.NotContains(t, list[0], "pw_hash", "no password material in responses")

	resp, _ = h.do("POST", "/api/v1/fleet/admins", map[string]string{"email": "nope", "password": "pw12345678"})
	require.Equal(t, 400, resp.StatusCode, "email must look like an email")
	resp, _ = h.do("POST", "/api/v1/fleet/admins", map[string]string{"email": "b@hody.dev", "password": "short"})
	require.Equal(t, 400, resp.StatusCode, "password needs 8+ characters")

	resp, body := h.do("POST", "/api/v1/fleet/admins", map[string]string{"email": "b@hody.dev", "password": "pw12345678"})
	require.Equal(t, 201, resp.StatusCode)
	require.NotZero(t, body["id"])

	resp, _ = h.do("POST", "/api/v1/fleet/admins", map[string]string{"email": "b@hody.dev", "password": "pw12345678"})
	require.Equal(t, 409, resp.StatusCode, "duplicate email")

	resp, list = h.doList("GET", "/api/v1/fleet/admins")
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, list, 2)

	resp, _ = h.do("DELETE", "/api/v1/fleet/admins/9999", nil)
	require.Equal(t, 404, resp.StatusCode)
}

// TestDeleteAdminRevokesSessions covers both halves of the delete contract:
// the deleted admin's live cookie stops working on its very next request, and
// the last remaining admin cannot be removed - the self-delete guard catches
// it first, since the last admin left is always the one making the call.
func TestDeleteAdminRevokesSessions(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	owner := h.jar

	resp, body := h.do("POST", "/api/v1/fleet/admins", map[string]string{"email": "b@hody.dev", "password": "pw12345678"})
	require.Equal(t, 201, resp.StatusCode)
	bID := jsonNum(body["id"].(float64))

	h.jar = h.login("b@hody.dev", "pw12345678")
	resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 200, resp.StatusCode, "the second admin can use the API")
	second := h.jar

	h.jar = owner
	resp, _ = h.do("DELETE", "/api/v1/fleet/admins/"+bID, nil)
	require.Equal(t, 204, resp.StatusCode)

	h.jar = second
	resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 401, resp.StatusCode, "a deleted admin's session dies immediately")

	h.jar = owner
	_, ownerList := h.doList("GET", "/api/v1/fleet/admins")
	require.Len(t, ownerList, 1)
	resp, body = h.do("DELETE", "/api/v1/fleet/admins/"+jsonNum(ownerList[0]["id"].(float64)), nil)
	require.Equal(t, 409, resp.StatusCode, "the last admin stays")
	require.Equal(t, "cannot delete your own account", body["error"])
}

// TestAdminEmailsAreCaseInsensitive pins one address to one account: a mixed
// case address creates the same admin a lowercase one would, is rejected as a
// duplicate of it, and signs in under either spelling.
func TestAdminEmailsAreCaseInsensitive(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	resp, _ := h.do("POST", "/api/v1/fleet/admins", map[string]string{"email": "  Hody@Example.COM ", "password": "pw12345678"})
	require.Equal(t, 201, resp.StatusCode)

	_, list := h.doList("GET", "/api/v1/fleet/admins")
	require.Len(t, list, 2)
	require.Equal(t, "hody@example.com", list[1]["email"], "stored lowercase and trimmed")

	resp, _ = h.do("POST", "/api/v1/fleet/admins", map[string]string{"email": "hody@example.com", "password": "pw12345678"})
	require.Equal(t, 409, resp.StatusCode, "same admin, different casing")

	// Either spelling logs in, because the lookup normalizes too.
	for _, email := range []string{"hody@example.com", "HODY@Example.com"} {
		h.jar = h.login(email, "pw12345678")
		resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
		require.Equal(t, 200, resp.StatusCode, email)
	}
}

// TestCannotDeleteOwnAccount covers the guard that is not about the last
// admin: even with somebody else left to sign in, deleting the row you are
// signed in as is refused.
func TestCannotDeleteOwnAccount(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	resp, _ := h.do("POST", "/api/v1/fleet/admins", map[string]string{"email": "b@hody.dev", "password": "pw12345678"})
	require.Equal(t, 201, resp.StatusCode)

	_, list := h.doList("GET", "/api/v1/fleet/admins")
	require.Len(t, list, 2)

	resp, body := h.do("DELETE", "/api/v1/fleet/admins/"+jsonNum(list[0]["id"].(float64)), nil)
	require.Equal(t, 409, resp.StatusCode)
	require.Equal(t, "cannot delete your own account", body["error"])

	_, list = h.doList("GET", "/api/v1/fleet/admins")
	require.Len(t, list, 2, "nothing was deleted")
}

// TestSessionExpiresAfterTTL pins the 12 h session TTL at the API boundary:
// the cookie is unchanged, but the server refuses it once its row is stale.
func TestSessionExpiresAfterTTL(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	resp, _ := h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 200, resp.StatusCode)

	now := time.Now().Add(12*time.Hour + time.Minute)
	h.s.SetNowForTesting(func() time.Time { return now })

	resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 401, resp.StatusCode, "the session died with its TTL")
}

// TestCSRFRequiredOnMutations pins double-submit: cookies alone (what a
// cross-site form can send) never authorize a state change.
func TestCSRFRequiredOnMutations(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	// Cookie-only POST: exactly what a cross-site form gets to send.
	req := h.newRequest("POST", "/api/v1/fleet/targets", jsonBody(map[string]any{"name": "local", "kind": "filesystem", "path": t.TempDir()}))
	req.Header.Del("X-WarpHold-CSRF")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 403, resp.StatusCode, "no CSRF header")

	req = h.newRequest("POST", "/api/v1/fleet/targets", jsonBody(map[string]any{"name": "local", "kind": "filesystem", "path": t.TempDir()}))
	req.Header.Set("X-WarpHold-CSRF", "not-the-cookie")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 403, resp.StatusCode, "header must equal the cookie")

	// Reads are exempt.
	req = h.newRequest("GET", "/api/v1/fleet/targets", nil)
	req.Header.Del("X-WarpHold-CSRF")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	// With the header the same POST succeeds.
	resp, _ = h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "local", "kind": "filesystem", "path": t.TempDir()})
	require.Equal(t, 201, resp.StatusCode)

	require.NotEmpty(t, h.cookie("wh_csrf"), "the CSRF cookie is readable by the UI")
	for _, c := range h.jar {
		if c.Name == "wh_csrf" {
			require.False(t, c.HttpOnly, "the UI has to read it to echo it back")
			require.Equal(t, http.SameSiteStrictMode, c.SameSite)
		}
		if c.Name == "wh_session" {
			require.True(t, c.HttpOnly)
		}
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	first := h.jar

	h.jar = h.login("hody@hody.dev", "pw12345678")
	second := h.jar

	resp, _ := h.do("POST", "/api/v1/fleet/admins/me/password", map[string]string{"current": "wrong", "new": "newpw12345"})
	require.Equal(t, 401, resp.StatusCode)
	resp, _ = h.do("POST", "/api/v1/fleet/admins/me/password", map[string]string{"current": "pw12345678", "new": "short"})
	require.Equal(t, 400, resp.StatusCode)

	resp, _ = h.do("POST", "/api/v1/fleet/admins/me/password", map[string]string{"current": "pw12345678", "new": "newpw12345"})
	require.Equal(t, 204, resp.StatusCode)

	h.jar = second
	resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 200, resp.StatusCode, "the session that changed the password survives")

	h.jar = first
	resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 401, resp.StatusCode, "every other session is revoked")

	h.jar = nil
	resp, _ = h.do("POST", "/api/v1/fleet/session", map[string]string{"email": "hody@hody.dev", "password": "pw12345678"})
	require.Equal(t, 401, resp.StatusCode, "old password is gone")
	h.jar = h.login("hody@hody.dev", "newpw12345")
	resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 200, resp.StatusCode)
}
