// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package worker

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/siddontang/go/sync2"
	"github.com/soheilhy/cmux"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/pingcap/dm/dm/common"
	"github.com/pingcap/dm/dm/config"
	"github.com/pingcap/dm/dm/pb"
	"github.com/pingcap/dm/pkg/log"
)

var (
	cmuxReadTimeout = 10 * time.Second
)

// Server accepts RPC requests
// dispatches requests to worker
// sends responses to RPC client
type Server struct {
	sync.Mutex
	wg     sync.WaitGroup
	closed sync2.AtomicBool

	cfg *Config

	rootLis net.Listener
	svr     *grpc.Server
	worker  *Worker
}

// NewServer creates a new Server
func NewServer(cfg *Config) *Server {
	s := Server{
		cfg: cfg,
	}
	s.closed.Set(true) // not start yet
	return &s
}

// Start starts to serving
func (s *Server) Start() error {
	var err error
	s.rootLis, err = net.Listen("tcp", s.cfg.WorkerAddr)
	if err != nil {
		return errors.Trace(err)
	}

	s.worker, err = NewWorker(s.cfg)
	if err != nil {
		return errors.Trace(err)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// start running worker to handle requests
		s.worker.Start()
	}()

	// create a cmux
	m := cmux.New(s.rootLis)
	m.SetReadTimeout(cmuxReadTimeout) // set a timeout, ref: https://github.com/pingcap/tidb-binlog/pull/352

	// match connections in order: first gRPC, then HTTP
	grpcL := m.Match(cmux.HTTP2HeaderField("content-type", "application/grpc"))
	httpL := m.Match(cmux.HTTP1Fast())

	s.svr = grpc.NewServer()
	pb.RegisterWorkerServer(s.svr, s)
	go func() {
		err2 := s.svr.Serve(grpcL)
		if err2 != nil && !common.IsErrNetClosing(err2) && err2 != cmux.ErrListenerClosed {
			log.L().Error("fail to start gRPC server", log.ShortError(err2))
		}
	}()
	go InitStatus(httpL) // serve status

	s.closed.Set(false)

	log.L().Info("start gRPC API", zap.String("listened address", s.cfg.WorkerAddr))
	err = m.Serve()
	if err != nil && common.IsErrNetClosing(err) {
		err = nil
	}
	return errors.Trace(err)
}

// Close close the RPC server
func (s *Server) Close() {
	s.Lock()
	defer s.Unlock()
	if s.closed.Get() {
		return
	}

	err := s.rootLis.Close()
	if err != nil && !common.IsErrNetClosing(err) {
		log.L().Error("fail to close net listener", log.ShortError(err))
	}
	if s.svr != nil {
		// GracefulStop can not cancel active stream RPCs
		// and the stream RPC may block on Recv or Send
		// so we use Stop instead to cancel all active RPCs
		s.svr.Stop()
	}

	// close worker and wait for return
	s.worker.Close()
	s.wg.Wait()

	s.closed.Set(true)
}

// StartSubTask implements WorkerServer.StartSubTask
func (s *Server) StartSubTask(ctx context.Context, req *pb.StartSubTaskRequest) (*pb.OperateSubTaskResponse, error) {
	log.L().Info("", zap.String("request", "start subtask"), zap.Stringer("payload", req))

	cfg := config.NewSubTaskConfig()
	err := cfg.Decode(req.Task)
	if err != nil {
		err = errors.Annotatef(err, "decode subtask config from request %+v", req.Task)
		log.L().Error("fail to decode task", zap.String("request", "start subtask"), zap.Stringer("payload", req), zap.Error(err))
		return nil, err
	}

	opLogID, err := s.worker.StartSubTask(cfg)
	if err != nil {
		err = errors.Annotatef(err, "start sub task %s", cfg.Name)
		log.L().Error("fail to start subtask", zap.String("request", "start subtask"), zap.Stringer("payload", req), zap.Error(err))
		return nil, err
	}

	return &pb.OperateSubTaskResponse{
		Meta: &pb.CommonWorkerResponse{
			Result: true,
			Msg:    "",
		},
		Op:    pb.TaskOp_Start,
		LogID: opLogID,
	}, nil
}

