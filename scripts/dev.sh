#!/usr/bin/env bash
# Run the development backend and Vite together, and leave the terminal with nothing
# behind when it is interrupted.
#
# Why this is a script instead of two `&` lines in the recipe: Ctrl-C reaches the whole
# foreground process group, so `make` dies first and its recipe shell inherits the
# cleanup. The shell then has to stop two trees it did not create (`go run`'s child is the
# server; npm wraps Vite the same way), and one of them — a backend finishing an index
# settlement — deliberately outlives cancellation. A bare `wait` therefore blocked
# forever while the prompt had already returned, and the backend kept working in the
# background with no listener and no way to see it.
#
# So each half runs in its own process group, the trap stops the groups, and the shell
# exits only once both are reaped. A half that ignores TERM is KILLed after the grace
# period: an orphan that outlives its own terminal is the failure this exists to prevent,
# and the journal resumes whatever that kill cut.
set -u

root=$(cd "$(dirname "$0")/.." && pwd)
grace=${EAM_DEV_STOP_GRACE:-10}

set -m # job control: each background job becomes a process-group leader
( cd "$root/backend" && exec env APP_ENV="${APP_ENV:-development}" go run ./cmd/eam server ) &
backend=$!
( cd "$root/web" && exec npm run dev ) &
web=$!
set +m # groups are what matter; the notices on termination are noise here

echo "backend http://127.0.0.1:8080 · frontend http://127.0.0.1:5173 (Ctrl-C stops both)"

stopped=0
stop() {
	if [ "$stopped" = 1 ]; then
		# A second interrupt is the operator's escape hatch from a drain that is taking
		# its time; the backend documents the same rule for its own second signal.
		printf '\nkilling backend and frontend\n'
		kill -KILL -- "-$backend" "-$web" 2>/dev/null
		return
	fi
	stopped=1
	printf '\nstopping backend and frontend\n'
	kill -TERM -- "-$backend" "-$web" 2>/dev/null
	# A deadline, not a poll: wait(1) has no timeout, and this is the one place where
	# "the process is still there" is a fact to act on rather than a state to observe.
	( sleep "$grace" && kill -KILL -- "-$backend" "-$web" 2>/dev/null ) &
	killer=$!
	wait "$backend" 2>/dev/null
	wait "$web" 2>/dev/null
	kill "$killer" 2>/dev/null
	wait "$killer" 2>/dev/null
	echo "stopped"
}

trap 'stop; exit 130' INT TERM

# The backend is the half a developer is usually watching: when it stops on its own (a
# bind failure, a compile error, a crash), take the frontend down with it instead of
# leaving a proxy in front of nothing.
wait "$backend"
status=$?
if [ "$stopped" = 0 ]; then
	echo
	echo "backend exited; stopping the frontend"
	stop
fi
exit "$status"
