package randomrun

// The handler verb contract. The run loop and replay read a product's
// history through a few record shapes its handlers must write; the Driver
// names the actions, these name what their records say. contract_test.go
// pins them, and the block pack's tests check its handlers against them.
//
//   - A fault handler (Driver.Fault, Driver.OutageRestart) records an invoke
//     and its complete with verb VerbKill or VerbRestart, the event's action.
//     Replay ends the current step at such an invoke: a fault's records are
//     not the step's operation.
//   - The healthy handler (Driver.Healthy, Driver.Look) records one observe
//     with verb VerbHealthy (replay ends the current step there too) and
//     answers {"value": "ok", "healthy": "true"} at a healthy point,
//     {"value": "ok", "healthy": "false"} otherwise. Only "true" resolves.
//   - A single look (Driver.Look) is that record with result.look ==
//     LookSingle: one look, no wait, one record per look. It also answers
//     DownKey: the processes it found down, separated by spaces ("" for
//     none); the run takes one before every kill.
//   - An observation (Driver.Observe) answers no outcome and records one
//     observe with verb VerbObserve on its subject (the driver reads it back
//     to place a rerun's unknown, LastObservation); every other handler
//     answers {"value": "ok" | "refused" | "unknown"}.
const (
	VerbKill    = FaultKill
	VerbRestart = FaultRestart
	VerbHealthy = "healthy"
	VerbObserve = "observe"
	// VerbFaultApply and VerbFaultRestore are a store fault's records (a
	// pause and its continue): inside a step, never its operation.
	VerbFaultApply   = "fault_apply"
	VerbFaultRestore = "fault_restore"
	LookSingle  = "single"
	// HealthyKey is the healthy handler's answer key; HealthyTrue its value
	// at a healthy point.
	HealthyKey  = "healthy"
	HealthyTrue = "true"
	// DownKey is a single look's answer key for the processes found down.
	DownKey = "down"
)
