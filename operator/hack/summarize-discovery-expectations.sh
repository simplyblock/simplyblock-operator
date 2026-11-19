#!/usr/bin/env bash
#
# Summarizes how a branch changes the recorded discovery expectations, as
# Markdown for a pull request comment or a CI job summary.
#
# A change to one default rewrites the same line in hundreds of expected files
# under internal/controllers/deployment/testdata/discovery (#805 changed 396 of
# them), which no reviewer reads file by file and which GitHub stops rendering
# past 300 files. Grouping identical added and removed lines turns that into a
# handful of rows that say what changed and in how many places. Lines that occur
# once stay visible as rows of their own, so a case-specific change is not
# folded into the bulk.
#
# Usage: hack/summarize-discovery-expectations.sh <base-ref> [<head-ref>]
# Prints nothing when the branch changes no expectation.

set -euo pipefail

if (($# < 1 || $# > 2)); then
	echo "usage: $0 <base-ref> [<head-ref>]" >&2
	exit 2
fi

base="$1"
head="${2:-HEAD}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CASES="$(cd "$SCRIPT_DIR/.." && pwd)/internal/controllers/deployment/testdata/discovery"

# The rows shown before the rest is counted, and the width a line is cut to.
# A summary longer than this is no longer one a reviewer reads.
MAX_ROWS=40
MAX_WIDTH=200

# Read line by line because the bash 3.2 that macOS ships has no mapfile.
files=()
while IFS= read -r file; do
	files+=("$file")
done < <(git diff --name-only --relative "$base...$head" -- "$CASES" | grep -E '/expected[^/]*$' || true)
if ((${#files[@]} == 0)); then
	exit 0
fi

file_count="${#files[@]}"
case_count="$(printf '%s\n' "${files[@]}" | xargs -n1 dirname | sort -u | wc -l | tr -d ' ')"

# Every added or removed line, without the +++/--- headers, grouped by content.
rows="$(git diff --no-color --unified=0 --relative "$base...$head" -- "${files[@]}" |
	grep -E '^[+-]' | grep -vE '^(\+\+\+|---) ' |
	sort | uniq -c | sort -rn -k1,1 -s)"
row_count="$(printf '%s\n' "$rows" | wc -l | tr -d ' ')"

echo "### Discovery expectations"
echo
echo "This branch changes ${file_count} expected files across ${case_count} cases. Identical lines are grouped, most frequent first."
echo
echo '```diff'
printf '%s\n' "$rows" | head -n "$MAX_ROWS" | awk -v width="$MAX_WIDTH" '{
	count = $1
	sub(/^[ \t]*[0-9]+ /, "")
	sign = substr($0, 1, 1)
	line = substr($0, 2)
	if (length(line) > width) line = substr(line, 1, width) "..."
	printf "%s %5d x %s\n", sign, count, line
}'
echo '```'
if ((row_count > MAX_ROWS)); then
	echo
	echo "$((row_count - MAX_ROWS)) more distinct lines are not shown. Run \`operator/hack/summarize-discovery-expectations.sh ${base}\` locally for the rest."
fi
