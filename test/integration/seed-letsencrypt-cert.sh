#!/bin/sh
# Registers a self-signed certificate as a "letsencrypt" certificate on an NPM
# container, laid out like certbot's live/archive directories, so that the
# certificate download endpoint serves it. Real Let's Encrypt certificates
# cannot be issued in a local test.
#
# Usage: seed-letsencrypt-cert.sh <container> <days valid> <version N> <domain> [domain...]
# Prints the certificate ID. Running again with an existing domain set and a
# higher version N simulates a renewal of that certificate.
set -eu
container=$1 days=$2 version=$3
shift 3
domains="$*"

docker exec -i -e DOMAINS="$domains" -e DAYS="$days" -e VERSION="$version" -w /app "$container" sh -s <<'EOF'
set -eu
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
first=${DOMAINS%% *}
san=""
for d in $DOMAINS; do san="${san:+$san,}DNS:$d"; done

openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=npm-sync test CA" \
  -keyout "$work/ca.key" -out "$work/chain.pem" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=$first" -keyout "$work/privkey.pem" -out "$work/leaf.csr" 2>/dev/null
printf 'subjectAltName=%s\n' "$san" > "$work/ext"
openssl x509 -req -in "$work/leaf.csr" -CA "$work/chain.pem" -CAkey "$work/ca.key" -CAcreateserial \
  -days "$DAYS" -extfile "$work/ext" -out "$work/cert.pem" 2>/dev/null
cat "$work/cert.pem" "$work/chain.pem" > "$work/fullchain.pem"
expires=$(date -u -d "@$(date -u -d "$(openssl x509 -in "$work/cert.pem" -noout -enddate | cut -d= -f2)" +%s)" '+%Y-%m-%d %H:%M:%S')

id=$(DOMAINS="$DOMAINS" EXPIRES="$expires" node --input-type=module -e '
import knex from "knex";
const db = knex({ client: "sqlite3", connection: { filename: "/data/database.sqlite" }, useNullAsDefault: true });
const domains = process.env.DOMAINS.split(" ").sort();
const now = new Date().toISOString().slice(0, 19).replace("T", " ");
const existing = (await db("certificate").where({ provider: "letsencrypt", is_deleted: 0 }))
  .find((c) => JSON.stringify(JSON.parse(c.domain_names)) === JSON.stringify(domains));
let id;
if (existing) {
  id = existing.id;
  await db("certificate").where({ id }).update({ expires_on: process.env.EXPIRES, modified_on: now });
} else {
  [id] = await db("certificate").insert({
    created_on: now, modified_on: now, owner_user_id: 1, is_deleted: 0, provider: "letsencrypt",
    nice_name: domains.join(", "), domain_names: JSON.stringify(domains), expires_on: process.env.EXPIRES,
    meta: JSON.stringify({ letsencrypt_agree: true, dns_challenge: false }),
  });
}
console.log(id);
await db.destroy();
')

archive=/etc/letsencrypt/archive/npm-$id
live=/etc/letsencrypt/live/npm-$id
mkdir -p "$archive" "$live"
for f in cert chain fullchain privkey; do
  cp "$work/$f.pem" "$archive/$f$VERSION.pem"
  ln -sfn "../../archive/npm-$id/$f$VERSION.pem" "$live/$f.pem"
done
echo "$id"
EOF
