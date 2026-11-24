#!/usr/bin/env bash
#
# Sets up one host that serves the operator training (index.html, the chapters,
# and crd-quiz.html) together with the results API (results-api.py) the pages
# record progress and finished tests with.
#
# It is written as EC2 user data for Amazon Linux 2023 and lives beside the
# pages because their learner and statistics views only work with the API next
# to them: a copy served without it records nothing. What it installs:
#
# - nginx serves the training directory at / and proxies /api/ to the API, with
#   a per-address request limit and no access log for that path, so nothing
#   beyond the records themselves is written down.
# - results-api.py runs as a systemd service and keeps one JSON file under
#   /var/lib/crd-quiz: the learners' progress, time, and test attempts by name,
#   and the quiz's anonymous point totals.
# - admin.html shows every learner's progress to whoever has the admin token,
#   which is chosen at launch and reaches the API as an environment variable.
#
# Usage: pack the training directory, put the archive where the host can fetch
# it once at boot, choose an admin token, substitute both, and pass the result
# as user data when launching the instance:
#
#   tar -C operator/docs -czf training.tar.gz training
#   SITE_URL=<url of training.tar.gz> ADMIN_TOKEN=<a long random string> \
#     perl -pe 's/__SITE_URL__/$ENV{SITE_URL}/; s/__ADMIN_TOKEN__/$ENV{ADMIN_TOKEN}/' \
#     training-host.sh > user-data.sh
#
# A presigned S3 URL works and leaves no bucket behind once it has expired. The
# substitution goes through perl rather than sed because such a URL carries an
# ampersand, which sed's replacement reads as the matched text.

set -euo pipefail

SITE_URL="${SITE_URL:-__SITE_URL__}"
ADMIN_TOKEN="${ADMIN_TOKEN:-__ADMIN_TOKEN__}"

dnf install -y nginx python3

rm -rf /usr/share/nginx/html/*
curl -fsSL "$SITE_URL" | tar -xz -C /usr/share/nginx/html --strip-components=1
install -d /opt/training
install -m 0644 /usr/share/nginx/html/results-api.py /opt/training/results-api.py
# The scripts are not pages, and the pycache is a leftover of a local run.
rm -rf /usr/share/nginx/html/results-api.py /usr/share/nginx/html/training-host.sh \
  /usr/share/nginx/html/check-chapters.js /usr/share/nginx/html/__pycache__

# The token is written to a root-only file the unit reads, so it is in neither
# the unit nor the process list.
install -m 0600 /dev/null /opt/training/api.env
echo "ADMIN_TOKEN=$ADMIN_TOKEN" > /opt/training/api.env

cat > /etc/systemd/system/training-api.service <<'UNIT'
[Unit]
Description=Results API of the operator training
After=network.target

[Service]
ExecStart=/usr/bin/python3 /opt/training/results-api.py
EnvironmentFile=/opt/training/api.env
DynamicUser=yes
StateDirectory=crd-quiz
Restart=always

[Install]
WantedBy=multi-user.target
UNIT

# The limit zone has to be declared outside the server block, the location
# inside the default one, which is why there are two files. A chapter page
# sends the time spent every half minute besides its statistics and progress
# reads, so the limit leaves room for that and for a few pages in a row.
cat > /etc/nginx/conf.d/training-zone.conf <<'NGINX'
limit_req_zone $binary_remote_addr zone=training:1m rate=60r/m;
NGINX

cat > /etc/nginx/default.d/training.conf <<'NGINX'
location /api/ {
    limit_req zone=training burst=30 nodelay;
    access_log off;
    proxy_pass http://127.0.0.1:8080;
}
NGINX

systemctl daemon-reload
systemctl enable --now training-api
systemctl enable --now nginx
