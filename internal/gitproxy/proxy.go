package gitproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/hvo/mcp-forj/internal/policy"
	"github.com/hvo/mcp-forj/internal/provider"
)

// gitRoutePrefix roots every proxied git smart-HTTP endpoint.
const gitRoutePrefix = "/git/"

// Git smart-HTTP service names. They appear both as the path segment after
// "<repo>.git/" and (for info/refs) as the "service" query value.
const (
	serviceInfoRefs    = "info/refs"
	serviceUploadPack  = "git-upload-pack"
	serviceReceivePack = "git-receive-pack"
)

// shutdownTimeout bounds the graceful shutdown once the context is cancelled.
const shutdownTimeout = 10 * time.Second

// Upstream transport bounds. git smart-HTTP is interactive and streams large
// packfiles, so there is no whole-request timeout: only dialing, response
// headers and idle keep-alives are bounded, letting an in-flight body stream
// for as long as the transfer keeps moving.
const (
	upstreamDialTimeout           = 10 * time.Second
	upstreamResponseHeaderTimeout = 30 * time.Second
	upstreamIdleConnTimeout       = 90 * time.Second
	upstreamTLSHandshakeTimeout   = 10 * time.Second
	upstreamExpectContinueTimeout = 1 * time.Second
)

// Client-facing server timeouts. Only the header read and idle keep-alives
// are bounded (Slowloris defense, keep-alive cleanup); there is deliberately
// no ReadTimeout or WriteTimeout because git smart-HTTP streams large
// packfiles in both directions and a whole-request deadline would truncate
// healthy transfers.
const (
	serverReadHeaderTimeout = 10 * time.Second
	serverIdleTimeout       = 120 * time.Second
)

// errUpstreamRedirect is returned by ModifyResponse when the upstream answers
// a 3xx. It surfaces through ErrorHandler as a 502 so the client never
// receives a Location header that would make it send the proxy token to an
// attacker-chosen redirect target.
var errUpstreamRedirect = errors.New("gitproxy: upstream redirect refused")

// Config configures the git proxy server.
type Config struct {
	// Listen is the bind address, e.g. "127.0.0.1:8417".
	Listen string
	// PublicURL is the absolute http(s) URL at which clients reach the proxy.
	PublicURL string
	// Token is the HTTP Basic password the proxy accepts; the username is
	// ignored. It is never logged and never forwarded upstream.
	Token string
	// TLSCert and TLSKey enable HTTPS when both are set.
	TLSCert string
	TLSKey  string
	// AllowInsecure explicitly permits plain HTTP (no TLS) on a non-loopback
	// listen address. Without it, New refuses such a configuration.
	AllowInsecure bool
	// Branches lists doublestar globs matched against branch names a push may
	// target. An empty list allows no branch (deny by default).
	Branches []string
}

// Server is an authenticated git smart-HTTP reverse proxy. It exposes only the
// smart-HTTP endpoints (info/refs plus upload/receive-pack), authorizes every
// request against the capability policy before touching the provider, and
// enforces the push policy (branch allowlist, no default branch, no deletes,
// fast-forward only).
type Server struct {
	cfg       Config
	registry  *provider.Registry
	guard     *policy.Guard
	logger    *slog.Logger
	public    *url.URL
	transport *http.Transport
}

