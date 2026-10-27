// The two scripts the probe pods run. They are shell because what they run is
// iputils' arping and ping, and each writes one JSON object to the termination
// log. Inputs arrive in environment variables the builder has validated as
// interface names and IPv4 addresses.

package netprobe

// guestScript checks, from inside the pod attached to the declared networks,
// each interface's MTU, whether another host answers for each declared address
// (arping in duplicate address detection mode), and whether each peer answers a
// ping sent out of its interface.
//
// IFACES is a comma-separated list of interface names, and ARPING and PEERS are
// lists of name=address pairs. ARPING lists the pod's own address first, then the
// candidates, which are asked about and never attached.
const guestScript = `set -u
ifaces=""; dups=""; peers=""
IFS=,
for name in $IFACES; do
  mtu=$(cat "/sys/class/net/$name/mtu" 2>/dev/null || echo 0)
  ifaces="${ifaces:+$ifaces,}{\"name\":\"$name\",\"mtu\":$mtu}"
done
for pair in $ARPING; do
  name=${pair%%=*}; address=${pair#*=}
  out=$(arping -D -c 2 -w 3 -I "$name" "$address" 2>&1)
  replies=$(printf '%s\n' "$out" | sed -n 's/.*Received \([0-9][0-9]*\) response.*/\1/p' | head -n 1)
  dups="${dups:+$dups,}{\"interface\":\"$name\",\"address\":\"$address\",\"replies\":${replies:-0}}"
done
for pair in $PEERS; do
  name=${pair%%=*}; peer=${pair#*=}
  if ping -c 2 -W 2 -I "$name" "$peer" >/dev/null 2>&1; then ok=true; else ok=false; fi
  peers="${peers:+$peers,}{\"interface\":\"$name\",\"peer\":\"$peer\",\"ok\":$ok}"
done
printf '{"interfaces":[%s],"duplicates":[%s],"peers":[%s]}' "$ifaces" "$dups" "$peers" > /dev/termination-log
`

// hostScript pings each target from the host network namespace, which is the path
// a host process takes to an SPDK pod on its own worker. TARGETS is a
// comma-separated list of addresses.
const hostScript = `set -u
targets=""
IFS=,
for address in $TARGETS; do
  if ping -c 2 -W 2 "$address" >/dev/null 2>&1; then ok=true; else ok=false; fi
  targets="${targets:+$targets,}{\"address\":\"$address\",\"ok\":$ok}"
done
printf '{"targets":[%s]}' "$targets" > /dev/termination-log
`
