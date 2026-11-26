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
// change nothing. The `retryable` flag is about retrying, not about effect
// (detach_aborted is retryable and applied nothing; outcome_unknown is
// retryable and may have applied), so it decides nothing here.
var chunkRefusals = map[string]bool{
	// core/host/volume/chunk_control.go chunkErrorCode, ErrDetachAborted:
	// "nothing was published and serving resumed here".
	"detach_aborted": true,
	// cmd/sw-block/chunk.go: bad usage or a local start check, answered
	// before the operation is sent (chunkFail with VolumeStatusExitInvalid).
	"bad_request": true,
}

// chunkOutcome maps an `sw-block ops chunk` exit and its JSON error body to
// an outcome: exit 0 ok; a code in chunkRefusals refused; every other answer,
// outcome_unknown included whatever its flag says, unknown, for a read of the
// state to decide.
func chunkOutcome(stdout string, code int) (string, string) {
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