// New constructs a Server. registry resolves the provider segment of a route
// and guard performs capability authorization including the .noai overlay. A
// nil logger discards logs. Token, Listen and PublicURL are mandatory; an
// empty Branches list is valid and denies every push. Plain HTTP is only
// accepted on a loopback listen address unless TLS is configured or
// AllowInsecure is set explicitly.
func New(cfg Config, registry *provider.Registry, guard *policy.Guard, logger *slog.Logger) (*Server, error) {
	if registry == nil {
		return nil, errors.New("gitproxy: provider registry is required")
	}
	if guard == nil {
		return nil, errors.New("gitproxy: policy guard is required")
	}
	if cfg.Listen == "" {
		return nil, errors.New("gitproxy: listen address is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("gitproxy: token is required")
	}
	hasCert, hasKey := cfg.TLSCert != "", cfg.TLSKey != ""
	if hasCert != hasKey {
		return nil, errors.New("gitproxy: tls_cert and tls_key must be set together")
	}
	if !hasCert && !cfg.AllowInsecure && !isLoopbackAddr(cfg.Listen) {
		return nil, fmt.Errorf("gitproxy: plain HTTP on non-loopback %q requires TLS or allow_insecure", cfg.Listen)
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("gitproxy: public_url must be an absolute http(s) URL")
	}
	branches := append([]string(nil), cfg.Branches...)
	for _, pattern := range branches {
		if pattern == "" || !doublestar.ValidatePattern(pattern) {
			return nil, fmt.Errorf("gitproxy: invalid branch pattern %q", pattern)
		}
	}
	cfg.Branches = branches
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// A dedicated transport (not http.DefaultTransport): bounded dialing and
	// response headers, but no whole-request timeout so packfile streaming
	// stays possible. HTTP/2 is not forced because git smart-HTTP is HTTP/1.1.
	dialer := &net.Dialer{Timeout: upstreamDialTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       upstreamIdleConnTimeout,
		TLSHandshakeTimeout:   upstreamTLSHandshakeTimeout,
		ExpectContinueTimeout: upstreamExpectContinueTimeout,
		ResponseHeaderTimeout: upstreamResponseHeaderTimeout,
	}
	return &Server{cfg: cfg, registry: registry, guard: guard, logger: logger, public: u, transport: transport}, nil
}

// Handler returns the HTTP handler (rooted at "/git/"). Everything outside the
// supported smart-HTTP surface answers 404.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(gitRoutePrefix, s.serveGit)
	return mux
}