// OperateSubTask implements WorkerServer.OperateSubTask
func (s *Server) OperateSubTask(ctx context.Context, req *pb.OperateSubTaskRequest) (*pb.OperateSubTaskResponse, error) {
	log.L().Info("", zap.String("request", "operate subtask"), zap.Stringer("payload", req))
	opLogID, err := s.worker.OperateSubTask(req.Name, req.Op)
	if err != nil {
		err = errors.Annotatef(err, "operate(%s) sub task %s", req.Op.String(), req.Name)
		log.L().Error("fail to operate task", zap.String("request", "operate subtask"), zap.Stringer("payload", req), zap.Error(err))
		return nil, err
	}

	return &pb.OperateSubTaskResponse{
		Meta: &pb.CommonWorkerResponse{
			Result: true,
			Msg:    "",
		},
		Op:    req.Op,
		LogID: opLogID,
	}, nil
}

// UpdateSubTask implements WorkerServer.UpdateSubTask
func (s *Server) UpdateSubTask(ctx context.Context, req *pb.UpdateSubTaskRequest) (*pb.OperateSubTaskResponse, error) {
	log.L().Info("", zap.String("request", "update subtask"), zap.Stringer("payload", req))
	cfg := config.NewSubTaskConfig()
	err := cfg.Decode(req.Task)
	if err != nil {
		err = errors.Annotatef(err, "decode config from request %+v", req.Task)
		log.L().Error("fail to decode subtask", zap.String("request", "update subtask"), zap.Stringer("payload", req), zap.Error(err))
		return nil, err
	}

	opLogID, err := s.worker.UpdateSubTask(cfg)
	if err != nil {
		err = errors.Annotatef(err, "update sub task %s", cfg.Name)
		log.L().Error("fail to update task", zap.String("request", "update subtask"), zap.Stringer("payload", req), zap.Error(err))
		return nil, err
	}

	return &pb.OperateSubTaskResponse{
		Meta: &pb.CommonWorkerResponse{
			Result: true,
			Msg:    "",
		},
		Op:    pb.TaskOp_Update,
		LogID: opLogID,
	}, nil
}

// QueryTaskOperation implements WorkerServer.QueryTaskOperation
func (s *Server) QueryTaskOperation(ctx context.Context, req *pb.QueryTaskOperationRequest) (*pb.QueryTaskOperationResponse, error) {
	log.L().Info("", zap.String("request", "query subtask"), zap.Stringer("payload", req))

	taskName := req.Name
	opLogID := req.LogID

	opLog, err := s.worker.meta.GetTaskLog(opLogID)
	if err != nil {
		err = errors.Annotatef(err, "fail to get operation %d of task %s", opLogID, taskName)
		log.L().Error(err.Error())
		return nil, err
	}

	return &pb.QueryTaskOperationResponse{
		Log: opLog,
		Meta: &pb.CommonWorkerResponse{
			Result: true,
			Msg:    "",
		},
	}, nil
}

// QueryStatus implements WorkerServer.QueryStatus
func (s *Server) QueryStatus(ctx context.Context, req *pb.QueryStatusRequest) (*pb.QueryStatusResponse, error) {
	log.L().Info("", zap.String("request", "query status"), zap.Stringer("payload", req))

	resp := &pb.QueryStatusResponse{
		Result:        true,
		SubTaskStatus: s.worker.QueryStatus(req.Name),
		RelayStatus:   s.worker.relayHolder.Status(),
	}

	if len(resp.SubTaskStatus) == 0 {
		resp.Msg = "no sub task started"
	}
	return resp, nil
}

// QueryError implements WorkerServer.QueryError
func (s *Server) QueryError(ctx context.Context, req *pb.QueryErrorRequest) (*pb.QueryErrorResponse, error) {
	log.L().Info("", zap.String("request", "query error"), zap.Stringer("payload", req))

	resp := &pb.QueryErrorResponse{
		Result:       true,
		SubTaskError: s.worker.QueryError(req.Name),
		RelayError:   s.worker.relayHolder.Error(),
	}

	return resp, nil
}

