// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Tries to use the guidelines at https://cloud.google.com/apis/design for the gRPC API where possible.
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/spf13/viper"

	gw "github.com/googleforgames/open-match2/v2/pkg/api"
)

func unaryPanicRecoveryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			var fullMethod string
			if info != nil {
				fullMethod = info.FullMethod
			}
			logger.Errorf("Recovered from panic in unary gRPC handler %s: %v\n%s", fullMethod, r, string(debug.Stack()))
			err = status.Error(codes.Internal, "internal server error")
		}
	}()
	return handler(ctx, req)
}

func streamPanicRecoveryInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			var fullMethod string
			if info != nil {
				fullMethod = info.FullMethod
			}
			logger.Errorf("Recovered from panic in stream gRPC handler %s: %v\n%s", fullMethod, r, string(debug.Stack()))
			err = status.Error(codes.Internal, "internal server error")
		}
	}()
	return handler(srv, ss)
}

// start brings up the gRPC and HTTP gateway servers based on the configuration
// and shuts them down gracefully when parentCtx is cancelled or a termination signal arrives.
func start(parentCtx context.Context, cfg *viper.Viper) {
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	// SIGTERM is signaled by k8s/Cloud Run when it wants an instance to stop.
	// SIGINT is signaled when running locally and hitting Ctrl+C.
	ctx, stop := signal.NotifyContext(parentCtx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var wg sync.WaitGroup

	// gRPC server startup
	logger.Infof("Open Match Server starting unexposed gRPC server on port %v", cfg.GetString("OM_GRPC_PORT"))
	lis, err := net.Listen("tcp", ":"+cfg.GetString("OM_GRPC_PORT"))
	if err != nil {
		logger.Fatalf("Couldn't listen on port %v - net.Listen error: %v", cfg.GetString("OM_GRPC_PORT"), err)
	}
	var opts []grpc.ServerOption
	opts = append(opts,
		grpc.StatsHandler(&Handler{}),
		grpc.UnaryInterceptor(unaryPanicRecoveryInterceptor),
		grpc.StreamInterceptor(streamPanicRecoveryInterceptor),
	)
	omServer := grpc.NewServer(opts...)
	pb.RegisterOpenMatchServiceServer(omServer, &grpcServer{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		if serveErr := omServer.Serve(lis); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			logger.Fatal(serveErr)
		}
	}()
	logger.Debugf("Open Match Core gRPC server with OTEL instrumentation started")

	// http (grpc-gateway) server setup
	logger.Infof("Open Match Server starting HTTP gRPC proxy on port %v", cfg.GetString("PORT"))
	mux := runtime.NewServeMux()
	err = gw.RegisterOpenMatchServiceHandlerFromEndpoint(
		ctx,
		mux,
		"localhost:"+cfg.GetString("OM_GRPC_PORT"),
		[]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
	)
	if err != nil {
		logger.Fatal(err)
	}
	httpServer := &http.Server{
		Addr:              ":" + cfg.GetString("PORT"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if httpErr := httpServer.ListenAndServe(); httpErr != nil && !errors.Is(httpErr, http.ErrServerClosed) {
			logger.Fatalf("Failed to start http server: %s", httpErr)
		}
	}()
	logger.Debugf("Open Match Core http server started")

	// Wait for context cancellation or termination signal.
	<-ctx.Done()
	stop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if shutdownErr := httpServer.Shutdown(shutdownCtx); shutdownErr != nil {
		logger.Warnf("HTTP server shutdown error: %v", shutdownErr)
	}

	grpcDone := make(chan struct{})
	go func() {
		omServer.GracefulStop()
		close(grpcDone)
	}()
	select {
	case <-grpcDone:
	case <-shutdownCtx.Done():
		omServer.Stop()
		<-grpcDone
	}

	wg.Wait()
}

// Handler implements [stats.Handler](https://pkg.go.dev/google.golang.org/grpc/stats#Handler) interface.
type Handler struct{}

// TagConn can attach some information to the given context.
// The context used in HandleConn for this connection will be derived from the context returned.
// In the gRPC client:
// The context used in HandleRPC for RPCs on this connection will be the user's context and NOT derived from the context returned here.
// In the gRPC server:
// The context used in HandleRPC for RPCs on this connection will be derived from the context returned here.
func (st *Handler) TagConn(ctx context.Context, stat *stats.ConnTagInfo) context.Context {
	return ctx
}

// HandleConn processes the Conn stats.
func (st *Handler) HandleConn(ctx context.Context, stat stats.ConnStats) {
}

type rpcStatCtxKey struct{}

// TagRPC can attach some information to the given context.
func (st *Handler) TagRPC(ctx context.Context, stat *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, rpcStatCtxKey{}, stat)
}

// HandleRPC lets us track RPC stats without manually instrumenting each gRPC
// handler function in Open Match individually.  Note: All stat fields are
// read-only.  Some samples of how to use the stats handler HandleRPC method.
// https://stackoverflow.com/a/74586253 for accessing stuff from stats.RPCStats
// https://pkg.go.dev/google.golang.org/grpc/examples/features/stats_monitoring#section-readme
func (st *Handler) HandleRPC(ctx context.Context, stat stats.RPCStats) {

	// HandleRPC is called multiple times during a single request-response cycle,
	// and each time the stats.RPCStats argument holds a different data struct.
	// Test the type to figure out which of the calls this is, and log metrics
	// when it's a part of the cycle we are interested in.
	switch s := stat.(type) {
	//case *stats.InPayload:
	//	n := s.WireLength // Example.
	//	_ = n
	case *stats.End:
		// RPC name was stored in the context when the TagRPC handler was run (when
		// the RPC context was created), retrieve it here so it can be added as an
		// attribute to the metric entries.
		var rpcName string
		if ctxRpcTagInfo, ok := ctx.Value(rpcStatCtxKey{}).(*stats.RPCTagInfo); ok {
			rpcName = filepath.Base(ctxRpcTagInfo.FullMethodName)
		}
		rpcNameAttr := metric.WithAttributes(attribute.String("rpc", rpcName))

		// Record rpc duration in a histogram
		otelRpcDuration.Record(ctx,
			// Convert us into float ms
			float64(s.EndTime.Sub(s.BeginTime).Microseconds())/1000.0,
			// Add the name of this RPC to this measurement.
			rpcNameAttr,
		)

		// Track rpc errors
		if s.Error != nil {
			exitStatus, _ := status.FromError(s.Error)
			otelRpcErrors.Add(ctx, 1, rpcNameAttr, metric.WithAttributes(
				attribute.String("grpc.status", exitStatus.Code().String()),
			))
		}
	}
}

// New returns a new implementation of [stats.Handler](https://pkg.go.dev/google.golang.org/grpc/stats#Handler) interface.
func New() *Handler {
	return &Handler{}
}
