package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/shared"
	"github.com/tidwall/sjson"
)

type peerMember struct {
	peerID       string
	reverseProxy *httputil.ReverseProxy
}

type peerRoute struct {
	peerID          string
	upstreamModelID string
	apiKey          string
}

type Peer struct {
	cfg     config.Config
	logger  *logmon.Monitor
	members map[string]*peerMember // peerID -> transport

	routesMu sync.RWMutex
	routes   map[string]peerRoute // bare or FQ model key -> route

	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool
	inflight     sync.WaitGroup
}

func NewPeer(cfg config.Config, logger *logmon.Monitor) (*Peer, error) {
	peers := cfg.Peers
	members := make(map[string]*peerMember)

	peerIDs := make([]string, 0, len(peers))
	for peerID := range peers {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)

	for _, peerID := range peerIDs {
		peer := peers[peerID]

		peerTransport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   time.Duration(peer.Timeouts.Connect) * time.Second,
				KeepAlive: time.Duration(peer.Timeouts.KeepAlive) * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   time.Duration(peer.Timeouts.TLSHandshake) * time.Second,
			ResponseHeaderTimeout: time.Duration(peer.Timeouts.ResponseHeader) * time.Second,
			ExpectContinueTimeout: time.Duration(peer.Timeouts.ExpectContinue) * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       time.Duration(peer.Timeouts.IdleConn) * time.Second,
		}

		reverseProxy := &httputil.ReverseProxy{
			Transport: peerTransport,
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(peer.ProxyURL)
				r.Out.Host = r.Out.URL.Host
			},
		}

		reverseProxy.ModifyResponse = func(resp *http.Response) error {
			if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				resp.Header.Set("X-Accel-Buffering", "no")
			}
			return nil
		}

		capturedID := peerID
		reverseProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Warnf("peer %s: proxy error: %v", capturedID, err)
			errMsg := fmt.Sprintf("peer proxy error: %v", err)
			if runtime.GOOS == "darwin" && strings.Contains(err.Error(), "connect: no route to host") {
				errMsg += " (hint: on macOS, check System Settings > Privacy & Security > Local Network permissions)"
			}
			http.Error(w, errMsg, http.StatusBadGateway)
		}

		members[peerID] = &peerMember{
			peerID:       peerID,
			reverseProxy: reverseProxy,
		}
	}

	shutdownCtx, shutdownFn := context.WithCancel(context.Background())

	return &Peer{
		cfg:         cfg,
		logger:      logger,
		members:     members,
		routes:      make(map[string]peerRoute),
		shutdownCtx: shutdownCtx,
		shutdownFn:  shutdownFn,
	}, nil
}

func (r *Peer) Handles(model string) bool {
	r.routesMu.RLock()
	defer r.routesMu.RUnlock()
	_, ok := r.routes[model]
	return ok
}

// ReplaceDiscovered installs discovery-owned peer routes (bare + FQ keys).
func (r *Peer) ReplaceDiscovered(routes map[string]discovery.PeerRoute) error {
	next := make(map[string]peerRoute, len(routes))
	for key, spec := range routes {
		if _, ok := r.members[spec.PeerID]; !ok {
			r.logger.Warnf("discovery peer route %q: unknown peer %s, skipping", key, spec.PeerID)
			continue
		}
		next[key] = peerRoute{
			peerID:          spec.PeerID,
			upstreamModelID: spec.UpstreamModelID,
			apiKey:          spec.ApiKey,
		}
	}
	r.routesMu.Lock()
	r.routes = next
	r.routesMu.Unlock()
	return nil
}

func (r *Peer) Shutdown(timeout time.Duration) error {
	if !r.shuttingDown.CompareAndSwap(false, true) {
		return fmt.Errorf("shutdown already in progress")
	}

	if timeout == 0 {
		r.shutdownFn()
		r.inflight.Wait()
		return nil
	}

	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		r.shutdownFn()
		r.inflight.Wait()
		return fmt.Errorf("peer shutdown timed out after %v", timeout)
	}
}

func (r *Peer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.shuttingDown.Load() {
		shared.SendError(w, req, fmt.Errorf("peer proxy is shutting down"))
		return
	}
	r.inflight.Add(1)
	defer r.inflight.Done()

	data, err := shared.FetchContext(req, r.cfg)
	if err != nil {
		shared.SendError(w, req, err)
		return
	}

	r.routesMu.RLock()
	route, found := r.routes[data.ModelID]
	r.routesMu.RUnlock()
	if !found {
		r.logger.Warnf("peer model not found: %s", data.ModelID)
		shared.SendError(w, req, ErrNoPeerModelFound)
		return
	}

	member, ok := r.members[route.peerID]
	if !ok {
		shared.SendError(w, req, ErrNoPeerModelFound)
		return
	}

	r.logger.Debugf("peer: routing model %s to peer %s (upstream=%s)", data.ModelID, route.peerID, route.upstreamModelID)

	if route.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+route.apiKey)
		req.Header.Set("x-api-key", route.apiKey)
	}

	if route.upstreamModelID != "" && data.Model != route.upstreamModelID {
		if err := rewritePeerRequestModel(req, route.upstreamModelID); err != nil {
			shared.SendError(w, req, err)
			return
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopReq := context.AfterFunc(req.Context(), cancel)
	stopShutdown := context.AfterFunc(r.shutdownCtx, cancel)
	req = req.WithContext(ctx)

	member.reverseProxy.ServeHTTP(w, req)

	stopShutdown()
	stopReq()
	cancel()
}

func rewritePeerRequestModel(req *http.Request, modelID string) error {
	ct := req.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") || req.Body == nil {
		return nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return fmt.Errorf("reading body for model rewrite: %w", err)
	}
	_ = req.Body.Close()
	body, err = sjson.SetBytes(body, "model", modelID)
	if err != nil {
		return fmt.Errorf("rewriting model: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	req.Header.Del("Transfer-Encoding")
	return nil
}
