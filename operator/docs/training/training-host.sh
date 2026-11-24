#!/usr/bin/env bash
#
# Sets up one host that serves the operator training (index.html, the chapters,
# and crd-quiz.html) together with the results API (results-api.py) the pages
# send finished tests to.
#
# It is written as EC2 user data for Amazon Linux 2023 and lives beside the
# pages because their sharing and statistics views only work with the API next
# to them: a copy served without it hides them. What it installs:
#
# - nginx serves the training directory at / and proxies /api/ to the API, with
#   a per-address request limit and no access log for that path, so nothing
#   about who sent a result is written down.
# - results-api.py runs as a systemd service and keeps one JSON file of
#   integers and booleans, the outcome of every test sent in, under
#   /var/lib/crd-quiz.
#
# Usage: pack the training directory, put the archive where the host can fetch
# it once at boot, substitute that URL, and pass the result as user data when
# launching the instance:
#
#   tar -C operator/docs -czf training.tar.gz training
#   SITE_URL=<url of training.tar.gz> \
#     perl -pe 's/__SITE_URL__/$ENV{SITE_URL}/' training-host.sh > user-data.sh
#
# A presigned S3 URL works and leaves no bucket behind once it has expired. The
# substitution goes through perl rather than sed because such a URL carries an
# ampersand, which sed's replacement reads as the matched text.

set -euo pipefail

SITE_URL="${SITE_URL:-__SITE_URL__}"

dnf install -y nginx python3

rm -rf /usr/share/nginx/html/*
curl -fsSL "$SITE_URL" | tar -xz -C /usr/share/nginx/html --strip-components=1
install -d /opt/training
install -m 0644 /usr/share/nginx/html/results-api.py /opt/training/results-api.py
# The scripts are not pages, and the pycache is a leftover of a local run.
rm -rf /usr/share/nginx/html/results-api.py /usr/share/nginx/html/training-host.sh \
  /usr/share/nginx/html/check-chapters.js /usr/share/nginx/html/__pycache__

cat > /etc/systemd/system/training-api.service <<'UNIT'
[Unit]
Description=Results API of the operator training
After=network.target

[Service]
ExecStart=/usr/bin/python3 /opt/training/results-api.py
DynamicUser=yes
StateDirectory=crd-quiz
Restart=always

[Install]
WantedBy=multi-user.target
UNIT

# The limit zone has to be declared outside the server block, the location
# inside the default one, which is why there are two files. The index page and
# every chapter page fetch the statistics once, so the limit leaves room for
# reading a few chapters in a row.
cat > /etc/nginx/conf.d/training-zone.conf <<'NGINX'
limit_req_zone $binary_remote_addr zone=training:1m rate=30r/m;
NGINX

cat > /etc/nginx/default.d/training.conf <<'NGINX'
location /api/ {
    limit_req zone=training burst=20 nodelay;
    access_log off;
    proxy_pass http://127.0.0.1:8080;
}
NGINX

systemctl daemon-reload
systemctl enable --now training-api
systemctl enable --now nginx
