#!/usr/bin/sh
#
# Installs the AMCO node agent on an AIX node as the "amco-agent" SRC
# subsystem (group "kubernetes", alongside containerd and the kubelet) and
# starts it at boot from /etc/inittab. Run as root on the AIX node.
#
#   install-agent.sh -b ./amco-agent -k ./kubeconfig [-n NODE_NAME] [-- EXTRA_AGENT_ARGS]
#   install-agent.sh -u
#
#   -b  amco-agent binary built with `make agent-aix`
#   -k  kubeconfig for the amco-agent service account
#       (create it with hack/agent-kubeconfig.sh)
#   -n  Kubernetes node name (default: short hostname, lower case)
#   -u  uninstall (stops and removes the subsystem; files are left in place)
#
# Everything after "--" is appended to the agent arguments, e.g. "-- --v=4".
set -eu

BIN_DIR=/opt/amco/bin
BIN=$BIN_DIR/amco-agent
CONF_DIR=/etc/amco
KUBECONFIG_PATH=$CONF_DIR/kubeconfig
LOG=/var/log/amco-agent.log
SUBSYS=amco-agent
GROUP=kubernetes

binary=""
kubeconfig=""
node=$(hostname | cut -d. -f1 | tr '[:upper:]' '[:lower:]')
uninstall=0
while getopts "b:k:n:u" opt; do
	case $opt in
	b) binary=$OPTARG ;;
	k) kubeconfig=$OPTARG ;;
	n) node=$OPTARG ;;
	u) uninstall=1 ;;
	*) sed -n '3,17p' "$0"; exit 2 ;;
	esac
done
shift $((OPTIND - 1))
[ "${1:-}" = "--" ] && shift
extra="$*"

if [ "$(uname -s)" != "AIX" ]; then
	echo "this script must run on the AIX node" >&2
	exit 1
fi

if [ $uninstall -eq 1 ]; then
	stopsrc -s $SUBSYS >/dev/null 2>&1 || true
	rmitab $SUBSYS >/dev/null 2>&1 || true
	rmssys -s $SUBSYS >/dev/null 2>&1 || true
	echo "removed SRC subsystem $SUBSYS and its inittab entry ($BIN and $CONF_DIR left in place)"
	exit 0
fi

[ -n "$binary" ] && [ -n "$kubeconfig" ] || { echo "-b and -k are required" >&2; exit 2; }

mkdir -p $BIN_DIR $CONF_DIR
chmod 0700 $CONF_DIR
cp "$binary" $BIN.new
chmod 0755 $BIN.new
mv $BIN.new $BIN
cp "$kubeconfig" $KUBECONFIG_PATH
chmod 0600 $KUBECONFIG_PATH

args="run --node-name=$node --kubeconfig=$KUBECONFIG_PATH --v=2"
[ -n "$extra" ] && args="$args $extra"

# (Re)define the subsystem:
#   -S -n 15 -f 9   signal communication: SIGTERM to stop, SIGKILL to force
#   -w 300          give an apply in progress 5 minutes before SIGKILL
#   -R              restart the agent if it exits abnormally
#   -Q              single instance
lssrc -s $SUBSYS >/dev/null 2>&1 && { stopsrc -s $SUBSYS >/dev/null 2>&1 || true; rmssys -s $SUBSYS; }
mkssys -s $SUBSYS -G $GROUP -p $BIN -a "$args" -u 0 \
	-S -n 15 -f 9 -w 300 -R -Q \
	-i /dev/null -o $LOG -e $LOG

# Start at boot once TCP/IP is up (the agent needs the API server).
entry="$SUBSYS:2:once:/usr/bin/startsrc -s $SUBSYS >/dev/console 2>&1"
if lsitab $SUBSYS >/dev/null 2>&1; then
	chitab "$entry"
else
	mkitab -i rctcpip "$entry"
fi

startsrc -s $SUBSYS
lssrc -s $SUBSYS
echo "amco-agent installed; logs: $LOG"
