package webdavd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/middleware"
	"github.com/rs/cors"
	"github.com/rs/xid"
	"golang.org/x/net/webdav"

	"github.com/drakkan/sftpgo/common"
	"github.com/drakkan/sftpgo/dataprovider"
	"github.com/drakkan/sftpgo/logger"
	"github.com/drakkan/sftpgo/metrics"
	"github.com/drakkan/sftpgo/utils"
)

var (
	err401        = errors.New("Unauthorized")
	xForwardedFor = http.CanonicalHeaderKey("X-Forwarded-For")
	xRealIP       = http.CanonicalHeaderKey("X-Real-IP")
)

type webDavServer struct {
	config  *Configuration
	binding Binding
}

func (s *webDavServer) listenAndServe(compressor *middleware.Compressor) error {
	handler := compressor.Handler(s)
	httpServer := &http.Server{
		Addr:              s.binding.GetAddress(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16, // 64KB
		ErrorLog:          log.New(&logger.StdLoggerWrapper{Sender: logSender}, "", 0),
	}
	if s.config.Cors.Enabled {
		c := cors.New(cors.Options{
			AllowedOrigins:     s.config.Cors.AllowedOrigins,
			AllowedMethods:     s.config.Cors.AllowedMethods,
			AllowedHeaders:     s.config.Cors.AllowedHeaders,
			ExposedHeaders:     s.config.Cors.ExposedHeaders,
			MaxAge:             s.config.Cors.MaxAge,
			AllowCredentials:   s.config.Cors.AllowCredentials,
			OptionsPassthrough: true,
		})
		handler = c.Handler(handler)
	}
	httpServer.Handler = handler
	if certMgr != nil && s.binding.EnableHTTPS {
		serviceStatus.Bindings = append(serviceStatus.Bindings, s.binding)
		httpServer.TLSConfig = &tls.Config{
			GetCertificate: certMgr.GetCertificateFunc(),
			MinVersion:     tls.VersionTLS12,
		}
		if s.binding.ClientAuthType == 1 {
			httpServer.TLSConfig.ClientCAs = certMgr.GetRootCAs()
			httpServer.TLSConfig.ClientAuth = tls.RequireAndVerifyClientCert
			httpServer.TLSConfig.VerifyConnection = s.verifyTLSConnection
		}
		logger.Info(logSender, "", "starting HTTPS serving, binding: %v", s.binding.GetAddress())
		return httpServer.ListenAndServeTLS("", "")
	}
	s.binding.EnableHTTPS = false
	serviceStatus.Bindings = append(serviceStatus.Bindings, s.binding)
	logger.Info(logSender, "", "starting HTTP serving, binding: %v", s.binding.GetAddress())
	return httpServer.ListenAndServe()
}

func (s *webDavServer) verifyTLSConnection(state tls.ConnectionState) error {
	if certMgr != nil {
		var clientCrt *x509.Certificate
		var clientCrtName string
		if len(state.PeerCertificates) > 0 {
			clientCrt = state.PeerCertificates[0]
			clientCrtName = clientCrt.Subject.String()
		}
		if len(state.VerifiedChains) == 0 {
			logger.Warn(logSender, "", "TLS connection cannot be verified: unable to get verification chain")
			return errors.New("TLS connection cannot be verified: unable to get verification chain")
		}
		for _, verifiedChain := range state.VerifiedChains {
			var caCrt *x509.Certificate
			if len(verifiedChain) > 0 {
				caCrt = verifiedChain[len(verifiedChain)-1]
			}
			if certMgr.IsRevoked(clientCrt, caCrt) {
				logger.Debug(logSender, "", "tls handshake error, client certificate %#v has been revoked", clientCrtName)
				return common.ErrCrtRevoked
			}
		}
	}

	return nil
}

// returns true if we have to handle a HEAD response, for a directory, ourself
func (s *webDavServer) checkRequestMethod(ctx context.Context, r *http.Request, connection *Connection) bool {
	// see RFC4918, section 9.4
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		p := path.Clean(r.URL.Path)
		info, err := connection.Stat(ctx, p)
		if err == nil && info.IsDir() {
			if r.Method == http.MethodHead {
				return true
			}
			r.Method = "PROPFIND"
			if r.Header.Get("Depth") == "" {
				r.Header.Add("Depth", "1")
			}
		}
	}
	return false
}

