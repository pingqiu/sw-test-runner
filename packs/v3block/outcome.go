package v3block

// The outcome of one operator-surface call: ok, refused or unknown. Each
// mapping decides from an exit code and the command's own answer, never from
// a log line. "refused" means nothing changed, so it is used only where the
// product states that; anything else that is not ok is unknown, for a read
// of the state to decide.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// v3ctlAnswer is v3ctl's JSON answer.
type v3ctlAnswer struct {
	Outcome    string `json:"outcome"`
	Volume     string `json:"volume"`
	LayoutID   string `json:"layout_id"`
	StoreReady bool   `json:"store_ready"`
	CallNs     int64  `json:"call_ns"`
	Error      string `json:"error"`
}

// parseV3ctl maps v3ctl's exit code and answer to an outcome. The exit code
// and the answer must agree; anything else is unknown.
func parseV3ctl(stdout string, code int, volume string) (v3ctlAnswer, string) {
	var a v3ctlAnswer
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &a); err != nil || a.Volume != volume {
		return a, "unknown"
	}
	want, known := map[string]int{"ok": 0, "refused": 1, "unknown": 3}[a.Outcome]
	if !known || want != code || (a.Outcome == "ok" && len(a.LayoutID) != 32) {
		return a, "unknown"
	}
	return a, a.Outcome
}

// chunkRefusals are the `sw-block ops chunk` error codes the product states
// change nothing, whatever the operation. The `retryable` flag is about
// retrying, not about effect (detach_aborted is retryable and applied
// nothing; outcome_unknown is retryable and may have applied), so it decides
// nothing here.
var chunkRefusals = map[string]bool{
	// core/host/volume/chunk_control.go chunkErrorCode, ErrDetachAborted:
	// "nothing was published and serving resumed here".
	"detach_aborted": true,
	// cmd/sw-block/chunk.go: bad usage or a local start check, answered
	// before the operation is sent (chunkFail with VolumeStatusExitInvalid).
	"bad_request": true,
}

// The operations whose answers chunkOutcome maps, as the product routes them.
const (
	chunkDetach         = "detach"                 // holder POST /chunk/detach
	chunkSnapshot       = "snapshot"               // holder POST /chunk/snapshot
	chunkSnapDelHolder  = "snapshot_delete.holder" // holder POST /chunk/snapshot/delete
	chunkSnapDelStore   = "snapshot_delete.store"  // ops chunk snapshot-delete, store direct
	chunkRestore        = "restore"                // ops chunk restore, master CreateVolume
	chunkRelease        = "release"                // ops chunk release, store direct
	chunkVolumeDelete   = "delete"                 // ops chunk volume-delete
	refusedBeforeChange = "workspace: deletion refused before persistence"
)

