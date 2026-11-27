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
// answered with a refusal it gives before any write: refusals below); 3
// unknown (any other error, a deadline, or an answer that does not confirm
// the request); 2 usage.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
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

// refusal is one master answer that v3ctl reports as refused: a gRPC code,
// and text the answer's message must contain ("" for any). Every other error
// is unknown. Each entry is a row of packs/v3block/product/outcomes.json
// (action "create" or "resolve", path "v3ctl create" or "v3ctl resolve");
// a runner test keeps the two equal. Citations are product file:line at
// seaweed-block 14dff69b0f7cc139f9bde7849dec6ab34063c947.
type refusal struct {
	code     codes.Code
	contains string
}

var refusals = map[string][]refusal{
	// CreateVolume (core/host/master/services.go:57). Its first write is
	// the lifecycle record, services.go:147 -> core/lifecycle/store.go
	// putLocked (:153, :182, :200).
	"create": {
		// The interceptor admits the call before the handler runs
		// (core/auth/authorize.go:141-145, handler at :151): a caller not
		// in the role file or without a verified certificate
		// (core/auth/identity.go:185-204).
		{codes.Unauthenticated, ""},
		// The same interceptor: CreateVolume needs operator
		// (core/host/master/authz.go:38; authorize.go:104-116, 58-60).
		{codes.PermissionDenied, ""},
		// lifecycleError on ErrInvalidVolumeSpec (services.go:158,
		// 516-518), which FileStore.CreateVolume returns only before its
		// putLocked (store.go:144-145, 162-166, 179, 192); services.go:95,
		// 130 are restore only, before :147. A layout create error is
		// wrapped as not ready first (services.go:155-156, 194-196).
		{codes.InvalidArgument, ""},
		// services.go:89-91 before any write, or lifecycleError on
		// ErrVolumeConflict (services.go:158, 520-521): store.go:140-148
		// and :175-176 return before putLocked; :218-219 cannot differ
		// from the record :169 just wrote, as every writer of the records
		// holds workspaceMu (services.go:68-69, 243-244, 353-354).
		{codes.AlreadyExists, ""},
		// FailedPrecondition only for a reserved name (services.go:70-71,
		// under workspaceMu, before :147). The same code follows the
		// record write at :152-153 (layout.ErrExists / ErrVolumeDeleting
		// from the layout create) and at :203, so any other text is
		// unknown.
		{codes.FailedPrecondition, "is being deleted or was deleted; its id is never reused"},
		// NotFound is unknown: for a create without a source the only path
		// that answers it, services.go:149-150, follows the record write
		// (:126 is restore only).
	},
	// GetVolume (services.go:224-240) writes nothing ("It never creates a
	// record", :221-222; lifecycleVolumeResponse :201-219 only reads), so
	// every answer it gives changed nothing.
	"resolve": {
		{codes.Unauthenticated, ""},    // authorize.go:141-145; identity.go:185-204
		{codes.PermissionDenied, ""},   // authorize.go:141-145; authz.go:19
		{codes.InvalidArgument, ""},    // services.go:228-229
		{codes.FailedPrecondition, ""}, // services.go:232-233, 203
		{codes.NotFound, ""},           // services.go:236-237
		{codes.AlreadyExists, ""},      // services.go:224-240 (writes nothing)
	},
}

// refused says whether the master's error is a refusal of cmd listed above.
func refused(cmd string, err error) bool {
	st := status.Convert(err)
	for _, r := range refusals[cmd] {
		if st.Code() == r.code && strings.Contains(st.Message(), r.contains) {
			return true
		}
	}
	return false
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
		if refused(os.Args[1], err) {
			a.Outcome, exit = "refused", 1
		} else {
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
