#!/bin/sh
# Generates, into the directory given as the first argument, what the
# integration RustFS needs: ca.crt, rustfs_cert.pem and rustfs_key.pem (a
# throwaway CA and a server certificate for rustfs, localhost and 127.0.0.1,
# in the names RustFS loads from RUSTFS_TLS_PATH) and the random admin
# credentials admin-access-key and admin-secret-key (RUSTFS_ACCESS_KEY_FILE
# and RUSTFS_SECRET_KEY_FILE). POSIX sh and openssl only, so it runs on a CI
# runner and in the alpine/openssl image of the devcontainer stack alike.
# Everything is valid for two days and meant to be thrown away.
set -eu

dir="${1:?usage: gen-certs.sh <directory>}"
mkdir -p "$dir"
cd "$dir"
umask 077

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 \
    -keyout ca.key -out ca.crt -subj "/CN=operator-s3 integration CA" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout rustfs_key.pem -out server.csr -subj "/CN=rustfs"
cat > server.ext <<'EXT'
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=DNS:rustfs,DNS:localhost,IP:127.0.0.1
EXT
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
    -days 2 -extfile server.ext -out rustfs_cert.pem
# 16 and 32 hex characters: within RustFS's access key (3-128) and secret key
# (8-40) lengths.
openssl rand -hex 8 > admin-access-key
openssl rand -hex 16 > admin-secret-key
rm -f ca.key ca.srl server.csr server.ext

# RustFS runs as uid 10001, the test process as another user, so everything
# is readable by all, the key too. That is acceptable for a key that lives
# two days in a directory made for one run (a CI temp directory or a compose
# volume nothing else mounts).
chmod 755 .
chmod 644 ca.crt rustfs_cert.pem rustfs_key.pem admin-access-key admin-secret-key
