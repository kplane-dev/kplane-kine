#!/usr/bin/env bash
# Generate a self-signed CA + apiserver serving cert + ServiceAccount
# key pair into a target directory. Idempotent — re-running is a no-op
# once .complete exists.
#
# Usage:
#   gen-certs.sh <out-dir> <hostname-or-ip> [<hostname-or-ip> ...]
#
# Outputs:
#   ca.crt / ca.key       — self-signed CA
#   tls.crt / tls.key     — apiserver serving cert (signed by CA, with SANs)
#   sa.key  / sa.pub      — RSA pair for ServiceAccount token signing
#   token.csv             — static "admin" token, written to disk for
#                           the apiserver's --token-auth-file flag
#
# These certs are for the benchmark cluster only. They live as long
# as the kind cluster does (laptop) or the CI job (GH Actions). DO NOT
# copy this pattern into anything you care about — there's no rotation,
# no revocation, no proper SANs validation.

# shellcheck source=scripts/lib.sh
source "$(dirname "$0")/lib.sh"
check_tool openssl

out="${1:-}"; shift || die "usage: $0 <out-dir> <hostname-or-ip> [...]"
[ -n "$out" ] || die "missing out-dir"
[ $# -gt 0 ] || die "must provide at least one hostname/IP for SANs"

if [ -f "$out/.complete" ]; then
  log "certs at $out already generated — skipping"
  exit 0
fi

mkdir -p "$out"

# Build the subjectAltName string from positional args. IPv4 → IP:..., else → DNS:...
sans=""
for h in "$@"; do
  if [[ "$h" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    sans+="IP:$h,"
  else
    sans+="DNS:$h,"
  fi
done
sans=${sans%,}

cat >"$out/openssl.cnf" <<EOF
[req]
distinguished_name = dn
req_extensions     = ext
prompt             = no
[dn]
CN = kplane-apiserver
[ext]
subjectAltName = $sans
EOF

log "generating CA"
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout "$out/ca.key" -out "$out/ca.crt" \
  -subj "/CN=kplane-kine-bench-ca" 2>/dev/null

log "generating apiserver serving cert (SANs: $sans)"
openssl req -newkey rsa:2048 -nodes \
  -keyout "$out/tls.key" -out "$out/tls.csr" \
  -config "$out/openssl.cnf" 2>/dev/null
openssl x509 -req -days 365 \
  -in "$out/tls.csr" \
  -CA "$out/ca.crt" -CAkey "$out/ca.key" -CAcreateserial \
  -out "$out/tls.crt" \
  -extfile "$out/openssl.cnf" -extensions ext 2>/dev/null

log "generating ServiceAccount key pair"
openssl genrsa -out "$out/sa.key" 2048 2>/dev/null
openssl rsa -in "$out/sa.key" -pubout -out "$out/sa.pub" 2>/dev/null

log "generating static admin token"
token=$(openssl rand -hex 16)
echo "$token,admin,admin,\"system:masters\"" > "$out/token.csv"
echo "$token" > "$out/admin.token"

# Cleanup intermediate
rm -f "$out/tls.csr" "$out/openssl.cnf" "$out/ca.srl"

touch "$out/.complete"
log "certs ready at $out (CA, serving, SA pair, admin token)"
