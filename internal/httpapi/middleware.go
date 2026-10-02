package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/ansonarose/seeyouthere-backend/internal/auth"
)

type ctxKey int

const (
	ctxClientIP ctxKey = iota
	ctxUserID
	ctxSessionID
	ctxAdminRoles
)

const sessionCookie = "syt_session"

// clientIP resolves the caller's address. Behind Caddy, the rightmost X-Forwarded-For
// entry is the one Caddy itself saw; anything to its left is client-supplied and untrusted.
func (s *Server) clientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := remoteIP(r.RemoteAddr)
		if s.cfg.TrustProxy {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				parts := strings.Split(xff, ",")
				if addr, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
					ip = addr.Unmap()
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxClientIP, ip)))
	})
}

func remoteIP(remoteAddr string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, _ := netip.ParseAddr(host)
	return addr.Unmap()
}

func clientIPFrom(ctx context.Context) netip.Addr {
	ip, _ := ctx.Value(ctxClientIP).(netip.Addr)
	return ip
}

// ipRateKey renders ip for use in an s.allow rate-limit key. IPv4 addresses
// are used as-is; IPv6 addresses are collapsed to their /64, since an
// attacker with an IPv6 allocation can otherwise cycle through a huge
// number of distinct addresses to dodge a per-IP limit. Every limit keyed
// on the client IP should go through this helper.
func ipRateKey(ip netip.Addr) string {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Is4() {
		return ip.String()
	}
	return netip.PrefixFrom(ip, 64).Masked().Addr().String()
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}

// cors lets the web app's origins call the API with cookies.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		w.Header().Add("Vary", "Origin")
		if origin == "" || !slices.Contains(s.cfg.AppOrigins, origin) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
			h.Set("Access-Control-Allow-Headers", "Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrf rejects cross-origin state-changing browser requests, except from the web app's origins.
func (s *Server) csrf(next http.Handler) http.Handler {
	p := http.NewCrossOriginProtection()
	for _, o := range s.cfg.AppOrigins {
		if err := p.AddTrustedOrigin(o); err != nil {
			panic(err) // origins are validated at startup via config
		}
	}
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "cross_origin", "Cross-origin request blocked.")
	}))
	return p.Handler(next)
}

func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		sess, err := s.sessions.Lookup(r.Context(), c.Value)
		switch {
		case errors.Is(err, auth.ErrNoSession):
			s.clearSessionCookie(w)
			next.ServeHTTP(w, r)
		case err != nil:
			serverError(w, r, err)
		default:
			ctx := context.WithValue(r.Context(), ctxUserID, sess.UserID)
			ctx = context.WithValue(ctx, ctxSessionID, sess.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		}
	})
}

func userIDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxUserID).(uuid.UUID)
	return id, ok
}

func sessionIDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxSessionID).(uuid.UUID)
	return id, ok
}

func requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := userIDFrom(r.Context()); !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Log in to continue.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Domain:   s.cfg.CookieDomain,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// secureHeaders adds headers every /v1 JSON response should have. Handlers
// that need a different Cache-Control (the catalog) set it after this
// middleware runs, which simply overwrites the default.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Path:     "/",
		Domain:   s.cfg.CookieDomain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}
