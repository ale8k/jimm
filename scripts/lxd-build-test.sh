#!/bin/sh
# lxd-build-test.sh - run JIMM tests in a clean LXD environment

set -eu

image=${image:-ubuntu:16.04}
container=${container:-jimm-test-`uuidgen`}
packages="build-essential bzr git make mongodb"

lxc launch -e ubuntu:16.04 $container
trap "lxc delete --force $container" EXIT

lxc exec $container -- sh -c 'while [ ! -f /var/lib/cloud/instance/boot-finished ]; do sleep 0.1; done'

# Configure the http_proxy for snapd
cat << EOF | lxc exec $container -- tee /etc/environment > /dev/null
http_proxy=${http_proxy:-}
https_proxy=${https_proxy:-${http_proxy:-}}
no_proxy=${no_proxy:-}
EOF
lxc exec $container -- systemctl daemon-reload
lxc exec $container -- systemctl restart snapd
lxc exec $container -- snap set system proxy.http=${http_proxy:-}
lxc exec $container -- snap set system proxy.https=${https_proxy:-${http_proxy:-}}

lxc exec --env http_proxy=${http_proxy:-} --env no_proxy=${no_proxy:-} $container -- apt-get update -y
lxc exec --env http_proxy=${http_proxy:-} --env no_proxy=${no_proxy:-} $container -- apt-get install -y $packages
lxc exec $container -- snap install go --classic

lxc file push --uid 1000 --gid 1000 --mode 600 ${NETRC:-$HOME/.netrc} $container/home/ubuntu/.netrc
lxc exec --cwd /home/ubuntu/ --user 1000 --group 1000 $container -- mkdir -p /home/ubuntu/src
tar c . | lxc exec --cwd /home/ubuntu/src/ --user 1000 --group 1000 $container -- tar x
lxc exec \
	--env HOME=/home/ubuntu \
	--env http_proxy=${http_proxy:-} \
	--env no_proxy=${no_proxy:-} \
	--cwd /home/ubuntu/src/ \
	--user 1000 \
	--group 1000 \
	$container -- go mod download

if [ -n "${juju_version:-}" ]; then
	lxc exec \
		--env HOME=/home/ubuntu \
		--env http_proxy=${http_proxy:-} \
		--env no_proxy=${no_proxy:-} \
		--cwd /home/ubuntu/src/ \
		--user 1000 \
		--group 1000 \
		$container -- go get github.com/juju/juju@$juju_version
fi

lxc exec \
	--env HOME=/home/ubuntu \
	--env http_proxy=${http_proxy:-} \
	--env no_proxy=${no_proxy:-} \
	--cwd /home/ubuntu/src/ \
	--user 1000 \
	--group 1000 \
	$container -- make check