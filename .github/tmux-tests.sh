#!/bin/sh
# Runs the tests relying on tmux with the tmux of this container, on Debian or Fedora: their test
# binaries, built with go test -c, are in /tests, and the module is in /src, read-only. A failing
# package does not keep the other one from running.
set -eu

# Beyond tmux: the real less for TestCaptureFullScreen, ps for TestKillWindowSurvivors
if command -v apt-get >/dev/null; then
	apt-get update -qq
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq tmux less procps >/dev/null
else
	dnf install -y -q tmux less procps-ng util-linux shadow-utils
fi

# A user of their own: root skips the tests of what a user can not write. A copy: tests run from
# the directory of their package, and must not write into the module.
useradd -m tester
cp -R /src /home/tester/src
chown -R tester /home/tester/src

su tester -c sh <<'EOF'
set -u
tmux -V
bash --version | head -1
status=0
for package in tmux:tmux cmd/ratd:ratd; do
	cd "$HOME/src/${package%:*}" || exit 1
	# -short: TestMinimumTmux starts a container of its own
	"/tests/${package#*:}.test" -test.short -test.count=1 || status=1
done
exit "$status"
EOF
