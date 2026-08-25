package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

const cookieName = "apphive_session"
const csrfCookieName = "apphive_csrf"

// Middleware returns an http.Handler that requires a valid session cookie.
// Unauthenticated requests are redirected to /login.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil || !m.ValidateSession(cookie.Value) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// Slide the cookie lifetime along with the server-side session TTL,
		// otherwise active users are hard-logged-out at the static MaxAge.
		SetCookie(w, cookie.Value, secureRequest(r))
		if requiresCSRF(r.Method) && !m.validCSRF(r, cookie.Value) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetCookie writes the session cookie to the response.
func SetCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((24 * 60 * 60)), // 24 hours
	})
}

// SetCSRFCookie writes the readable CSRF cookie used by same-origin forms/HTMX.
func SetCSRFCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((24 * 60 * 60)), // 24 hours
	})
}

// ClearCookies clears the session and CSRF cookies.
func ClearCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:   cookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:   csrfCookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
}

// TokenFromRequest extracts the session token from the cookie.
func TokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// CSRFCookieValue returns the current CSRF cookie value, or "" if absent.
func CSRFCookieValue(r *http.Request) string {
	c, err := r.Cookie(csrfCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// ValidLoginCSRF checks the double-submit-cookie token on the pre-auth login
// form (which has no session yet, so session-bound tokens cannot apply).
func ValidLoginCSRF(r *http.Request) bool {
	cookie := CSRFCookieValue(r)
	submitted := r.FormValue("_csrf")
	if cookie == "" || submitted == "" || len(submitted) != len(cookie) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(submitted), []byte(cookie)) == 1
}

func requiresCSRF(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	default:
		return true
	}
}

// validCSRF checks the submitted token against the session-bound expected
// token (HMAC of the session token). The readable CSRF cookie exists only to
// let same-origin JS/forms learn the value; it is not what we validate
// against, so a planted cookie alone no longer forges a valid pair.
func (m *Manager) validCSRF(r *http.Request, sessionToken string) bool {
	submitted := r.Header.Get("X-CSRF-Token")
	if submitted == "" {
		submitted = r.FormValue("_csrf")
	}
	expected := m.CSRFForSession(sessionToken)
	if submitted == "" || len(submitted) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(submitted), []byte(expected)) == 1
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
