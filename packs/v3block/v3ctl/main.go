//go:build ignore

// v3ctl makes the two blockmaster calls the sw-block CLI has no command for,
// with the CLI's own dial (mutual TLS from SW_BLOCK_TLS_* when set). It is
// built inside the product checkout under test (its go.mod and module cache),
// so the runner module never imports the product:
//
//	cp v3ctl/main.go <product>/.runner-v3ctl/main.go
//	(cd <product> && go build -o <bin>/v3ctl ./.runner-v3ctl)
//
//	v3ctl create  --master ADDR --volume V --size BYTES   one CreateVolume call
//	v3ctl resolve --master ADDR --volume V                the master's layout id
//
// Each prints one JSON object on stdout. Exit 0 ok; 1 refused (the master
// answered with a definite refusal); 3 unknown (unavailable, deadline, or an
// answer that does not confirm the request); 2 usage.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/seaweedfs/seaweed-block/core/rpc/control"
	"github.com/seaweedfs/seaweed-block/core/rpc/link"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type answer struct {
	Outcome    string `json:"outcome"` // ok | refused | unknown
	Volume     string `json:"volume"`
	LayoutID   string `json:"layout_id,omitempty"`
	StoreReady bool   `json:"store_ready,omitempty"`
	CallNs     int64  `json:"call_ns"` // the RPC alone, monotonic, this host
	Code       string `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: v3ctl create|resolve --master ADDR --volume V [--size BYTES]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("v3ctl "+os.Args[1], flag.ContinueOnError)
	master := fs.String("master", "", "blockmaster gRPC address")
	volume := fs.String("volume", "", "volume name")
	size := fs.Uint64("size", 0, "create: size in bytes")
	timeout := fs.Duration("timeout", 60*time.Second, "bound for the call")
	if fs.Parse(os.Args[2:]) != nil || *master == "" || *volume == "" || fs.NArg() != 0 {
		os.Exit(2)
	}
	conn, err := link.DialMasterFromEnv(*master)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer conn.Close()
	lc := control.NewLifecycleServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	a := answer{Volume: *volume}
	start := time.Now()
	switch os.Args[1] {
	case "create":
		if *size == 0 {
			os.Exit(2)
		}
		var r *control.CreateVolumeResponse
		r, err = lc.CreateVolume(ctx, &control.CreateVolumeRequest{VolumeId: *volume, SizeBytes: *size, ReplicationFactor: 1, Protocol: "nvme"})
		a.CallNs = time.Since(start).Nanoseconds()
		if err == nil {
			a.LayoutID, a.StoreReady = r.GetLayoutId(), r.GetStoreReady()
			if r.GetVolumeId() != *volume || !r.GetStoreReady() || len(a.LayoutID) != 32 {
				a.Outcome, a.Error = "unknown", "the answer does not confirm a store-ready volume of this name"
			}
		}
	case "resolve":
		var r *control.CreateVolumeResponse // GetVolume answers with the create record
		r, err = lc.GetVolume(ctx, &control.GetLifecycleVolumeRequest{VolumeId: *volume})
		a.CallNs = time.Since(start).Nanoseconds()
		if err == nil {
			a.LayoutID = r.GetLayoutId()
			if r.GetVolumeId() != *volume || len(a.LayoutID) != 32 {
				a.Outcome, a.Error = "unknown", "the master did not resolve this name to a layout id"
			}
		}
	default:
		os.Exit(2)
	}
	exit := 0
	switch {
	case err != nil:
		a.Code, a.Error = status.Code(err).String(), err.Error()
		switch status.Code(err) {
		case codes.InvalidArgument, codes.FailedPrecondition, codes.AlreadyExists, codes.NotFound, codes.PermissionDenied, codes.Unauthenticated:
			a.Outcome, exit = "refused", 1
		default:
			a.Outcome, exit = "unknown", 3
		}
	case a.Outcome == "unknown":
		exit = 3
	default:
		a.Outcome = "ok"
	}
	_ = json.NewEncoder(os.Stdout).Encode(a)
	os.Exit(exit)
}
