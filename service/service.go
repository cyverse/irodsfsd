package service

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cyverse/irodsfsd/commons"
	"github.com/cyverse/irodsfsd/service/api"
	"github.com/cyverse/irodsfsd/service/store"
	"github.com/cyverse/irodsfsd/service/web"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

// Service owns the gRPC and REST listeners and their shared mount API.
type Service struct {
	config     *commons.Config
	grpcServer *grpc.Server
	restServer *http.Server
	server     *MountServer
	logger     *log.Entry
	dataStore  *store.Store
	// manager is nil for a Service built directly via newService with a
	// fake mountOperations (as tests do); periodic reconciliation and
	// mount shutdown require the concrete type and are skipped when nil.
	manager *MountManager
	mux     *http.ServeMux

	mutex         sync.Mutex
	listener      net.Listener
	listen        func(string, string) (net.Listener, error)
	restListen    func(string, string) (net.Listener, error)
	restListener  net.Listener
	started       bool
	cancel        context.CancelFunc
	reconcileDone chan struct{}
}

func NewService(config *commons.Config) (*Service, error) {
	if config == nil {
		return nil, errors.New("config is required")
	}
	key, err := config.GetRecoveryEncryptionKey()
	if err != nil {
		return nil, err
	}
	dataStore, err := store.Open(config.GetMountDatabasePath(), key)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open mount database")
	}
	repository := store.NewMountRepository(dataStore)

	manager, err := NewMountManager(config, repository)
	if err != nil {
		_ = dataStore.Close()
		return nil, errors.Wrap(err, "failed to create mount manager")
	}
	// Reconcile stored mount intent against the real system state before the
	// API begins accepting requests, so a client never sees a mount that is
	// still being restored or torn down from a previous run.
	if err := manager.Reconcile(context.Background()); err != nil {
		_ = dataStore.Close()
		return nil, errors.Wrap(err, "failed to reconcile mount state at startup")
	}
	svc, err := newService(config, manager)
	if err != nil {
		_ = dataStore.Close()
		return nil, err
	}
	svc.dataStore = dataStore
	svc.manager = manager
	manager.metrics = NewMetrics(manager)
	svc.registerMetricsRoute(manager.metrics)
	return svc, nil
}

func newService(config *commons.Config, manager mountOperations) (*Service, error) {
	if config == nil {
		return nil, errors.New("config is required")
	}
	server, err := newMountServer(manager)
	if err != nil {
		return nil, err
	}
	grpcServer := grpc.NewServer(grpc.MaxConcurrentStreams(0))
	api.RegisterMountServiceServer(grpcServer, server)
	restHandler := NewRESTHandler(server, config)
	mux := http.NewServeMux()
	restHandler.RegisterRoutes(mux)
	// The management UI shares the REST API's origin (design.md) and is a
	// single embedded page, so it is registered as a catch-all for
	// whatever the REST routes above did not already claim.
	mux.Handle("GET /", web.Handler())
	return &Service{
		config:     config,
		grpcServer: grpcServer,
		restServer: &http.Server{Handler: mux},
		server:     server,
		mux:        mux,
		logger:     log.WithFields(log.Fields{}),
		listen:     net.Listen,
		restListen: net.Listen,
	}, nil
}

// registerMetricsRoute adds the /metrics endpoint to svc's mux, which
// already has the catch-all "GET /" web UI route registered by newService.
// Go's ServeMux forbids mixing a method-less pattern with a more specific
// path against a method-restricted, less specific one (it cannot resolve
// the ambiguity), so this must stay method-restricted too — see
// TestServiceMetricsAndWebRoutesCoexist, which exists specifically to catch
// a regression here without needing NewService's FUSE/BadgerDB
// dependencies.
func (svc *Service) registerMetricsRoute(metrics *Metrics) {
	svc.mux.Handle("GET /metrics", metrics.Handler())
}

