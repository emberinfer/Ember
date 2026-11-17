# Gateway executor operation

Permanent production configuration: **EMBER_PIPELINE_DAEMON=0**.
The resident pipeline mode is deprecated and disabled by default. Only an
explicit value of 1 selects the retained experimental path; do not remove the
production override as part of a library sync. Legacy per-request mode plus
streaming protocol readers and cancel-before-reap is the supported configuration.

Verdict (2026-11-17): daemon deprecated. A local real-client probe shows that Python
stdin iteration responds to a newline without EOF: a nonexistent local model
produces job_end(ok=false), and a second job does likewise. This disproves a
universal stdin-iteration buffering explanation on this runtime, but does not
explain the production zero-forward failure. There is no successful real-client
freeze -> stage-forward -> result -> job_end proof for that deployment. Fail-fast
probes and the synthetic Go lifecycle harness do not authorize enablement.

Legacy framing: write one JSON object, close stdin; read stage_start /
stage_end (or stage_failed), token, and finally result; EOF and process exit
terminate the request. Resident framing: write JSON plus newline and keep stdin
open; the same events precede job_end(ok=true). On exceptions job_end(ok=false)
replaces successful completion. The Go bridge consumes job_end instead of
forwarding it to the request event parser. A failed job, protocol/read error or
cancelled request kills/reaps the resident process and discards its pool slot.
The slot remains exclusive until the consumer acknowledges completion.

Readers accept arbitrarily large newline-terminated event lines; a truncated
final line is an error. Result lines still require memory proportional to their
size (including JSON/hex copies); this change is not a result-protocol redesign.
Malformed events cancel execution before bounded process reaping. A process
that closes stdout but stays alive is also killed after the completion grace.

Pipeline and MoE failures persist the last 4096 stderr bytes in mode-0600 files:
<executor WorkDir>/gateway-failures/<sanitized-request-id>-<kind>-*.stderr.
The directory is created with mode 0700. EMBER_GATEWAY_FAILURE_DIR overrides
it for both executors. Failure logs contain stderr_path; failed persistence is
reported as unavailable with the filesystem error. Filenames are unique and
request IDs cannot introduce path components. Operators should apply their
normal restricted-log retention policy to this directory. Experimental daemon
stderr remains resident process stderr, not a per-request tail; its failure
artifact may therefore be empty. Production legacy mode captures the tail.