// RemoteURL builds the credential-free proxy clone URL for provider/repo, e.g.
// https://proxy/git/gitlab-work/team/app.git. The result never contains the
// proxy token or any provider credential.
func (s *Server) RemoteURL(providerName, repo string) (string, error) {
	if err := validateNameSegment(providerName, false); err != nil {
		return "", fmt.Errorf("gitproxy: invalid provider name: %w", err)
	}
	if err := validateNameSegment(repo, true); err != nil {
		return "", fmt.Errorf("gitproxy: invalid repository path: %w", err)
	}
	if _, ok := s.registry.Get(providerName); !ok {
		return "", fmt.Errorf("gitproxy: unknown provider %q", providerName)
	}
	base := *s.public
	base.Path = strings.TrimSuffix(base.Path, "/") + "/git/" + providerName + "/" + repo + ".git"
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

// ListenAndServe serves until ctx is cancelled, then shuts down gracefully.
// It returns the serve error, if any; a clean cancellation shutdown returns
// nil. New has already rejected unsafe plain-HTTP configurations, so serving
// here needs no further warning.
func (s *Server) ListenAndServe(ctx context.Context) error {
	tls := s.cfg.TLSCert != "" && s.cfg.TLSKey != ""
	// Bounded header read and idle keep-alives only: a ReadTimeout or
	// WriteTimeout would kill healthy long-running packfile uploads and
	// downloads, so the streaming body stays untimed by design.
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		var err error
		if tls {
			err = srv.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
		} else {
			err = srv.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	s.logger.Info("git proxy listening",
		"listen", s.cfg.Listen, "public_url", s.cfg.PublicURL, "tls", tls)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	// The request context is done; detach so the shutdown is not cancelled with it.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		srv.Close()
		return fmt.Errorf("gitproxy: shutdown: %w", err)
	}
	return <-serveErr
}

// route is one parsed git smart-HTTP request.
type route struct {
	providerName string
	repo         string
	service      string
	capability   policy.Capability
	isPush       bool
}

// serveGit is the single entry point of the handler: authenticate first (an
// unauthenticated client learns nothing about routes), then parse the route,
// authorize the capability, apply the push policy, and forward.
func (s *Server) serveGit(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(r) {
		setAuthChallenge(w)
		s.logger.Info("git proxy authentication failed", "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "git proxy: authentication required", http.StatusUnauthorized)
		return
	}
	rt, ok := s.parseRoute(r)
	if !ok {
		http.Error(w, "git proxy: unsupported endpoint", http.StatusNotFound)
		return
	}
	// An unknown provider name is a routing miss, answered before the policy
	// runs; a policy denial below stays indistinguishable (403).
	p, ok := s.registry.Get(rt.providerName)
	if !ok {
		http.Error(w, "git proxy: unsupported endpoint", http.StatusNotFound)
		return
	}
	// Authorize before any provider call (deny by default). Repository tags are
	// not fetched here: a grant with a tag constraint sees unknown tags and
	// therefore fails closed, which is the documented behavior for git traffic.
	if err := s.guard.Authorize(r.Context(), rt.providerName, rt.repo, rt.capability); err != nil {
		// Every denial answers identically so a .noai repository stays
		// indistinguishable from an unknown or policy-denied one.
		s.logger.Info("git proxy access denied",
			"provider", rt.providerName, "repo", rt.repo,
			"capability", string(rt.capability), "reason", err.Error())
		http.Error(w, "git proxy: repository is not accessible", http.StatusForbidden)
		return
	}
	if rt.isPush {
		if err := s.applyPushPolicy(r, rt, p); err != nil {
			s.logger.Info("git proxy push rejected",
				"provider", rt.providerName, "repo", rt.repo, "reason", err.Error())
			if errors.Is(err, errMalformedPush) {
				http.Error(w, "git proxy: malformed push request", http.StatusBadRequest)
				return
			}
			http.Error(w, "git proxy: push rejected by branch policy", http.StatusForbidden)
			return
		}
	}
	s.forward(w, r, rt, p)
}

// parseRoute splits "/git/<provider>/<repo...>.git/<service...>". The provider
// is the first segment after "/git/"; the repository ends at the LAST ".git/"
// so repository paths cannot contain a ".git" segment (also rejected by
// validateNameSegment). Only the smart-HTTP methods and services pass.
func (s *Server) parseRoute(r *http.Request) (*route, bool) {
	rest := strings.TrimPrefix(r.URL.Path, gitRoutePrefix)
	providerName, remainder, ok := strings.Cut(rest, "/")
	if !ok {
		return nil, false
	}
	i := strings.LastIndex(remainder, ".git/")
	if i < 0 {
		return nil, false
	}
	repo, service := remainder[:i], remainder[i+len(".git/"):]
	if err := validateNameSegment(providerName, false); err != nil {
		return nil, false
	}
	if err := validateNameSegment(repo, true); err != nil {
		return nil, false
	}
	rt := &route{providerName: providerName, repo: repo, service: service}
	switch service {
	case serviceInfoRefs:
		if r.Method != http.MethodGet {
			return nil, false
		}
		switch r.URL.Query().Get("service") {
		case serviceUploadPack:
			rt.capability = policy.CapRepoRead
		case serviceReceivePack:
			rt.capability = policy.CapRepoWrite
		default:
			return nil, false
		}
	case serviceUploadPack:
		if r.Method != http.MethodPost {
			return nil, false
		}
		rt.capability = policy.CapRepoRead
	case serviceReceivePack:
		if r.Method != http.MethodPost {
			return nil, false
		}
		rt.capability = policy.CapRepoWrite
		rt.isPush = true
	default:
		// Dumb-protocol paths (objects/info, HEAD, ...) are not exposed.
		return nil, false
	}
	return rt, true
}

// applyPushPolicy parses the pkt-line command section from the request body,
// checks every ref update against the push policy, and rewinds the body with
// io.MultiReader so the ReverseProxy forwards the original bytes unchanged.
// The Content-Length stays correct because no byte is added or removed.
func (s *Server) applyPushPolicy(r *http.Request, rt *route, p provider.Provider) error {
	br := bufio.NewReader(r.Body)
	updates, consumed, err := ReadReceivePackCommands(br)
	if err != nil {
		return fmt.Errorf("%w: %v", errMalformedPush, err)
	}
	if err := s.checkPush(r.Context(), p, rt.repo, updates); err != nil {
		return err
	}
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(consumed), br))
	return nil
}

// inboundCredentialHeaders are client-supplied headers that could carry an
// upstream credential: the proxy's own Basic auth (Authorization), a GitLab
// API token (Private-Token), session state (Cookie) and proxy authentication
// (Proxy-Authorization). They are always stripped before the provider's own
// Authorization is set, so the upstream sees exactly one credential, the one
// the provider supplied, and never anything the client sent (identity
// separation).
var inboundCredentialHeaders = []string{"Authorization", "Private-Token", "Cookie", "Proxy-Authorization"}