// FetchDDLInfo implements WorkerServer.FetchDDLInfo
// we do ping-pong send-receive on stream for DDL (lock) info
// if error occurred in Send / Recv, just retry in client
func (s *Server) FetchDDLInfo(stream pb.Worker_FetchDDLInfoServer) error {
	log.L().Info("", zap.String("request", "fetch ddl info"))
	var ddlInfo *pb.DDLInfo
	for {
		// try fetch pending to sync DDL info from worker
		ddlInfo = s.worker.FetchDDLInfo(stream.Context())
		if ddlInfo == nil {
			return nil // worker closed or context canceled
		}
		log.L().Info("", zap.String("request", "fetch ddl info"), zap.Stringer("ddl info", ddlInfo))
		// send DDLInfo to dm-master
		err := stream.Send(ddlInfo)
		if err != nil {
			log.L().Error("fail to send DDLInfo to RPC stream", zap.String("request", "fetch ddl info"), zap.Stringer("ddl info", ddlInfo), log.ShortError(err))
			return err
		}

		// receive DDLLockInfo from dm-master
		in, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			log.L().Error("fail to receive DDLLockInfo from RPC stream", zap.String("request", "fetch ddl info"), zap.Stringer("ddl info", ddlInfo), log.ShortError(err))
			return err
		}
		log.L().Info("receive DDLLockInfo", zap.String("request", "fetch ddl info"), zap.Stringer("ddl lock info", in), log.ShortError(err))

		//ddlInfo = nil // clear and protect to put it back

		err = s.worker.RecordDDLLockInfo(in)
		if err != nil {
			// if error occurred when recording DDLLockInfo, log an error
			// user can handle this case using dmctl
			log.L().Error("fail to record DDLLockInfo", zap.String("request", "fetch ddl info"), zap.Stringer("ddl lock info", in), zap.Error(err))
		}
	}
}

// ExecuteDDL implements WorkerServer.ExecuteDDL
func (s *Server) ExecuteDDL(ctx context.Context, req *pb.ExecDDLRequest) (*pb.CommonWorkerResponse, error) {
	log.L().Info("", zap.String("request", "execute ddl"), zap.Stringer("payload", req))

	err := s.worker.ExecuteDDL(ctx, req)
	if err != nil {
		log.L().Error("fail to execute ddl", zap.String("request", "execute ddl"), zap.Stringer("payload", req), zap.Error(err))
	}
	return makeCommonWorkerResponse(err), nil
}

// BreakDDLLock implements WorkerServer.BreakDDLLock
func (s *Server) BreakDDLLock(ctx context.Context, req *pb.BreakDDLLockRequest) (*pb.CommonWorkerResponse, error) {
	log.L().Info("", zap.String("request", "break ddl lock"), zap.Stringer("payload", req))

	err := s.worker.BreakDDLLock(ctx, req)
	if err != nil {
		log.L().Error("fail to break ddl lock", zap.String("request", "break ddl lock"), zap.Stringer("payload", req), zap.Error(err))
	}
	return makeCommonWorkerResponse(err), nil
}

// HandleSQLs implements WorkerServer.HandleSQLs
func (s *Server) HandleSQLs(ctx context.Context, req *pb.HandleSubTaskSQLsRequest) (*pb.CommonWorkerResponse, error) {
	log.L().Info("", zap.String("request", "handle sqls"), zap.Stringer("payload", req))

	err := s.worker.HandleSQLs(ctx, req)
	if err != nil {
		log.L().Error("fail to handle sqls", zap.String("request", "handle sqls"), zap.Stringer("payload", req), zap.Error(err))
	}
	return makeCommonWorkerResponse(err), nil
}

// SwitchRelayMaster implements WorkerServer.SwitchRelayMaster
func (s *Server) SwitchRelayMaster(ctx context.Context, req *pb.SwitchRelayMasterRequest) (*pb.CommonWorkerResponse, error) {
	log.L().Info("", zap.String("request", "switch relay master"), zap.Stringer("payload", req))

	err := s.worker.SwitchRelayMaster(ctx, req)
	if err != nil {
		log.L().Error("fail to switch relay master", zap.String("request", "switch relay master"), zap.Stringer("payload", req), zap.Error(err))
	}
	return makeCommonWorkerResponse(err), nil
}

