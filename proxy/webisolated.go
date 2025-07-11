package proxy

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dropbox/godropbox/errors"
	"github.com/pritunl/pritunl-zero/authorizer"
	"github.com/pritunl/pritunl-zero/errortypes"
	"github.com/pritunl/pritunl-zero/logger"
	"github.com/pritunl/pritunl-zero/node"
	"github.com/pritunl/pritunl-zero/searches"
	"github.com/pritunl/pritunl-zero/service"
	"github.com/pritunl/pritunl-zero/settings"
	"github.com/pritunl/pritunl-zero/utils"
	"github.com/sirupsen/logrus"
)

type webIsolated struct {
	reqHost     string
	serverHost  string
	serverProto string
	proxyProto  string
	proxyPort   int
	Client      *http.Client
	ErrorLog    *log.Logger
}

func (w *webIsolated) ServeHTTP(rw http.ResponseWriter, r *http.Request,
	authr *authorizer.Authorizer) {

	reqUrl, err := utils.ProxyUrl(r.URL, w.serverProto, w.serverHost)
	if err != nil {
		WriteErrorLog(rw, r, 500, err)
		return
	}

	srcBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		err = errortypes.ReadError{
			errors.Wrap(err, "request: Read request failed"),
		}
		WriteErrorLog(rw, r, 500, err)
		return
	}

	reqBody := bytes.NewBuffer(srcBody)
	req, err := http.NewRequest(r.Method, reqUrl.String(), reqBody)
	if err != nil {
		err = errortypes.RequestError{
			errors.Wrap(err, "request: Create request failed"),
		}
		WriteErrorLog(rw, r, 500, err)
		return
	}

	utils.CopyHeaders(req.Header, r.Header)
	req.Header.Set("X-Forwarded-For",
		node.Self.GetRemoteAddr(r))
	req.Header.Set("X-Forwarded-Host", req.Host)
	req.Header.Set("X-Forwarded-Proto", w.proxyProto)
	req.Header.Set("X-Forwarded-Port", strconv.Itoa(w.proxyPort))

	if authr != nil {
		usr, _ := authr.GetUser(nil)
		if usr != nil {
			req.Header.Set("X-Forwarded-User", usr.Username)
		}
	}

	if w.reqHost != "" {
		req.Host = w.reqHost
	}

	stripCookieHeaders(req)

	if settings.Elastic.ProxyRequests {
		index := searches.Request{
			Address:   node.Self.GetRemoteAddr(r),
			Timestamp: time.Now(),
			Scheme:    reqUrl.Scheme,
			Host:      reqUrl.Host,
			Path:      reqUrl.Path,
			Query:     reqUrl.Query(),
			Header:    r.Header.Clone(),
		}

		if authr.IsValid() {
			usr, _ := authr.GetUser(nil)

			if usr != nil {
				index.User = usr.Id.Hex()
				index.Username = usr.Username
				index.Session = authr.SessionId()
			}
		}

		contentType := strings.ToLower(r.Header.Get("Content-Type"))
		if searches.RequestTypes.Contains(contentType) &&
			req.ContentLength != 0 && srcBody != nil {

			index.Body = string(srcBody)
		}

		index.Index()
	}

	resp, err := w.Client.Do(req)
	if err != nil {
		err = errortypes.RequestError{
			errors.Wrap(err, "request: Request failed"),
		}
		WriteErrorLog(rw, r, 500, err)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	utils.CopyHeaders(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(rw, resp.Body)
}

func (w *webIsolated) ServeOptionsHTTP(rw http.ResponseWriter,
	r *http.Request, authr *authorizer.Authorizer) {

	reqUrl, err := utils.ProxyUrlLimited(r.URL, w.serverProto, w.serverHost)
	if err != nil {
		WriteErrorLog(rw, r, 500, err)
		return
	}

	req, err := http.NewRequest("OPTIONS", reqUrl.String(), nil)
	if err != nil {
		err = errortypes.RequestError{
			errors.Wrap(err, "request: Create request failed"),
		}
		WriteErrorLog(rw, r, 500, err)
		return
	}

	utils.CopyHeaders(req.Header, r.Header)
	req.Header.Set("X-Forwarded-For",
		node.Self.GetRemoteAddr(r))
	req.Header.Set("X-Forwarded-Host", req.Host)
	req.Header.Set("X-Forwarded-Proto", w.proxyProto)
	req.Header.Set("X-Forwarded-Port", strconv.Itoa(w.proxyPort))

	if authr != nil {
		usr, _ := authr.GetUser(nil)
		if usr != nil {
			req.Header.Set("X-Forwarded-User", usr.Username)
		}
	}

	if w.reqHost != "" {
		req.Host = w.reqHost
	}

	stripCookieHeaders(req)

	if settings.Elastic.ProxyRequests {
		index := searches.Request{
			Address:   node.Self.GetRemoteAddr(r),
			Timestamp: time.Now(),
			Scheme:    reqUrl.Scheme,
			Host:      reqUrl.Host,
			Path:      reqUrl.Path,
			Query:     reqUrl.Query(),
			Header:    r.Header.Clone(),
		}

		if authr.IsValid() {
			usr, _ := authr.GetUser(nil)

			if usr != nil {
				index.User = usr.Id.Hex()
				index.Username = usr.Username
				index.Session = authr.SessionId()
			}
		}

		index.Index()
	}

	resp, err := w.Client.Do(req)
	if err != nil {
		err = errortypes.RequestError{
			errors.Wrap(err, "request: Request failed"),
		}
		WriteErrorLog(rw, r, 500, err)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	utils.CopyHeaders(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(rw, resp.Body)
}

func newWebIsolated(proxyProto string, proxyPort int, host *Host,
	server *service.Server) (w *webIsolated) {

	requestTimeout := time.Duration(
		settings.Router.RequestTimeout) * time.Second
	dialTimeout := time.Duration(
		settings.Router.DialTimeout) * time.Second
	dialKeepAlive := time.Duration(
		settings.Router.DialKeepAlive) * time.Second
	maxIdleConns := settings.Router.MaxIdleConns
	maxIdleConnsPerHost := settings.Router.MaxIdleConnsPerHost
	idleConnTimeout := time.Duration(
		settings.Router.IdleConnTimeout) * time.Second
	handshakeTimeout := time.Duration(
		settings.Router.HandshakeTimeout) * time.Second
	continueTimeout := time.Duration(
		settings.Router.ContinueTimeout) * time.Second
	headerTimeout := time.Duration(
		settings.Router.HeaderTimeout) * time.Second

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
	}
	if settings.Router.SkipVerify || net.ParseIP(server.Hostname) != nil {
		tlsConfig.InsecureSkipVerify = true
	}

	if host.ClientCertificate != nil {
		tlsConfig.Certificates = []tls.Certificate{
			*host.ClientCertificate,
		}
	}

	writer := &logger.ErrorWriter{
		Message: "node: Proxy isolated server error",
		Fields: logrus.Fields{
			"service": host.Service.Name,
			"domain":  host.Domain.Domain,
			"server": fmt.Sprintf(
				"%s://%s:%d",
				server.Protocol,
				server.Hostname,
				server.Port,
			),
		},
		Filters: []string{
			"context canceled",
		},
	}

	transportFix := &TransportFix{
		transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: dialKeepAlive,
				DualStack: true,
			}).DialContext,
			MaxResponseHeaderBytes: int64(
				settings.Router.MaxResponseHeaderBytes),
			MaxIdleConns:          maxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			ResponseHeaderTimeout: headerTimeout,
			IdleConnTimeout:       idleConnTimeout,
			TLSHandshakeTimeout:   handshakeTimeout,
			ExpectContinueTimeout: continueTimeout,
			TLSClientConfig:       tlsConfig,
		},
	}

	if settings.Router.DisableIdleConnections {
		transportFix.transport.DisableKeepAlives = true
		transportFix.transport.MaxIdleConns = 0
		transportFix.transport.MaxIdleConnsPerHost = 0
	}

	w = &webIsolated{
		reqHost:     host.Domain.Host,
		serverProto: server.Protocol,
		serverHost:  utils.FormatHostPort(server.Hostname, server.Port),
		proxyProto:  proxyProto,
		proxyPort:   proxyPort,
		Client: &http.Client{
			Transport: transportFix,
			CheckRedirect: func(r *http.Request, v []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: requestTimeout,
		},
		ErrorLog: log.New(writer, "", 0),
	}

	return
}