// forward transparently proxies the request to the provider's git endpoint
// with the provider's own git credentials. Client credentials never leave the
// proxy and no client credential header reaches the upstream; upstream
// failures map to a safe 502 without response bodies.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, rt *route, p provider.Provider) {
	remote, err := p.GitRemoteURL(r.Context(), rt.repo)
	if err != nil {
		s.logger.Error("git proxy: remote url lookup failed",
			"provider", rt.providerName, "repo", rt.repo, "error", err.Error())
		http.Error(w, "git proxy: upstream lookup failed", http.StatusBadGateway)
		return
	}
	target, err := url.Parse(remote)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		s.logger.Error("git proxy: remote url is not a valid http(s) URL",
			"provider", rt.providerName, "repo", rt.repo)
		http.Error(w, "git proxy: upstream lookup failed", http.StatusBadGateway)
		return
	}
	target.Path = strings.TrimSuffix(target.Path, "/") + "/" + rt.service
	target.RawQuery = r.URL.RawQuery

	authz, err := p.GitAuthHeader(r.Context(), rt.repo)
	if err != nil {
		s.logger.Error("git proxy: auth header lookup failed",
			"provider", rt.providerName, "repo", rt.repo, "error", err.Error())
		http.Error(w, "git proxy: upstream lookup failed", http.StatusBadGateway)
		return
	}

	rp := &httputil.ReverseProxy{
		// Flush after every write: git smart-HTTP is interactive and must not
		// be buffered.
		FlushInterval: -1,
		Transport:     s.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target
			pr.Out.Host = target.Host
			pr.SetXForwarded()
			// Identity separation: strip every inbound credential header, not
			// just Authorization, so a client cannot smuggle its own token or
			// cookie to the provider through the proxy. Only the provider's
			// git credential set below ever reaches the upstream.
			for _, h := range inboundCredentialHeaders {
				pr.Out.Header.Del(h)
			}
			if authz != "" {
				pr.Out.Header.Set("Authorization", authz)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// Block upstream redirects: a git client following a Location
			// would resend its proxy credentials to the redirect target.
			// Discarding the response also drops every upstream header.
			if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < 400 {
				return errUpstreamRedirect
			}
			return nil
		},
		ErrorHandler: func(ew http.ResponseWriter, _ *http.Request, err error) {
			s.logger.Error("git proxy: upstream request failed",
				"provider", rt.providerName, "repo", rt.repo,
				"service", rt.service, "error", err.Error())
			http.Error(ew, "git proxy: upstream request failed", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// urlReservedChars are the characters with reserved meaning in URL syntax:
// delimiters that truncate a path ("?", "#"), authority/user-info markers
// ("@"), query/parameter separators ("&", ";", "="), plus the remaining RFC
// 3986 gen-delims and sub-delims ("[]", ":", "$", "+", ",", "'", "(", ")",
// "*", "!"). A decoded repository or provider name containing any of them
// could make the policy match diverge from the URL sent upstream, so route
// parsing rejects them outright ("\\" and "%" are rejected separately).
const urlReservedChars = "?#@:;&=+$,[]'()*!"

// validateNameSegment rejects empty names, backslashes and names with
// characters that could not survive the URL round trip unchanged: control
// characters, spaces, percent signs, dot segments and the URL-reserved
// characters in urlReservedChars. Repository paths (slashes true) additionally
// reject empty segments and segments ending in ".git", which would make the
// ".git/<service>" split ambiguous. An attacker cannot use them to diverge
// policy matching from the provider call.
func validateNameSegment(s string, slashes bool) error {
	if s == "" {
		return errors.New("empty name")
	}
	segments := []string{s}
	if slashes {
		segments = strings.Split(s, "/")
	}
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("invalid segment %q", seg)
		}
		if strings.HasSuffix(seg, ".git") {
			return fmt.Errorf("segment %q ends in .git", seg)
		}
		if strings.ContainsRune(seg, '\\') || strings.ContainsRune(seg, '%') {
			return fmt.Errorf("segment %q contains an illegal character", seg)
		}
		if strings.ContainsAny(seg, urlReservedChars) {
			return fmt.Errorf("segment %q contains a URL-reserved character", seg)
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f || r == ' ' {
				return fmt.Errorf("segment %q contains a control character or space", seg)
			}
		}
	}
	return nil
}

// isLoopbackAddr reports whether a listen address binds only loopback
// interfaces. An empty host or a wildcard IP binds all interfaces and is not
// loopback.
func isLoopbackAddr(listen string) bool {
	host := listen
	if h, _, err := net.SplitHostPort(listen); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
