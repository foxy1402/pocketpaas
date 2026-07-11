package auth

import (
	"crypto/subtle"
	"net/http"
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
		if requiresCSRF(r.Method) && !validCSRF(r) {
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

func requiresCSRF(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	default:
		return true
	}
}

func validCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	submitted := r.Header.Get("X-CSRF-Token")
	if submitted == "" {
		submitted = r.FormValue("_csrf")
	}
	if submitted == "" || len(submitted) != len(cookie.Value) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(submitted), []byte(cookie.Value)) == 1
}
