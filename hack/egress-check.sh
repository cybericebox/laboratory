#!/usr/bin/env bash
# Checks the internet egress of one lab device on a running cluster (docs/specs/LAB-INTERNET-EGRESS.md): surfing works, everything else is
# refused, the lab's own /etc/hosts and DNS change nothing, upload is held to ~2 Mbit/s while download is not.
#
#   hack/egress-check.sh <namespace> <pod> [container]
#
# The device needs curl, dig, nc and traceroute (an image like nicolaka/netshoot). It runs commands with kubectl exec in the pod of a device
# of a lab that has internet.enabled; it changes nothing but a line of the device's /etc/hosts, which it removes. Exit 0 = every check passed.
set -uo pipefail
ns=${1:?namespace of the lab group}; pod=${2:?device pod}; ctr=${3:-}
x() { if [[ -n "$ctr" ]]; then kubectl -n "$ns" exec "$pod" -c "$ctr" -- "$@"; else kubectl -n "$ns" exec "$pod" -- "$@"; fi; }
fails=0
ok()   { echo "ok    $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }
check() { local name=$1; shift; if "$@" >/dev/null 2>&1; then ok "$name"; else fail "$name"; fi; }
refused() { local name=$1; shift; if "$@" >/dev/null 2>&1; then fail "$name (it worked)"; else ok "$name"; fi; }

echo "== surfing"
check "HTTPS to a site" x curl -sS -m 20 -o /dev/null -f https://example.com/
check "HTTP to a site" x curl -sS -m 20 -o /dev/null -f http://example.com/
check "a package mirror" x curl -sS -m 20 -o /dev/null -f https://pypi.org/simple/pip/
check "DNS A answered" bash -c "[[ -n \"\$(kubectl -n $ns exec $pod ${ctr:+-c $ctr} -- dig +short A example.com)\" ]]"

echo "== refused"
refused "SSH (tcp 22)" x nc -z -w 4 github.com 22
refused "a UDP datagram (QUIC port)" x bash -c 'echo x | nc -u -w 3 1.1.1.1 443 | grep -q .'
refused "SMTP (tcp 25)" x nc -z -w 4 smtp.gmail.com 25
refused "a bare IP over HTTPS" x curl -sSk -m 10 -o /dev/null -f https://93.184.216.34/
refused "a tunnel service" x curl -sS -m 10 -o /dev/null -f https://ngrok.io/

echo "== the lab's own naming changes nothing"
x sh -c 'echo "1.2.3.4 example.com" >> /etc/hosts' 2>/dev/null
check "a /etc/hosts entry is ignored (the proxy resolves the name)" x curl -sS -m 20 -o /dev/null -f https://example.com/
x sh -c "grep -v '^1.2.3.4 example.com\$' /etc/hosts > /tmp/hosts.new && cat /tmp/hosts.new > /etc/hosts" 2>/dev/null
check "a DNS server of the lab's own choice is answered by the gateway" bash -c "[[ -n \"\$(kubectl -n $ns exec $pod ${ctr:+-c $ctr} -- dig +short @9.9.9.9 A example.com)\" ]]"
check "the HTTPS record is empty (no ECH keys)" bash -c "[[ -z \"\$(kubectl -n $ns exec $pod ${ctr:+-c $ctr} -- dig +short @1.1.1.1 HTTPS cloudflare.com)\" ]]"

echo "== traceroute and ping"
tr=$(x traceroute -I -n -w 2 -q 1 -m 8 1.1.1.1 2>&1 || true)
if grep -Eq '(^| )(10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.)' <<<"$(tail -n +2 <<<"$tr" | awk 'NR>1')"; then fail "an internal hop shows in traceroute:"$'\n'"$tr"; else ok "no internal hop beyond the gateway"; fi
got=$(x ping -c 30 -i 0.05 -W 1 1.1.1.1 2>&1 | awk '/received/ {print $4}')
if [[ -n "$got" && "$got" -le 15 ]]; then ok "ping is limited ($got of 30 answered)"; else fail "ping is not limited (${got:-?} of 30 answered)"; fi

echo "== speed (upload ~2 Mbit/s, download far more)"
down=$(x curl -sS -m 60 -o /dev/null -w '%{speed_download}' 'https://speed.cloudflare.com/__down?bytes=25000000' 2>/dev/null || echo 0)
up=$(x sh -c 'head -c 3000000 /dev/zero | curl -sS -m 90 -o /dev/null -w "%{speed_upload}" --data-binary @- https://speed.cloudflare.com/__up' 2>/dev/null || echo 0)
echo "download ${down%.*} B/s, upload ${up%.*} B/s"
if (( ${up%.*} > 0 && ${up%.*} < 450000 )); then ok "upload is held (~2 Mbit/s = 250 kB/s)"; else fail "upload is not held to ~250 kB/s"; fi
if (( ${down%.*} > 1500000 )); then ok "download is not (over 12 Mbit/s)"; else fail "download is slow"; fi

echo
(( fails == 0 )) && echo "all checks passed" || echo "$fails check(s) failed"
exit $(( fails > 0 ))
