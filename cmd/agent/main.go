package main

import (
	"log"
	"net"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/cybericebox/laboratory/internal/agent/config"
	grpcserver "github.com/cybericebox/laboratory/internal/agent/grpc"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}
	cs, err := grpcserver.NewVersionedClientset(restCfg)
	if err != nil {
		log.Fatalf("versioned client: %v", err)
	}
	k8s, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Fatalf("core client: %v", err)
	}
	h := grpcserver.NewHandler(cs, k8s)
	srv, err := grpcserver.New(cfg, h)
	if err != nil {
		log.Fatalf("build server: %v", err)
	}
	lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("agent listening on port %s (mtls=%t)", cfg.GRPCPort, cfg.MTLS.Enabled)
	if err := srv.Serve(lis); err != nil {
		log.Printf("serve: %v", err)
		os.Exit(1)
	}
}