// opRefusals are the codes refused for one operation only: the same code
// comes from other operations, or from other paths of this one, after a
// state change, and is unknown there. A rule sees the error message; nil
// means every answer with the code. Product paths are on branch m1-product.
//
// Kept unknown on purpose (each can follow a change):
//   - snapshot: every refusal. The name rides on a cut that commits first,
//     unnamed, and advances HEAD (core/frontend/durable/
//     provider_layout_snapshot.go:56-66; the eleventh snapshot is
//     layout.ErrTooManySnapshots there, answered as internal).
//   - delete: snapshot_has_dependents, conflict, volume_deleting, and
//     not_admitted without the reservation sentinel. layout.DeleteVolume
//     commits the master's delete reservation first (core/frontend/
//     chunkstore/layout/delete.go:173-177; cmd/sw-block/
//     chunk_volume_delete.go:21-25: "A refused delete leaves the volume
//     reserved"), then RunMarkDeleting refuses (delete.go:60-71, 134-135).
//   - restore: not_admitted. FailedPrecondition is also answered after the
//     target's record is created (core/host/master/services.go:147-153,
//     169-198: restore target ErrForeignSource / ErrVolumeDeleting /
//     ErrExists), and the target name stays bound.
//   - release: conflict. A target without HEAD gets its tombstone first;
//     only then can the source's update lose (layout/delete.go:243-259).
//   - snapshot_exists, snapshot_deleting: no product code on m1-product
//     emits them (only core/frontend/chunkstore/control_types.go:44,46
//     defines them), so there is no path to show they change nothing.
var opRefusals = map[string]map[string]func(message string) bool{
	chunkSnapDelHolder: {
		// core/host/volume/chunk_control.go:307-308 maps ErrHasDependents,
		// ErrVolumeDeleting, ErrDetached and ErrIdentity to layout_refused.
		// On this route (chunk_control.go:529-546, 719-726) they come from
		// catalogVolume (:643-646), Head.DeleteSnapshot (layout/
		// snapshots.go:26-36) or UpdateHead's identity check (core/frontend/
		// chunkstore/layout_store.go:100-101), all before the one
		// conditional update (layout/head.go:519-535) writes.
		"layout_refused": nil,
	},
	chunkSnapDelStore: {
		// Head.DeleteSnapshot refuses before runTransition updates
		// (layout/snapshots.go:27-35, layout/head.go:527-529); codes from
		// cmd/sw-block/chunk_b.go:333,337.
		"snapshot_has_dependents": nil,
		"volume_deleting":         nil,
		// The one conditional update lost (chunk_b.go:343): the filer
		// refused the expected generation, so this call wrote nothing
		// (layout/head.go:534; layout_store.go:115-117; core/frontend/
		// chunkstore/swfs.go:124-134: FailedPrecondition is the definite
		// answer, anything else is returned unwrapped).
		"conflict": nil,
	},
	chunkRestore: {
		// gRPC AlreadyExists (cmd/sw-block/chunk_b.go:279-280) comes from
		// services.go:89-91 before any write, or from lifecycleError on
		// lifecycle.ErrVolumeConflict (services.go:158, 520-521): store.go
		// :140-148 and :175-176 return before putLocked; :218-219 cannot
		// differ from the record CreateVolume just wrote, as every writer
		// of the records holds workspaceMu (services.go:68-69, 243-244).
		"target_exists": nil,
	},
	chunkRelease: {
		// OperatorReleaseDependent refuses a live target before writing the
		// source (layout/delete.go:250-257), or when the tombstone's
		// exclusive create found a HEAD, so wrote nothing (:243-247);
		// code from chunk_b.go:340.
		"target_live": nil,
	},
	chunkVolumeDelete: {
		// Only the reservation's own refusal changes nothing: the
		// publisher refuses while a lease is live before calling persist
		// (core/authority/workspace_delete_reservation.go:12-14, 52-60),
		// the master answers FailedPrecondition for that sentinel alone
		// (core/host/master/volume_delete_authority.go:74-78), the CLI
		// wraps it as ErrNotAdmitted (chunk_volume_delete.go:85-90) and
		// answers not_admitted with err.Error() as the message (chunk_b.go
		// :339; cmd/sw-block/chunk.go:310-311). Reserve runs first
		// (layout/delete.go:173-175). The same code after the reservation
		// committed (RunMarkDeleting, delete.go:134-135) carries no such
		// text, and a rerun after a commit replays the reservation
		// (workspace_delete_reservation.go:49-51), so it never carries it.
		// The product has no separate code for this refusal; the sentinel
		// is its error text.
		"not_admitted": func(m string) bool { return strings.Contains(m, refusedBeforeChange) },
	},
}

// chunkOutcome maps one operation's `sw-block ops chunk` exit (or holder
// route answer) and its JSON error body to an outcome: exit 0 ok; a code in
// chunkRefusals, or in the operation's opRefusals whose rule holds, refused;
// every other answer, outcome_unknown included whatever its flag says,
// unknown, for a read of the state to decide.
func chunkOutcome(op, stdout string, code int) (string, string) {
	if code == 0 {
		return "ok", ""
	}
	var e struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &e) != nil || e.Code == "" {
		return "unknown", fmt.Sprintf("exit %d without an error body", code)
	}
	if chunkRefusals[e.Code] {
		return "refused", e.Code + ": " + e.Message
	}
	if rule, ok := opRefusals[op][e.Code]; ok && (rule == nil || rule(e.Message)) {
		return "refused", e.Code + ": " + e.Message
	}
	return "unknown", e.Code + ": " + e.Message
}

// wsOutcome maps `sw-block ops workspace` exit codes: 0 ok; 1, 2, 5 refused;
// 3 and 4 (and anything else) unknown.
func wsOutcome(code int) string {
	switch code {
	case 0:
		return "ok"
	case 1, 2, 5:
		return "refused"
	}
	return "unknown"
}