// ServeHTTP implements the http.Handler interface
func (s *webDavServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error(logSender, "", "panic in ServeHTTP: %#v stack strace: %v", r, string(debug.Stack()))
			http.Error(w, common.ErrGenericFailure.Error(), http.StatusInternalServerError)
		}
	}()
	if !common.Connections.IsNewConnectionAllowed() {
		logger.Log(logger.LevelDebug, common.ProtocolFTP, "", "connection refused, configured limit reached")
		http.Error(w, common.ErrConnectionDenied.Error(), http.StatusServiceUnavailable)
		return
	}
	checkRemoteAddress(r)
	ipAddr := utils.GetIPFromRemoteAddress(r.RemoteAddr)
	if common.IsBanned(ipAddr) {
		http.Error(w, common.ErrConnectionDenied.Error(), http.StatusForbidden)
		return
	}
	if err := common.Config.ExecutePostConnectHook(ipAddr, common.ProtocolWebDAV); err != nil {
		http.Error(w, common.ErrConnectionDenied.Error(), http.StatusForbidden)
		return
	}
	user, _, lockSystem, err := s.authenticate(r, ipAddr)
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Basic realm=\"SFTPGo WebDAV\"")
		http.Error(w, err401.Error(), http.StatusUnauthorized)
		return
	}

	connectionID, err := s.validateUser(&user, r)
	if err != nil {
		updateLoginMetrics(&user, ipAddr, err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	fs, err := user.GetFilesystem(connectionID)
	if err != nil {
		updateLoginMetrics(&user, ipAddr, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	updateLoginMetrics(&user, ipAddr, err)

	ctx := context.WithValue(r.Context(), requestIDKey, connectionID)
	ctx = context.WithValue(ctx, requestStartKey, time.Now())

	connection := &Connection{
		BaseConnection: common.NewBaseConnection(connectionID, common.ProtocolWebDAV, user, fs),
		request:        r,
	}
	common.Connections.Add(connection)
	defer common.Connections.Remove(connection.GetID())

	dataprovider.UpdateLastLogin(user) //nolint:errcheck

	if s.checkRequestMethod(ctx, r, connection) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		w.Write([]byte("")) //nolint:errcheck
		return
	}

	handler := webdav.Handler{
		FileSystem: connection,
		LockSystem: lockSystem,
		Logger:     writeLog,
	}
	handler.ServeHTTP(w, r.WithContext(ctx))
}

func (s *webDavServer) authenticate(r *http.Request, ip string) (dataprovider.User, bool, webdav.LockSystem, error) {
	var user dataprovider.User
	var err error
	username, password, ok := r.BasicAuth()
	if !ok {
		return user, false, nil, err401
	}
	result, ok := dataprovider.GetCachedWebDAVUser(username)
	if ok {
		cachedUser := result.(*dataprovider.CachedUser)
		if cachedUser.IsExpired() {
			dataprovider.RemoveCachedWebDAVUser(username)
		} else {
			if password != "" && cachedUser.Password == password {
				return cachedUser.User, true, cachedUser.LockSystem, nil
			}
			updateLoginMetrics(&cachedUser.User, ip, dataprovider.ErrInvalidCredentials)
			return user, false, nil, dataprovider.ErrInvalidCredentials
		}
	}
	user, err = dataprovider.CheckUserAndPass(username, password, ip, common.ProtocolWebDAV)
	if err != nil {
		user.Username = username
		updateLoginMetrics(&user, ip, err)
		return user, false, nil, err
	}
	lockSystem := webdav.NewMemLS()
	if password != "" {
		cachedUser := &dataprovider.CachedUser{
			User:       user,
			Password:   password,
			LockSystem: lockSystem,
		}
		if s.config.Cache.Users.ExpirationTime > 0 {
			cachedUser.Expiration = time.Now().Add(time.Duration(s.config.Cache.Users.ExpirationTime) * time.Minute)
		}
		dataprovider.CacheWebDAVUser(cachedUser, s.config.Cache.Users.MaxSize)
		if user.FsConfig.Provider != dataprovider.SFTPFilesystemProvider {
			// for sftp fs check root path does nothing so don't open a useless SFTP connection
			tempFs, err := user.GetFilesystem("temp")
			if err == nil {
				tempFs.CheckRootPath(user.Username, user.UID, user.GID)
				tempFs.Close()
			}
		}
	}
	return user, false, lockSystem, nil
}

func (s *webDavServer) validateUser(user *dataprovider.User, r *http.Request) (string, error) {
	connID := xid.New().String()
	connectionID := fmt.Sprintf("%v_%v", common.ProtocolWebDAV, connID)

	if !filepath.IsAbs(user.HomeDir) {
		logger.Warn(logSender, connectionID, "user %#v has an invalid home dir: %#v. Home dir must be an absolute path, login not allowed",
			user.Username, user.HomeDir)
		return connID, fmt.Errorf("cannot login user with invalid home dir: %#v", user.HomeDir)
	}
	if utils.IsStringInSlice(common.ProtocolWebDAV, user.Filters.DeniedProtocols) {
		logger.Debug(logSender, connectionID, "cannot login user %#v, protocol DAV is not allowed", user.Username)
		return connID, fmt.Errorf("Protocol DAV is not allowed for user %#v", user.Username)
	}
	if !user.IsLoginMethodAllowed(dataprovider.LoginMethodPassword, nil) {
		logger.Debug(logSender, connectionID, "cannot login user %#v, password login method is not allowed", user.Username)
		return connID, fmt.Errorf("Password login method is not allowed for user %#v", user.Username)
	}
	if user.MaxSessions > 0 {
		activeSessions := common.Connections.GetActiveSessions(user.Username)
		if activeSessions >= user.MaxSessions {
			logger.Debug(logSender, connID, "authentication refused for user: %#v, too many open sessions: %v/%v", user.Username,
				activeSessions, user.MaxSessions)
			return connID, fmt.Errorf("too many open sessions: %v", activeSessions)
		}
	}
	if dataprovider.GetQuotaTracking() > 0 && user.HasOverlappedMappedPaths() {
		logger.Debug(logSender, connectionID, "cannot login user %#v, overlapping mapped folders are allowed only with quota tracking disabled",
			user.Username)
		return connID, errors.New("overlapping mapped folders are allowed only with quota tracking disabled")
	}
	if !user.IsLoginFromAddrAllowed(r.RemoteAddr) {
		logger.Debug(logSender, connectionID, "cannot login user %#v, remote address is not allowed: %v", user.Username, r.RemoteAddr)
		return connID, fmt.Errorf("Login for user %#v is not allowed from this address: %v", user.Username, r.RemoteAddr)
	}
	return connID, nil
}

func writeLog(r *http.Request, err error) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	fields := map[string]interface{}{
		"remote_addr": r.RemoteAddr,
		"proto":       r.Proto,
		"method":      r.Method,
		"user_agent":  r.UserAgent(),
		"uri":         fmt.Sprintf("%s://%s%s", scheme, r.Host, r.RequestURI)}
	if reqID, ok := r.Context().Value(requestIDKey).(string); ok {
		fields["request_id"] = reqID
	}
	if reqStart, ok := r.Context().Value(requestStartKey).(time.Time); ok {
		fields["elapsed_ms"] = time.Since(reqStart).Nanoseconds() / 1000000
	}
	logger.GetLogger().Info().
		Timestamp().
		Str("sender", logSender).
		Fields(fields).
		Err(err).
		Send()
}

func checkRemoteAddress(r *http.Request) {
	if common.Config.ProxyProtocol != 0 {
		return
	}

	var ip string

	if xrip := r.Header.Get(xRealIP); xrip != "" {
		ip = xrip
	} else if xff := r.Header.Get(xForwardedFor); xff != "" {
		i := strings.Index(xff, ", ")
		if i == -1 {
			i = len(xff)
		}
		ip = strings.TrimSpace(xff[:i])
	}

	if len(ip) > 0 {
		r.RemoteAddr = ip
	}
}

func updateLoginMetrics(user *dataprovider.User, ip string, err error) {
	metrics.AddLoginAttempt(dataprovider.LoginMethodPassword)
	if err != nil {
		logger.ConnectionFailedLog(user.Username, ip, dataprovider.LoginMethodPassword, common.ProtocolWebDAV, err.Error())
		event := common.HostEventLoginFailed
		if _, ok := err.(*dataprovider.RecordNotFoundError); ok {
			event = common.HostEventUserNotFound
		}
		common.AddDefenderEvent(ip, event)
	}
	metrics.AddLoginResult(dataprovider.LoginMethodPassword, err)
	dataprovider.ExecutePostLoginHook(user, dataprovider.LoginMethodPassword, ip, common.ProtocolWebDAV, err)
}
