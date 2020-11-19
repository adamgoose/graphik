package runtime

import (
	"context"
	"fmt"
	apipb "github.com/autom8ter/graphik/api"
	"github.com/autom8ter/graphik/auth"
	"github.com/autom8ter/graphik/flags"
	"github.com/autom8ter/graphik/graph"
	"github.com/autom8ter/graphik/logger"
	"github.com/autom8ter/graphik/storage/log"
	"github.com/autom8ter/graphik/storage/snap"
	"github.com/autom8ter/machine"
	"github.com/golang/protobuf/proto"
	grpc_zap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	"github.com/hashicorp/raft"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	raftTimeout = 10 * time.Second
)

type Runtime struct {
	machine     *machine.Machine
	config      *auth.Config
	mu          sync.RWMutex
	raft        *raft.Raft
	graph       *graph.Graph
	close       sync.Once
	triggers    []apipb.TriggerServiceClient
	authorizers []apipb.AuthorizationServiceClient
	closed      bool
	closers     []func()
	logStore    *log.LogStore
	snapStore   *snap.SnapshotStore
}

func New(ctx context.Context, cfg *flags.Flags) (*Runtime, error) {
	os.MkdirAll(cfg.StoragePath, 0700)
	var triggers []apipb.TriggerServiceClient
	var authorizers []apipb.AuthorizationServiceClient
	var closers []func()
	for _, plugin := range cfg.Triggers {
		if plugin == "" {
			continue
		}
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		conn, err := grpc.DialContext(
			tctx,
			plugin,
			grpc.WithChainUnaryInterceptor(grpc_zap.UnaryClientInterceptor(logger.Logger())),
			grpc.WithInsecure(),
		)
		if err != nil {
			return nil, errors.Wrap(err, "failed to dial trigger")
		}
		triggers = append(triggers, apipb.NewTriggerServiceClient(conn))
		closers = append(closers, func() {
			conn.Close()
		})
	}
	for _, plugin := range cfg.Authorizers {
		if plugin == "" {
			continue
		}
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		conn, err := grpc.DialContext(
			tctx,
			plugin,
			grpc.WithChainUnaryInterceptor(grpc_zap.UnaryClientInterceptor(logger.Logger())),
			grpc.WithInsecure(),
		)
		if err != nil {
			return nil, errors.Wrap(err, "failed to dial authorizer")
		}
		authorizers = append(authorizers, apipb.NewAuthorizationServiceClient(conn))
		closers = append(closers, func() {
			conn.Close()
		})
	}
	m := machine.New(ctx)
	rconfig := raft.DefaultConfig()
	rconfig.LocalID = raft.ServerID(cfg.RaftID)
	// Setup Raft communication.
	addr, err := net.ResolveTCPAddr("tcp", cfg.BindRaft)
	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve raft bind address")
	}
	transport, err := raft.NewTCPTransport(cfg.BindRaft, addr, 3, raftTimeout, os.Stderr)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create raft transport")
	}

	// Create the snapshot store. This allows the Raft to truncate the log.
	snapshots, err := raft.NewFileSnapshotStore(cfg.StoragePath, 2, os.Stderr)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create snapshot store")
	}
	logStore, err := log.NewLogStore(filepath.Join(cfg.StoragePath, "logs.db"))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create bolt store")
	}
	snapshotStore, err := snap.NewSnapshotStore(filepath.Join(cfg.StoragePath, "snapshots.db"))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create bolt store")
	}
	c, err := auth.New(cfg.JWKS)
	if err != nil {
		return nil, err
	}
	g, err := graph.New(filepath.Join(cfg.StoragePath, "graph.db"))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create graph")
	}
	s := &Runtime{
		machine:     m,
		config:      c,
		raft:        nil,
		mu:          sync.RWMutex{},
		graph:       g,
		close:       sync.Once{},
		closers:     closers,
		triggers:    triggers,
		logStore:    logStore,
		snapStore:   snapshotStore,
		authorizers: authorizers,
	}
	rft, err := raft.NewRaft(rconfig, s, logStore, snapshotStore, snapshots, transport)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create raft")
	}
	s.raft = rft
	if cfg.JoinRaft == "" {
		configuration := raft.Configuration{
			Servers: []raft.Server{
				{
					ID:      rconfig.LocalID,
					Address: transport.LocalAddr(),
				},
			},
		}
		s.raft.BootstrapCluster(configuration)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := grpc.DialContext(ctx, cfg.JoinRaft, grpc.WithInsecure())
		if err != nil {
			return nil, errors.Wrap(err, "failed to join cluster")
		}
		client := apipb.NewGraphServiceClient(conn)
		_, err = client.JoinCluster(ctx, &apipb.RaftNode{
			NodeId:  cfg.RaftID,
			Address: cfg.BindRaft,
		})
		if err != nil {
			return nil, errors.Wrap(err, "failed to join cluster")
		}
		s.closers = append(s.closers, func() {
			conn.Close()
		})
	}

	s.machine.Go(func(routine machine.Routine) {
		logger.Info("refreshing jwks")
		if err := s.config.RefreshKeys(); err != nil {
			logger.Error("failed to refresh keys", zap.Error(errors.WithStack(err)))
		}
	}, machine.GoWithMiddlewares(machine.Cron(time.NewTicker(1*time.Minute))))
	return s, nil
}

func (s *Runtime) execute(ctx context.Context, cmd *apipb.StateChange) (*apipb.StateChange, error) {
	if state := s.raft.State(); state != raft.Leader {
		return nil, fmt.Errorf("not leader: %s", state.String())
	}
	now := time.Now()
	//defer func() {
	//	logger.Info("executing raft mutation", zap.Int64("duration(ms)", time.Since(now).Milliseconds()))
	//}()
	bits, err := proto.Marshal(cmd)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	future := s.raft.Apply(bits, raftTimeout)
	if err := future.Error(); err != nil {
		return nil, err
	}
	logger.Info("raft mutation completed", zap.Int64("duration(ms)", time.Since(now).Milliseconds()))
	if err, ok := future.Response().(error); ok {
		return nil, err
	}
	return future.Response().(*apipb.StateChange), nil
}

func (s *Runtime) Close() error {
	s.machine.Cancel()
	for _, closer := range s.closers {
		closer()
	}
	defer s.machine.Close()
	if err := s.raft.Shutdown().Error(); err != nil {
		logger.Error("raft shutdown failure", zap.Error(err))
	}
	if err := s.graph.Close(); err != nil {
		logger.Error("graph storage shutdown failure", zap.Error(err))
	}
	if err := s.snapStore.Close(); err != nil {
		logger.Error("raft snapshot storage shutdown failure", zap.Error(err))
	}
	if err := s.logStore.Close(); err != nil {
		logger.Error("raft log storage shutdown failure", zap.Error(err))
	}
	return nil
}

func (s *Runtime) Machine() *machine.Machine {
	return s.machine
}

func (s *Runtime) Config() *auth.Config {
	return s.config
}

func (r *Runtime) Go(fn machine.Func) {
	r.machine.Go(fn)
}

func (r *Runtime) Cancel() {
	r.machine.Cancel()
}

func (r *Runtime) Wait() {
	r.machine.Wait()
}