// OperateRelay implements WorkerServer.OperateRelay
func (s *Server) OperateRelay(ctx context.Context, req *pb.OperateRelayRequest) (*pb.OperateRelayResponse, error) {
	log.L().Info("", zap.String("request", "operate relay"), zap.Stringer("payload", req))

	resp := &pb.OperateRelayResponse{
		Op:     req.Op,
		Result: false,
	}

	err := s.worker.OperateRelay(ctx, req)
	if err != nil {
		log.L().Error("fail to operate relay", zap.String("request", "operate relay"), zap.Stringer("payload", req), zap.Error(err))
		resp.Msg = errors.ErrorStack(err)
		return resp, nil
	}

	resp.Result = true
	return resp, nil
}

// PurgeRelay implements WorkerServer.PurgeRelay
func (s *Server) PurgeRelay(ctx context.Context, req *pb.PurgeRelayRequest) (*pb.CommonWorkerResponse, error) {
	log.L().Info("", zap.String("request", "purge relay"), zap.Stringer("payload", req))

	err := s.worker.PurgeRelay(ctx, req)
	if err != nil {
		log.L().Error("fail to purge relay", zap.String("request", "purge relay"), zap.Stringer("payload", req), zap.Error(err))
	}
	return makeCommonWorkerResponse(err), nil
}

// UpdateRelayConfig updates config for relay and (dm-worker)
func (s *Server) UpdateRelayConfig(ctx context.Context, req *pb.UpdateRelayRequest) (*pb.CommonWorkerResponse, error) {
	log.L().Info("", zap.String("request", "update relay config"), zap.Stringer("payload", req))

	err := s.worker.UpdateRelayConfig(ctx, req.Content)
	if err != nil {
		log.L().Error("fail to update relay config", zap.String("request", "update relay config"), zap.Stringer("payload", req), zap.Error(err))
	}
	return makeCommonWorkerResponse(err), nil
}

// QueryWorkerConfig return worker config
// worker config is defined in worker directory now,
// to avoid circular import, we only return db config
func (s *Server) QueryWorkerConfig(ctx context.Context, req *pb.QueryWorkerConfigRequest) (*pb.QueryWorkerConfigResponse, error) {
	log.L().Info("", zap.String("request", "query worker config"), zap.Stringer("payload", req))

	resp := &pb.QueryWorkerConfigResponse{
		Result: true,
	}

	workerCfg, err := s.worker.QueryConfig(ctx)
	if err != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(err)
		log.L().Error("fail to query worker config", zap.String("request", "query worker config"), zap.Stringer("payload", req), zap.Error(err))
		return resp, nil
	}

	rawConfig, err := workerCfg.From.Toml()
	if err != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(err)
		log.L().Error("fail to marshal worker config", zap.String("request", "query worker config"), zap.Stringer("worker from config", &workerCfg.From), zap.Error(err))
	}

	resp.Content = rawConfig
	resp.SourceID = workerCfg.SourceID
	return resp, nil
}

// MigrateRelay migrate relay to original binlog pos
func (s *Server) MigrateRelay(ctx context.Context, req *pb.MigrateRelayRequest) (*pb.CommonWorkerResponse, error) {
	log.L().Info("", zap.String("request", "migrate relay"), zap.Stringer("payload", req))

	err := s.worker.MigrateRelay(ctx, req.BinlogName, req.BinlogPos)
	if err != nil {
		log.L().Error("fail to migrate relay", zap.String("request", "migrate relay"), zap.Stringer("payload", req), zap.Error(err))
	}
	return makeCommonWorkerResponse(err), nil
}

func makeCommonWorkerResponse(reqErr error) *pb.CommonWorkerResponse {
	resp := &pb.CommonWorkerResponse{
		Result: true,
	}
	if reqErr != nil {
		resp.Result = false
		resp.Msg = errors.ErrorStack(reqErr)
	}
	return resp
}