func (svc *Service) Start() error {
	svc.mutex.Lock()
	defer svc.mutex.Unlock()
	if svc.started {
		return errors.New("service is already started")
	}

	scheme, endpoint, err := commons.ParseServiceEndpoint(svc.config.GetServiceEndpoint())
	if err != nil {
		return err
	}
	listener, err := svc.listen(scheme, endpoint)
	if err != nil {
		return errors.Wrapf(err, "failed to listen on %s endpoint %q", scheme, endpoint)
	}
	var restListener net.Listener
	if svc.config.ManagementServicePort > 0 {
		restEndpoint := fmt.Sprintf(":%d", svc.config.ManagementServicePort)
		restListener, err = svc.restListen("tcp", restEndpoint)
		if err != nil {
			_ = listener.Close()
			return errors.Wrapf(err, "failed to listen on REST endpoint %q", restEndpoint)
		}
		svc.restListener = restListener
	}

	svc.listener = listener
	svc.started = true
	svc.logger.Infof("gRPC service listening on %s://%s", scheme, endpoint)
	go func() {
		if serveErr := svc.grpcServer.Serve(listener); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			svc.logger.WithError(serveErr).Error("gRPC service stopped unexpectedly")
		}
	}()
	if restListener != nil {
		svc.logger.Infof("REST service listening on http://%s", restListener.Addr())
		go func() {
			if serveErr := svc.restServer.Serve(restListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				svc.logger.WithError(serveErr).Error("REST service stopped unexpectedly")
			}
		}()
	}

	if svc.manager != nil {
		var ctx context.Context
		ctx, svc.cancel = context.WithCancel(context.Background())
		svc.reconcileDone = make(chan struct{})
		go svc.runPeriodicReconcile(ctx, svc.manager)
	}
	return nil
}

// runPeriodicReconcile compares stored mount intent against real system
// state at reconcile_interval, so drift (a mount that died unnoticed, one
// that lingers after its supervising records were cleaned up elsewhere) is
// corrected without requiring a daemon restart.
func (svc *Service) runPeriodicReconcile(ctx context.Context, manager *MountManager) {
	defer close(svc.reconcileDone)

	interval := time.Duration(svc.config.ReconcileInterval)
	if interval <= 0 {
		interval = commons.ReconcileIntervalDefault
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := manager.Reconcile(ctx); err != nil {
				svc.logger.WithError(err).Warn("periodic reconciliation failed")
			}
		}
	}
}

// Stop drains the gRPC and REST listeners, cancels the periodic reconcile
// loop, then safely unmounts every managed mount before returning. Shutdown
// order follows design.md: HTTP drain, reconcile-loop cancellation, safe
// unmount and child signaling, controller wait — with no separate
// grace-period sleep, since each step above is itself already bounded.
func (svc *Service) Stop() {
	svc.mutex.Lock()
	if !svc.started {
		svc.mutex.Unlock()
		return
	}
	svc.started = false
	cancel := svc.cancel
	reconcileDone := svc.reconcileDone
	manager := svc.manager
	svc.mutex.Unlock()

	var wait sync.WaitGroup
	if svc.restListener != nil {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := svc.restServer.Shutdown(context.Background()); err != nil {
				svc.logger.WithError(err).Warn("failed to stop REST service cleanly")
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		svc.grpcServer.GracefulStop()
	}()
	wait.Wait()

	if cancel != nil {
		cancel()
	}
	if reconcileDone != nil {
		<-reconcileDone
	}
	if manager != nil {
		manager.Shutdown(context.Background())
	}

	svc.removeUnixSocket()
	svc.logger.Info("gRPC and REST services stopped")
}

func (svc *Service) Release() {
	svc.Stop()
	svc.removeUnixSocket()
	if svc.dataStore != nil {
		if err := svc.dataStore.Close(); err != nil {
			svc.logger.WithError(err).Warn("failed to close mount database cleanly")
		}
	}
}

func (svc *Service) removeUnixSocket() {
	scheme, endpoint, err := commons.ParseServiceEndpoint(svc.config.GetServiceEndpoint())
	if err == nil && scheme == "unix" {
		if removeErr := os.Remove(endpoint); removeErr != nil && !os.IsNotExist(removeErr) {
			svc.logger.WithError(removeErr).Warnf("failed to remove unix socket %q", endpoint)
		}
	}
}
