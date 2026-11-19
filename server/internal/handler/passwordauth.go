// Added by the mouse9527/multica fork: password login (M-41). The policy
// lives in internal/passwordauth; this file is the HTTP surface.

package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/passwordauth"
)

// Error codes of the password endpoints, for the UI to translate.
const (
	passwordCodeInvalidCredentials = "invalid_credentials"
	passwordCodeThrottled          = "too_many_attempts"
	passwordCodeCurrentRequired    = "current_password_required"
	passwordCodeCurrentIncorrect   = "current_password_incorrect"
	passwordCodeInvalidPassword    = "invalid_password"
)

// maxPasswordRequestBytes bounds the request body: two passwords of at most
// passwordauth.MaxPasswordBytes plus JSON.
const maxPasswordRequestBytes = 8 << 10

type PasswordLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type SetPasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type PasswordStatusResponse struct {
	HasPassword bool `json:"has_password"`
}

// MountPasswordAuth registers POST /auth/password-login and
// GET/POST /api/me/password when MULTICA_PASSWORD_AUTH_ENABLED is on, and
// nothing otherwise. requireAuth is the session middleware of the
// authenticated routes. Invalid argon2id settings (including parameters that
// do not fit the hashing memory budget) leave the feature off rather than
// store weak hashes or overrun memory.
func (h *Handler) MountPasswordAuth(r chi.Router, requireAuth func(http.Handler) http.Handler, trustedProxies []*net.IPNet) {
	if !passwordauth.Enabled(os.Getenv) {
		return
	}
	hasher, err := passwordauth.HasherFromEnv(os.Getenv)
	if err != nil {
		slog.Error("password login disabled: invalid argon2id settings", "error", err)
		return
	}
	h.mountPasswordAuth(r, passwordauth.NewService(passwordauth.NewStore(h.DB, hasher, nil), nil), requireAuth, trustedProxies)
}

func (h *Handler) mountPasswordAuth(r chi.Router, svc *passwordauth.Service, requireAuth func(http.Handler) http.Handler, trustedProxies []*net.IPNet) {
	h.PasswordAuth = svc
	r.Post("/auth/password-login", func(w http.ResponseWriter, r *http.Request) {
		h.PasswordLogin(w, r, middleware.ClientIP(r, trustedProxies))
	})
	r.With(requireAuth, requirePasswordSession).Route("/api/me/password", func(r chi.Router) {
		r.Get("/", h.GetMyPasswordStatus)
		r.Post("/", h.SetMyPassword)
	})
}

// requirePasswordSession limits password management to a person's own login
// session, judged by the credential Auth actually used rather than by where
// it was sent: a personal access token placed in the auth cookie is still a
// personal access token. Agent task tokens and cloud tokens act for machines;
// a personal access token is a long-lived credential handed to scripts, and
// must not be able to turn itself into a password.
func requirePasswordSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMachineCredentialActor(r) || !middleware.AuthenticatedBySession(r) {
			writeError(w, http.StatusForbidden, "sign in to the web app to manage your password")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// PasswordLogin signs in with email and password. It never creates a user.
// Unknown emails, accounts without a password and wrong passwords get the
// same 401; a locked account and the per-IP limit get the same 429.
func (h *Handler) PasswordLogin(w http.ResponseWriter, r *http.Request, ip string) {
	var req PasswordLoginRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxPasswordRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Email) == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	user, err := h.PasswordAuth.Login(r.Context(), req.Email, req.Password, ip)
	switch {
	case errors.Is(err, passwordauth.ErrInvalidCredentials):
		writeErrorCode(w, http.StatusUnauthorized, passwordCodeInvalidCredentials, "invalid email or password")
		return
	case errors.Is(err, passwordauth.ErrThrottled):
		writeErrorCode(w, http.StatusTooManyRequests, passwordCodeThrottled, "too many attempts, try again later")
		return
	case err != nil:
		slog.Warn("password login failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}

	tokenString, err := h.issueJWT(user)
	if err != nil {
		if errors.Is(err, auth.ErrTemporarilyDisabledUser) {
			writeError(w, http.StatusForbidden, auth.TemporarilyDisabledUserError)
			return
		}
		slog.Warn("password login failed", append(logger.RequestAttrs(r), "error", err, "user_id", uuidToString(user.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	if err := auth.SetAuthCookies(w, tokenString); err != nil {
		slog.Warn("failed to set auth cookies", "error", err)
	}
	if h.CFSigner != nil {
		for _, cookie := range h.CFSigner.SignedCookies(time.Now().Add(auth.AuthTokenTTL())) {
			http.SetCookie(w, cookie)
		}
	}

	slog.Info("user logged in with password", append(logger.RequestAttrs(r), "user_id", uuidToString(user.ID))...)
	writeJSON(w, http.StatusOK, LoginResponse{
		Token: tokenString,
		User:  h.userToResponse(user),
	})
}

// GetMyPasswordStatus tells the settings page whether to ask for the current
// password.
func (h *Handler) GetMyPasswordStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	has, err := h.PasswordAuth.HasPassword(r.Context(), parseUUID(userID))
	if err != nil {
		slog.Warn("password status failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to load password status")
		return
	}
	writeJSON(w, http.StatusOK, PasswordStatusResponse{HasPassword: has})
}

// SetMyPassword sets the signed-in user's first password, or replaces it
// given the current one. The user must already exist: the session's user is
// looked up and nothing is ever created.
func (h *Handler) SetMyPassword(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req SetPasswordRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxPasswordRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	user, err := h.Queries.GetUser(r.Context(), parseUUID(userID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "user not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load user")
		return
	}

	err = h.PasswordAuth.SetPassword(r.Context(), user.ID, req.CurrentPassword, req.NewPassword)
	switch {
	case errors.Is(err, passwordauth.ErrPasswordTooShort), errors.Is(err, passwordauth.ErrPasswordTooLong):
		writeErrorCode(w, http.StatusBadRequest, passwordCodeInvalidPassword, strings.TrimPrefix(err.Error(), "passwordauth: "))
		return
	case errors.Is(err, passwordauth.ErrCurrentPasswordRequired):
		writeErrorCode(w, http.StatusBadRequest, passwordCodeCurrentRequired, "current password is required")
		return
	case errors.Is(err, passwordauth.ErrWrongCurrentPassword):
		slog.Warn("password change: wrong current password", append(logger.RequestAttrs(r), "user_id", userID)...)
		writeErrorCode(w, http.StatusForbidden, passwordCodeCurrentIncorrect, "current password is incorrect")
		return
	case errors.Is(err, passwordauth.ErrThrottled):
		writeErrorCode(w, http.StatusTooManyRequests, passwordCodeThrottled, "too many attempts, try again later")
		return
	case err != nil:
		slog.Warn("password change failed", append(logger.RequestAttrs(r), "error", err, "user_id", userID)...)
		writeError(w, http.StatusInternalServerError, "failed to set password")
		return
	}
	slog.Info("password set", append(logger.RequestAttrs(r), "user_id", userID)...)
	writeJSON(w, http.StatusOK, PasswordStatusResponse{HasPassword: true})
}
