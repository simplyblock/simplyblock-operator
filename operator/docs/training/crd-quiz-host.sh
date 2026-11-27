#!/usr/bin/env bash
#
# Sets up one host that serves the operator CRD quiz (crd-quiz.html) together
# with the results API (crd-quiz-api.py) the page sends finished quizzes to.
#
# It is written as EC2 user data for Amazon Linux 2023 and lives beside the
# quiz because the page's sharing and statistics views only work with the API
# next to it: a copy opened from disk hides them. What it installs:
#
# - nginx serves the page at / and proxies /api/results to the API, with a
#   per-address request limit and no access log for that path, so nothing
#   about who sent a result is written down.
# - crd-quiz-api.py runs as a systemd service and keeps one list of integers,
#   the point total of every quiz sent in, under /var/lib/crd-quiz.
#
# Usage: substitute the two URLs the host fetches the page and the API from
# once, at boot, then pass the result as user data when launching the instance:
#
#   PAGE_URL=<url of crd-quiz.html> API_URL=<url of crd-quiz-api.py> \
#     perl -pe 's/__PAGE_URL__/$ENV{PAGE_URL}/; s/__API_URL__/$ENV{API_URL}/' \
#     crd-quiz-host.sh > user-data.sh
#
# Presigned S3 URLs work and leave no bucket behind once they have expired. The
# substitution goes through perl rather than sed because such a URL carries an
# ampersand, which sed's replacement reads as the matched text.

set -euo pipefail

PAGE_URL="${PAGE_URL:-__PAGE_URL__}"
API_URL="${API_URL:-__API_URL__}"

dnf install -y nginx python3

install -d /opt/crd-quiz
curl -fsSL -o /opt/crd-quiz/api.py "$API_URL"
curl -fsSL -o /usr/share/nginx/html/index.html "$PAGE_URL"

cat > /etc/systemd/system/crd-quiz-api.service <<'UNIT'
[Unit]
Description=Results API of the operator CRD quiz
After=network.target

[Service]
ExecStart=/usr/bin/python3 /opt/crd-quiz/api.py
DynamicUser=yes
StateDirectory=crd-quiz
Restart=always

[Install]
WantedBy=multi-user.target
UNIT

# The limit zone has to be declared outside the server block, the location
# inside the default one, which is why there are two files.
cat > /etc/nginx/conf.d/crd-quiz-zone.conf <<'NGINX'
limit_req_zone $binary_remote_addr zone=crdquiz:1m rate=10r/m;
NGINX

cat > /etc/nginx/default.d/crd-quiz.conf <<'NGINX'
location = /api/results {
    limit_req zone=crdquiz burst=10 nodelay;
    access_log off;
    proxy_pass http://127.0.0.1:8080;
}
NGINX

systemctl daemon-reload
systemctl enable --now crd-quiz-api
systemctl enable --now nginx
