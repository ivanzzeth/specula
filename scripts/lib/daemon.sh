# shellcheck shell=bash
#
# Shared helpers for scripts that start a specula daemon and test against it.
# Source it:  . "$(dirname "${BASH_SOURCE[0]}")/lib/daemon.sh"
#
# ─────────────────────────────────────────────────────────────────────────────
# Why this file exists
#
# A backgrounded daemon that loses its bind is invisible to `set -e`. If the suite then
# proceeds, it grades WHATEVER ELSE is listening on that port and reports a pass for a
# binary it never touched — and can write test data into somebody else's database. This has
# already happened once here, to the OCI conformance gate.
#
# The first fix was free ports plus a "wait until healthz answers, and check the process is
# still alive" loop. That is NOT sufficient, and this was demonstrated, not theorised:
# running realclient-npm.sh with DATA_PORT forced to a port held by another instance, the
# script sailed through both checks and ran `npm install` against the wrong server.
#
# Both checks fail for the same reason — they are races, not identity checks:
#
#   * healthz on a taken port is answered INSTANTLY by the squatter, so the loop breaks on
#     its first iteration;
#   * our daemon needs ~125 ms to detect the bind failure and exit, so `kill -0` inside that
#     window still says "alive".
#
# The entire check therefore completes before the daemon has finished dying, and passes.
# No amount of retrying or sleeping repairs this: liveness and health cannot distinguish OUR
# server from a stranger's. Only identity can. wait_for_daemon below asks the kernel who
# actually owns the listening socket and refuses to continue unless the answer is our PID.
# ─────────────────────────────────────────────────────────────────────────────

# pick_free_port: ask the kernel for an unused TCP port (bind :0, read it back, release).
#
# Inherently racy — the port could be taken between release and use — but combined with the
# ownership assertion in wait_for_daemon that race is detected rather than silently
# mis-graded, which is the property that matters.
pick_free_port() {
  python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
}

# port_listener_pid <port> → PID currently LISTENing on <port>, or empty.
# ss reports the owning PID for our own processes, which is all we need.
port_listener_pid() {
  local port="$1"
  command -v ss >/dev/null 2>&1 || return 0
  # Under `set -o pipefail`, a non-matching grep would otherwise fail the pipeline
  # and abort the caller on the first empty-listen poll (before the daemon binds).
  ss -ltnHp "sport = :${port}" 2>/dev/null | grep -o 'pid=[0-9]*' | head -1 | cut -d= -f2 || true
}

# wait_for_daemon <pid> <port> <health_url> <logfile>
#
# Returns 0 only once ALL THREE hold:
#   1. the process is alive,
#   2. the listening socket on <port> is owned by <pid>  ← the check that actually matters,
#   3. <health_url> answers.
# Any foreign owner is a hard, immediate failure: never fall back to "well, something
# answered".
wait_for_daemon() {
  local pid="$1" port="$2" url="$3" log="$4"
  local i owner have_ss=1

  if ! command -v ss >/dev/null 2>&1; then
    have_ss=0
    echo "WARNING: 'ss' not found (iproute2) — cannot verify the listening socket belongs" >&2
    echo "         to our daemon. Falling back to liveness+health, which CANNOT tell our" >&2
    echo "         server apart from another process squatting on :${port}." >&2
  fi

  for i in $(seq 1 100); do
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "FATAL: specula (pid $pid) exited during startup. Log:" >&2
      cat "$log" >&2 2>/dev/null || true
      return 1
    fi

    if [[ "$have_ss" -eq 1 ]]; then
      owner="$(port_listener_pid "$port")"
      if [[ -n "$owner" && "$owner" != "$pid" ]]; then
        echo "FATAL: :${port} is held by pid ${owner}, NOT our daemon (pid ${pid})." >&2
        echo "       Refusing to continue — the suite would have graded the wrong server." >&2
        echo "       Our daemon's log:" >&2
        cat "$log" >&2 2>/dev/null || true
        return 1
      fi
      # Socket not bound yet: keep waiting rather than probing health, so we never accept
      # an answer before we know who is answering.
      [[ -z "$owner" ]] && { sleep 0.2; continue; }
    fi

    if curl -fsS --max-time 1 "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done

  echo "FATAL: specula never became healthy on :${port} (${url}). Log:" >&2
  cat "$log" >&2 2>/dev/null || true
  return 1
}
